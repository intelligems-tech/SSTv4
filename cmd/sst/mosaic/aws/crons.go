package aws

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/project"
	"github.com/sst/sst/v3/pkg/server"
)

// Pulumi types of a scheduled invocation: sst.aws.Cron makes an EventBridge
// rule and target, sst.aws.CronV2 an EventBridge Scheduler schedule.
const (
	eventRuleResourceType   = "aws:cloudwatch/eventRule:EventRule"
	eventTargetResourceType = "aws:cloudwatch/eventTarget:EventTarget"
	scheduleResourceType    = "aws:scheduler/schedule:Schedule"
)

// cronOutputLimit bounds the response a run keeps for the pane.
const cronOutputLimit = 4096

// CronResource is a schedule the app deployed that invokes a Lambda.
type CronResource struct {
	// Key identifies the cron to the run endpoint.
	Key      string
	Name     string
	Schedule string
	Enabled  bool
	// RuleArn is the rule or schedule.
	RuleArn   string
	TargetArn string
}

// Region is the region of the target function.
func (c CronResource) Region() string {
	return arnPart(c.TargetArn, 3)
}

// FunctionName is the target's function name and qualifier from its ARN.
func (c CronResource) FunctionName() string {
	if _, name, ok := strings.Cut(c.TargetArn, ":function:"); ok {
		return name
	}
	return c.TargetArn
}

// TestPayload is what a manual run sends: an empty event, as the Lambda
// console's Test button does, rather than the scheduled payload.
const TestPayload = "{}"

func arnPart(arn string, index int) string {
	parts := strings.Split(arn, ":")
	if len(parts) > index {
		return parts[index]
	}
	return ""
}

func isLambdaArn(arn string) bool {
	return strings.HasPrefix(arn, "arn:") && arnPart(arn, 2) == "lambda"
}

func enabledOutput(outputs map[string]interface{}) bool {
	if state, ok := outputs["state"].(string); ok && state != "" {
		return state == "ENABLED"
	}
	if enabled, ok := outputs["isEnabled"].(bool); ok {
		return enabled
	}
	return true
}

// CronsFromComplete lists the app's Lambda crons from a deploy, sorted by
// name. A rule with several Lambda targets lists one cron per target.
func CronsFromComplete(evt *project.CompleteEvent) []CronResource {
	type rule struct {
		arn, schedule string
		enabled       bool
	}
	rules := map[string]rule{}
	type target struct {
		rule, id, arn string
	}
	targets := []target{}
	crons := []CronResource{}
	for _, resource := range evt.Resources {
		outputs := resource.Outputs
		switch string(resource.Type) {
		case eventRuleResourceType:
			schedule := stringOutput(outputs, "scheduleExpression")
			if schedule == "" {
				continue
			}
			rules[stringOutput(outputs, "name")] = rule{arn: stringOutput(outputs, "arn"), schedule: schedule, enabled: enabledOutput(outputs)}
		case eventTargetResourceType:
			if arn := stringOutput(outputs, "arn"); isLambdaArn(arn) {
				targets = append(targets, target{rule: stringOutput(outputs, "rule"), id: stringOutput(outputs, "targetId"), arn: arn})
			}
		case scheduleResourceType:
			destination, _ := outputs["target"].(map[string]interface{})
			arn := stringOutput(destination, "arn")
			name := stringOutput(outputs, "name")
			if !isLambdaArn(arn) || name == "" {
				continue
			}
			group := stringOutput(outputs, "groupName")
			if group == "" {
				group = "default"
			}
			crons = append(crons, CronResource{
				Key:       "schedule/" + group + "/" + name,
				Name:      name,
				Schedule:  stringOutput(outputs, "scheduleExpression"),
				Enabled:   enabledOutput(outputs),
				RuleArn:   stringOutput(outputs, "arn"),
				TargetArn: arn,
			})
		}
	}
	perRule := map[string]int{}
	for _, t := range targets {
		perRule[t.rule]++
	}
	for _, t := range targets {
		r, ok := rules[t.rule]
		if !ok {
			continue
		}
		name := t.rule
		if perRule[t.rule] > 1 {
			name += " → " + t.id
		}
		crons = append(crons, CronResource{
			Key:       "rule/" + t.rule + "/" + t.id,
			Name:      name,
			Schedule:  r.schedule,
			Enabled:   r.enabled,
			RuleArn:   r.arn,
			TargetArn: t.arn,
		})
	}
	sort.Slice(crons, func(i, j int) bool { return crons[i].Name < crons[j].Name })
	return crons
}

