package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/project"
)

const (
	nightlyRuleArn = "arn:aws:events:us-east-1:123:rule/x-nightly"
	nightlyFnArn   = "arn:aws:lambda:us-east-1:123:function:x-pool:live"
)

func cronDeploy() *project.CompleteEvent {
	return &project.CompleteEvent{Finished: true, Resources: []apitype.ResourceV3{
		{Type: eventRuleResourceType, Outputs: map[string]interface{}{"name": "x-nightly", "arn": nightlyRuleArn, "scheduleExpression": "cron(0 3 * * ? *)", "state": "ENABLED"}},
		{Type: eventTargetResourceType, Outputs: map[string]interface{}{"rule": "x-nightly", "targetId": "Target0", "arn": nightlyFnArn, "input": `{"job":"nightly"}`}},
		{Type: eventRuleResourceType, Outputs: map[string]interface{}{"name": "x-fanout", "arn": "arn:aws:events:us-east-1:123:rule/x-fanout", "scheduleExpression": "rate(5 minutes)", "state": "DISABLED"}},
		{Type: eventTargetResourceType, Outputs: map[string]interface{}{"rule": "x-fanout", "targetId": "a", "arn": "arn:aws:lambda:us-east-1:123:function:x-a", "input": `{"job":"a"}`}},
		{Type: eventTargetResourceType, Outputs: map[string]interface{}{"rule": "x-fanout", "targetId": "b", "arn": "arn:aws:lambda:us-east-1:123:function:x-b"}},
		{Type: eventTargetResourceType, Outputs: map[string]interface{}{"rule": "x-fanout", "targetId": "task", "arn": "arn:aws:ecs:us-east-1:123:cluster/x"}},
		{Type: eventRuleResourceType, Outputs: map[string]interface{}{"name": "x-on-upload", "arn": "arn:aws:events:us-east-1:123:rule/x-on-upload", "eventPattern": `{"source":["aws.s3"]}`}},
		{Type: eventTargetResourceType, Outputs: map[string]interface{}{"rule": "x-on-upload", "targetId": "t", "arn": "arn:aws:lambda:us-east-1:123:function:x-upload"}},
		{Type: scheduleResourceType, Outputs: map[string]interface{}{
			"name": "x-hourly", "arn": "arn:aws:scheduler:us-east-1:123:schedule/default/x-hourly", "scheduleExpression": "rate(1 hour)", "state": "ENABLED",
			"target": map[string]interface{}{"arn": "arn:aws:lambda:us-east-1:123:function:x-hourly", "input": `{"hour":true}`},
		}},
	}}
}

func TestCronsFromCompleteListsScheduledLambdaTargets(t *testing.T) {
	crons := CronsFromComplete(cronDeploy())
	names := []string{}
	for _, cron := range crons {
		names = append(names, cron.Name)
	}
	want := []string{"x-fanout → a", "x-fanout → b", "x-hourly", "x-nightly"}
	if len(names) != len(want) {
		t.Fatalf("event-pattern rules and non-Lambda targets are not crons, got %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
	a, hourly, nightly := crons[0], crons[2], crons[3]
	if a.Enabled || a.Key != "rule/x-fanout/a" {
		t.Fatalf("unexpected %+v", a)
	}
	if !hourly.Enabled || hourly.Key != "schedule/default/x-hourly" {
		t.Fatalf("unexpected %+v", hourly)
	}
	if nightly.FunctionName() != "x-pool:live" || nightly.Region() != "us-east-1" || nightly.Schedule != "cron(0 3 * * ? *)" {
		t.Fatalf("unexpected %+v", nightly)
	}
}

func TestCronRunnerInvokesTheTargetAndPublishesTheOutcome(t *testing.T) {
	evts := bus.Subscribe(&CronRunStartedEvent{}, &CronRunEvent{})
	var invoked CronResource
	var sent []byte
	runner := newCronRunner(func(ctx context.Context, cron CronResource, payload []byte) (string, string, []byte, error) {
		invoked, sent = cron, payload
		return "req-1", "Unhandled", []byte(`{"errorMessage":"boom"}`), nil
	})
	if err := runner.Run(context.Background(), "rule/x-nightly/Target0"); err == nil {
		t.Fatal("a cron that was not deployed cannot run")
	}
	runner.deployed(CronsFromComplete(cronDeploy()))
	if err := runner.Run(context.Background(), "rule/x-nightly/Target0"); err != nil {
		t.Fatal(err)
	}
	if invoked.TargetArn != nightlyFnArn {
		t.Fatalf("invoked %+v", invoked)
	}
	started := (<-evts).(*CronRunStartedEvent)
	done := (<-evts).(*CronRunEvent)
	if string(sent) != "{}" || started.Payload != "{}" || started.RunID != done.RunID {
		t.Fatalf("a run sends the empty test event, even to a target with input: %s / %+v / %+v", sent, started, done)
	}
	if done.RequestID != "req-1" || done.FunctionError != "Unhandled" || done.Output != `{"errorMessage":"boom"}` || done.Error != "" {
		t.Fatalf("unexpected outcome %+v", done)
	}

	runner.invoke = func(ctx context.Context, cron CronResource, payload []byte) (string, string, []byte, error) {
		return "", "", nil, errors.New("AccessDenied")
	}
	runner.Run(context.Background(), "rule/x-nightly/Target0")
	<-evts
	if failed := (<-evts).(*CronRunEvent); failed.Error != "AccessDenied" {
		t.Fatalf("an invoke failure is reported, got %+v", failed)
	}
}