// CronRunStartedEvent says a manual run of a cron began.
type CronRunStartedEvent struct {
	Key   string
	RunID string
	At    time.Time
	// Payload is the event sent to the function.
	Payload string
}

// CronRunEvent is how a manual run ended. Error is a failure to invoke;
// FunctionError is the function failing.
type CronRunEvent struct {
	Key           string
	RunID         string
	RequestID     string `json:",omitempty"`
	Duration      time.Duration
	FunctionError string `json:",omitempty"`
	Output        string `json:",omitempty"`
	Error         string `json:",omitempty"`
}

// cronInvoker invokes a function synchronously with a payload.
type cronInvoker func(ctx context.Context, cron CronResource, payload []byte) (requestID string, functionError string, output []byte, err error)

// lambdaInvoke invokes the deployed function, so the run takes the same
// path as the schedule: through the live stub to the local worker.
func lambdaInvoke(config aws.Config) cronInvoker {
	return func(ctx context.Context, cron CronResource, payload []byte) (string, string, []byte, error) {
		client := lambda.NewFromConfig(config, func(o *lambda.Options) {
			if region := cron.Region(); region != "" {
				o.Region = region
			}
		})
		out, err := client.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String(cron.TargetArn), Payload: payload})
		if err != nil {
			return "", "", nil, err
		}
		requestID, _ := awsmiddleware.GetRequestIDMetadata(out.ResultMetadata)
		return requestID, aws.ToString(out.FunctionError), out.Payload, nil
	}
}

// cronRunner remembers the deployed crons and runs them on request.
type cronRunner struct {
	mu     sync.Mutex
	crons  map[string]CronResource
	invoke cronInvoker
	now    func() time.Time
}

func newCronRunner(invoke cronInvoker) *cronRunner {
	return &cronRunner{crons: map[string]CronResource{}, invoke: invoke, now: time.Now}
}

func (r *cronRunner) deployed(crons []CronResource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.crons = map[string]CronResource{}
	for _, cron := range crons {
		r.crons[cron.Key] = cron
	}
}

// Run invokes a cron's function with TestPayload and publishes
// the start and the outcome. It returns once the function answers.
func (r *cronRunner) Run(ctx context.Context, key string) error {
	r.mu.Lock()
	cron, ok := r.crons[key]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no deployed cron %q", key)
	}
	runID := newRunID()
	started := r.now()
	payload := []byte(TestPayload)
	bus.Publish(&CronRunStartedEvent{Key: key, RunID: runID, At: started, Payload: string(payload)})
	requestID, functionError, output, err := r.invoke(ctx, cron, payload)
	evt := &CronRunEvent{Key: key, RunID: runID, RequestID: requestID, Duration: r.now().Sub(started), FunctionError: functionError}
	if len(output) > cronOutputLimit {
		output = append(output[:cronOutputLimit:cronOutputLimit], []byte("…")...)
	}
	evt.Output = string(output)
	if err != nil {
		evt.Error = err.Error()
	}
	slog.Info("cron run", "cron", key, "requestID", requestID, "functionError", functionError, "error", evt.Error)
	bus.Publish(evt)
	return nil
}

// newRunID is a random UUID that ties a run's start to its outcome.
func newRunID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type cronRunRequest struct {
	Key string `json:"key"`
}

// handleCronRun serves `POST /api/cron/run {key}` for the dev UI. It answers
// once the run starts; the outcome arrives as a CronRunEvent.
func handleCronRun(ctx context.Context, runner *cronRunner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req cronRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
			http.Error(w, "expected {\"key\": \"<cron key>\"}", http.StatusBadRequest)
			return
		}
		runner.mu.Lock()
		_, ok := runner.crons[req.Key]
		runner.mu.Unlock()
		if !ok {
			http.Error(w, "no deployed cron "+req.Key, http.StatusNotFound)
			return
		}
		go runner.Run(ctx, req.Key)
		w.WriteHeader(http.StatusAccepted)
	}
}

// watchCrons keeps the runner's crons current with each deploy.
func watchCrons(ctx context.Context, runner *cronRunner) {
	evts := bus.Subscribe(&project.CompleteEvent{})
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-evts:
			complete := evt.(*project.CompleteEvent)
			if complete.Old || !complete.Finished {
				continue
			}
			runner.deployed(CronsFromComplete(complete))
		}
	}
}

// startCrons wires cron discovery and the run endpoint into the dev server.
func startCrons(ctx context.Context, config aws.Config, s *server.Server) {
	runner := newCronRunner(lambdaInvoke(config))
	s.Mux.HandleFunc("/api/cron/run", handleCronRun(ctx, runner))
	go watchCrons(ctx, runner)
}
