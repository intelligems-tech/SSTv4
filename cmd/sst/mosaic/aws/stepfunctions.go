package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/project"
	"github.com/sst/sst/v3/pkg/server"
)

// stateMachineResourceType is the Pulumi type of a state machine, made by
// sst.aws.StepFunctions or directly.
const stateMachineResourceType = "aws:sfn/stateMachine:StateMachine"

const (
	// executionsPerMachine is how many recent executions the pane sees.
	executionsPerMachine = 25
	// historyPages bounds the history read for one execution.
	historyPages = 5
	// sfnPreviewLimit bounds each input and output sent to the pane; the
	// server keeps the full data for replays.
	sfnPreviewLimit = 4096
	// replayPoll is how often a replayed execution is checked.
	replayPoll = time.Second
)

// What happened to a state on one visit.
const (
	VisitRunning   = "running"
	VisitSucceeded = "succeeded"
	VisitFailed    = "failed"
	// VisitCaught is a state that failed into a Catch.
	VisitCaught = "caught"
)

// Replay modes: run one state through TestState, or start the whole
// execution again with its input.
const (
	ReplayState     = "state"
	ReplayExecution = "execution"
)

// StateMachineResource is a state machine the app deployed.
type StateMachineResource struct {
	Name       string
	Arn        string
	Type       string
	RoleArn    string
	Definition string
}

// Region is the state machine's region.
func (m StateMachineResource) Region() string {
	return arnPart(m.Arn, 3)
}

// StateMachinesFromComplete lists the app's state machines, by name.
func StateMachinesFromComplete(evt *project.CompleteEvent) []StateMachineResource {
	result := []StateMachineResource{}
	for _, resource := range evt.Resources {
		if string(resource.Type) != stateMachineResourceType {
			continue
		}
		outputs := resource.Outputs
		machine := StateMachineResource{
			Name:       stringOutput(outputs, "name"),
			Arn:        stringOutput(outputs, "arn"),
			Type:       stringOutput(outputs, "type"),
			RoleArn:    stringOutput(outputs, "roleArn"),
			Definition: stringOutput(outputs, "definition"),
		}
		if machine.Arn == "" {
			continue
		}
		if machine.Type == "" {
			machine.Type = "STANDARD"
		}
		result = append(result, machine)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// StepFunctionVisit is one entry into a state during an execution. ID is
// the history id of the entered event.
type StepFunctionVisit struct {
	ID      int64
	State   string
	Status  string
	Entered time.Time
	Exited  time.Time `json:",omitempty"`
	Input   string    `json:",omitempty"`
	Output  string    `json:",omitempty"`
	Error   string    `json:",omitempty"`
	Cause   string    `json:",omitempty"`
	// Retries counts failures the state retried past.
	Retries int `json:",omitempty"`
}

// StepFunctionExecution is a recent execution and the states it visited.
type StepFunctionExecution struct {
	Arn    string
	Name   string
	Status string
	Start  time.Time
	Stop   time.Time `json:",omitempty"`
	Input  string    `json:",omitempty"`
	Output string    `json:",omitempty"`
	Error  string    `json:",omitempty"`
	Cause  string    `json:",omitempty"`
	Visits []StepFunctionVisit
}

// Running reports whether the execution has not ended.
func (e *StepFunctionExecution) Running() bool {
	return e.Status == string(sfntypes.ExecutionStatusRunning) || e.Status == string(sfntypes.ExecutionStatusPendingRedrive)
}

// StepFunctionExecutionsEvent is a state machine's recent executions,
// newest first.
type StepFunctionExecutionsEvent struct {
	StateMachineArn string
	Executions      []StepFunctionExecution
	Error           string `json:",omitempty"`
}

// StepFunctionReplayStartedEvent says a replay began. Source is the
// execution the input came from; ExecutionArn is the new execution of an
// execution replay.
type StepFunctionReplayStartedEvent struct {
	ID              string
	StateMachineArn string
	Mode            string
	State           string `json:",omitempty"`
	Source          string
	ExecutionArn    string `json:",omitempty"`
	Input           string
	At              time.Time
}

// StepFunctionReplayEvent is how a replay ended. Status is a TestState
// status for a state replay and an execution status for an execution one.
type StepFunctionReplayEvent struct {
	ID              string
	StateMachineArn string
	Status          string
	Output          string `json:",omitempty"`
	Error           string `json:",omitempty"`
	Cause           string `json:",omitempty"`
	NextState       string `json:",omitempty"`
	ExecutionArn    string `json:",omitempty"`
	Duration        time.Duration
}

// stepFunctionsAPI is the part of the Step Functions client the pane uses.
type stepFunctionsAPI interface {
	ListExecutions(ctx context.Context, params *sfn.ListExecutionsInput, optFns ...func(*sfn.Options)) (*sfn.ListExecutionsOutput, error)
	GetExecutionHistory(ctx context.Context, params *sfn.GetExecutionHistoryInput, optFns ...func(*sfn.Options)) (*sfn.GetExecutionHistoryOutput, error)
	StartExecution(ctx context.Context, params *sfn.StartExecutionInput, optFns ...func(*sfn.Options)) (*sfn.StartExecutionOutput, error)
	DescribeExecution(ctx context.Context, params *sfn.DescribeExecutionInput, optFns ...func(*sfn.Options)) (*sfn.DescribeExecutionOutput, error)
	TestState(ctx context.Context, params *sfn.TestStateInput, optFns ...func(*sfn.Options)) (*sfn.TestStateOutput, error)
}

// stepFunctions reads executions and runs replays for the dev UI.
type stepFunctions struct {
	client func(region string) stepFunctionsAPI
	now    func() time.Time
	poll   time.Duration

	mu       sync.Mutex
	machines map[string]StateMachineResource
	// done caches ended executions with their full data, by ARN.
	done      map[string]*StepFunctionExecution
	inflight  map[string]bool
	published map[string][]byte
}

func newStepFunctions(client func(region string) stepFunctionsAPI) *stepFunctions {
	return &stepFunctions{
		client:    client,
		now:       time.Now,
		poll:      replayPoll,
		machines:  map[string]StateMachineResource{},
		done:      map[string]*StepFunctionExecution{},
		inflight:  map[string]bool{},
		published: map[string][]byte{},
	}
}

func regionalStepFunctions(config aws.Config) func(region string) stepFunctionsAPI {
	var mu sync.Mutex
	clients := map[string]*sfn.Client{}
	return func(region string) stepFunctionsAPI {
		mu.Lock()
		defer mu.Unlock()
		if client, ok := clients[region]; ok {
			return client
		}
		client := sfn.NewFromConfig(config, func(o *sfn.Options) {
			if region != "" {
				o.Region = region
			}
		})
		clients[region] = client
		return client
	}
}

func (s *stepFunctions) deployed(machines []StateMachineResource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.machines = map[string]StateMachineResource{}
	for _, machine := range machines {
		s.machines[machine.Arn] = machine
	}
}

func (s *stepFunctions) machine(arn string) (StateMachineResource, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	machine, ok := s.machines[arn]
	return machine, ok
}

// Refresh reads a machine's recent executions and publishes them when
// they changed. A refresh already under way for the machine wins.
func (s *stepFunctions) Refresh(ctx context.Context, arn string) {
	machine, ok := s.machine(arn)
	if !ok {
		return
	}
	s.mu.Lock()
	if s.inflight[arn] {
		s.mu.Unlock()
		return
	}
	s.inflight[arn] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, arn)
		s.mu.Unlock()
	}()

	evt := &StepFunctionExecutionsEvent{StateMachineArn: arn, Executions: []StepFunctionExecution{}}
	executions, err := s.executions(ctx, machine)
	if err != nil {
		evt.Error = err.Error()
	}
	for _, execution := range executions {
		evt.Executions = append(evt.Executions, preview(execution))
	}
	body, _ := json.Marshal(evt)
	s.mu.Lock()
	unchanged := bytes.Equal(s.published[arn], body)
	s.published[arn] = body
	s.mu.Unlock()
	if !unchanged {
		bus.Publish(evt)
	}
}

// executions lists a machine's recent executions with their histories.
// Ended ones are read once and cached.
func (s *stepFunctions) executions(ctx context.Context, machine StateMachineResource) ([]*StepFunctionExecution, error) {
	client := s.client(machine.Region())
	out, err := client.ListExecutions(ctx, &sfn.ListExecutionsInput{
		StateMachineArn: aws.String(machine.Arn),
		MaxResults:      executionsPerMachine,
	})
	if err != nil {
		return nil, err
	}
	result := make([]*StepFunctionExecution, len(out.Executions))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, item := range out.Executions {
		arn := aws.ToString(item.ExecutionArn)
		s.mu.Lock()
		cached := s.done[arn]
		s.mu.Unlock()
		if cached != nil {
			result[i] = cached
			continue
		}
		wg.Add(1)
		go func(i int, item sfntypes.ExecutionListItem) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			execution := &StepFunctionExecution{
				Arn:    arn,
				Name:   aws.ToString(item.Name),
				Status: string(item.Status),
				Start:  aws.ToTime(item.StartDate),
				Stop:   aws.ToTime(item.StopDate),
			}
			events, err := s.history(ctx, client, arn)
			if err != nil {
				execution.Error = "history: " + err.Error()
			} else {
				foldHistory(execution, events)
			}
			result[i] = execution
			if err == nil && !execution.Running() {
				s.mu.Lock()
				s.done[arn] = execution
				s.mu.Unlock()
			}
		}(i, item)
	}
	wg.Wait()
	s.prune(machine.Arn, result)
	return result, nil
}

// prune drops cached executions of a machine that left its recent list.
func (s *stepFunctions) prune(machineArn string, recent []*StepFunctionExecution) {
	keep := map[string]bool{}
	for _, execution := range recent {
		keep[execution.Arn] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for arn := range s.done {
		if !keep[arn] && executionOf(arn) == machineArn {
			delete(s.done, arn)
		}
	}
}

// executionOf is the state machine ARN of an execution ARN.
func executionOf(executionArn string) string {
	parts := bytes.Split([]byte(executionArn), []byte(":"))
	if len(parts) < 8 {
		return ""
	}
	parts[5] = []byte("stateMachine")
	return string(bytes.Join(parts[:7], []byte(":")))
}

func (s *stepFunctions) history(ctx context.Context, client stepFunctionsAPI, arn string) ([]sfntypes.HistoryEvent, error) {
	events := []sfntypes.HistoryEvent{}
	var token *string
	for page := 0; page < historyPages; page++ {
		out, err := client.GetExecutionHistory(ctx, &sfn.GetExecutionHistoryInput{
			ExecutionArn:         aws.String(arn),
			IncludeExecutionData: aws.Bool(true),
			MaxResults:           1000,
			NextToken:            token,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, out.Events...)
		token = out.NextToken
		if token == nil {
			break
		}
	}
	return events, nil
}

// foldHistory turns an execution's history into its input, output and the
// visit to each state. A failure belongs to the state whose entry it
// follows: a later attempt of that state makes it a retry, a state the
// execution moves on from after a failure was caught, and a failure the
// execution ends on failed the state.
func foldHistory(execution *StepFunctionExecution, events []sfntypes.HistoryEvent) {
	byID := map[int64]sfntypes.HistoryEvent{}
	visits := map[int64]*StepFunctionVisit{}
	failing := map[*StepFunctionVisit]bool{}
	// failures maps a failure event to the visit it failed.
	failures := map[int64]*StepFunctionVisit{}
	open := map[string][]*StepFunctionVisit{}
	order := []*StepFunctionVisit{}
	for _, e := range events {
		byID[e.Id] = e
		switch {
		case e.ExecutionStartedEventDetails != nil:
			execution.Input = aws.ToString(e.ExecutionStartedEventDetails.Input)
		case e.ExecutionSucceededEventDetails != nil:
			execution.Output = aws.ToString(e.ExecutionSucceededEventDetails.Output)
		case e.ExecutionFailedEventDetails != nil:
			execution.Error = aws.ToString(e.ExecutionFailedEventDetails.Error)
			execution.Cause = aws.ToString(e.ExecutionFailedEventDetails.Cause)
		case e.ExecutionTimedOutEventDetails != nil:
			execution.Error = aws.ToString(e.ExecutionTimedOutEventDetails.Error)
			execution.Cause = aws.ToString(e.ExecutionTimedOutEventDetails.Cause)
		case e.ExecutionAbortedEventDetails != nil:
			execution.Error = aws.ToString(e.ExecutionAbortedEventDetails.Error)
			execution.Cause = aws.ToString(e.ExecutionAbortedEventDetails.Cause)
		case e.StateEnteredEventDetails != nil:
			name := aws.ToString(e.StateEnteredEventDetails.Name)
			visit := &StepFunctionVisit{
				ID:      e.Id,
				State:   name,
				Status:  VisitRunning,
				Entered: aws.ToTime(e.Timestamp),
				Input:   aws.ToString(e.StateEnteredEventDetails.Input),
			}
			if caught := caughtBefore(e, byID, visits, failures); caught != nil && failing[caught] && caught.Status == VisitRunning {
				caught.Status = VisitCaught
			}
			visits[e.Id] = visit
			open[name] = append(open[name], visit)
			order = append(order, visit)
		case e.StateExitedEventDetails != nil:
			name := aws.ToString(e.StateExitedEventDetails.Name)
			if len(open[name]) == 0 {
				continue
			}
			visit := open[name][0]
			open[name] = open[name][1:]
			visit.Status, visit.Exited = VisitSucceeded, aws.ToTime(e.Timestamp)
			visit.Output = aws.ToString(e.StateExitedEventDetails.Output)
			if failing[visit] {
				visit.Status = VisitCaught
			}
		case e.TaskScheduledEventDetails != nil || e.LambdaFunctionScheduledEventDetails != nil || e.ActivityScheduledEventDetails != nil:
			if visit := enteredBefore(e, byID, visits); visit != nil && failing[visit] {
				failing[visit] = false
				visit.Retries++
			}
		default:
			message, cause, ok := failure(e)
			if !ok {
				continue
			}
			if visit := enteredBefore(e, byID, visits); visit != nil {
				visit.Error, visit.Cause = message, cause
				failing[visit] = true
				failures[e.Id] = visit
			}
		}
	}
	ended := !execution.Running()
	failed := execution.Status != string(sfntypes.ExecutionStatusSucceeded)
	for _, visit := range order {
		if visit.Status != VisitRunning {
			continue
		}
		switch {
		case !ended:
			// Running, or waiting out a retry interval.
		case failing[visit] && !failed:
			visit.Status = VisitCaught
		default:
			visit.Status = VisitFailed
			if visit.Error == "" {
				visit.Error, visit.Cause = execution.Error, execution.Cause
			}
		}
	}
	execution.Visits = make([]StepFunctionVisit, len(order))
	for i, visit := range order {
		execution.Visits[i] = *visit
	}
}

// enteredBefore follows an event's previous ids back to the state entry
// it belongs to.
func enteredBefore(e sfntypes.HistoryEvent, byID map[int64]sfntypes.HistoryEvent, visits map[int64]*StepFunctionVisit) *StepFunctionVisit {
	for id, hops := e.PreviousEventId, 0; id != 0 && hops < 1000; hops++ {
		if visit, ok := visits[id]; ok {
			return visit
		}
		previous, ok := byID[id]
		if !ok {
			return nil
		}
		id = previous.PreviousEventId
	}
	return nil
}

// caughtBefore follows a state entry back to the previous state's entry
// and returns the visit whose failure it passed, the one a Catch left.
func caughtBefore(e sfntypes.HistoryEvent, byID map[int64]sfntypes.HistoryEvent, visits map[int64]*StepFunctionVisit, failures map[int64]*StepFunctionVisit) *StepFunctionVisit {
	for id, hops := e.PreviousEventId, 0; id != 0 && hops < 1000; hops++ {
		if visit, ok := failures[id]; ok {
			return visit
		}
		if _, ok := visits[id]; ok {
			return nil
		}
		previous, ok := byID[id]
		if !ok {
			return nil
		}
		id = previous.PreviousEventId
	}
	return nil
}

// failure is the error and cause of an event that reports a state failing.
func failure(e sfntypes.HistoryEvent) (string, string, bool) {
	switch {
	case e.TaskFailedEventDetails != nil:
		return aws.ToString(e.TaskFailedEventDetails.Error), aws.ToString(e.TaskFailedEventDetails.Cause), true
	case e.TaskTimedOutEventDetails != nil:
		return aws.ToString(e.TaskTimedOutEventDetails.Error), aws.ToString(e.TaskTimedOutEventDetails.Cause), true
	case e.TaskStartFailedEventDetails != nil:
		return aws.ToString(e.TaskStartFailedEventDetails.Error), aws.ToString(e.TaskStartFailedEventDetails.Cause), true
	case e.TaskSubmitFailedEventDetails != nil:
		return aws.ToString(e.TaskSubmitFailedEventDetails.Error), aws.ToString(e.TaskSubmitFailedEventDetails.Cause), true
	case e.LambdaFunctionFailedEventDetails != nil:
		return aws.ToString(e.LambdaFunctionFailedEventDetails.Error), aws.ToString(e.LambdaFunctionFailedEventDetails.Cause), true
	case e.LambdaFunctionTimedOutEventDetails != nil:
		return aws.ToString(e.LambdaFunctionTimedOutEventDetails.Error), aws.ToString(e.LambdaFunctionTimedOutEventDetails.Cause), true
	case e.LambdaFunctionStartFailedEventDetails != nil:
		return aws.ToString(e.LambdaFunctionStartFailedEventDetails.Error), aws.ToString(e.LambdaFunctionStartFailedEventDetails.Cause), true
	case e.LambdaFunctionScheduleFailedEventDetails != nil:
		return aws.ToString(e.LambdaFunctionScheduleFailedEventDetails.Error), aws.ToString(e.LambdaFunctionScheduleFailedEventDetails.Cause), true
	case e.ActivityFailedEventDetails != nil:
		return aws.ToString(e.ActivityFailedEventDetails.Error), aws.ToString(e.ActivityFailedEventDetails.Cause), true
	case e.ActivityTimedOutEventDetails != nil:
		return aws.ToString(e.ActivityTimedOutEventDetails.Error), aws.ToString(e.ActivityTimedOutEventDetails.Cause), true
	case e.EvaluationFailedEventDetails != nil:
		return aws.ToString(e.EvaluationFailedEventDetails.Error), aws.ToString(e.EvaluationFailedEventDetails.Cause), true
	case e.MapRunFailedEventDetails != nil:
		return aws.ToString(e.MapRunFailedEventDetails.Error), aws.ToString(e.MapRunFailedEventDetails.Cause), true
	}
	return "", "", false
}

// preview is an execution with long payloads cut for the pane.
func preview(execution *StepFunctionExecution) StepFunctionExecution {
	result := *execution
	result.Input, result.Output = clip(result.Input), clip(result.Output)
	result.Cause = clip(result.Cause)
	result.Visits = make([]StepFunctionVisit, len(execution.Visits))
	for i, visit := range execution.Visits {
		visit.Input, visit.Output, visit.Cause = clip(visit.Input), clip(visit.Output), clip(visit.Cause)
		result.Visits[i] = visit
	}
	return result
}

func clip(text string) string {
	if len(text) <= sfnPreviewLimit {
		return text
	}
	return text[:sfnPreviewLimit] + "…"
}

// StepFunctionReplayRequest asks for a replay of one visit (Mode state) or
// of the execution it belongs to (Mode execution).
type StepFunctionReplayRequest struct {
	StateMachineArn string `json:"stateMachineArn"`
	ExecutionArn    string `json:"executionArn"`
	Mode            string `json:"mode"`
	Visit           int64  `json:"visit"`
}

// Replay runs a replay and publishes its start and outcome. It returns
// once the replay ends.
func (s *stepFunctions) Replay(ctx context.Context, req StepFunctionReplayRequest) error {
	machine, ok := s.machine(req.StateMachineArn)
	if !ok {
		return fmt.Errorf("no deployed state machine %q", req.StateMachineArn)
	}
	s.mu.Lock()
	execution := s.done[req.ExecutionArn]
	s.mu.Unlock()
	if execution == nil {
		client := s.client(machine.Region())
		described, err := client.DescribeExecution(ctx, &sfn.DescribeExecutionInput{ExecutionArn: aws.String(req.ExecutionArn)})
		if err != nil {
			return err
		}
		events, err := s.history(ctx, client, req.ExecutionArn)
		if err != nil {
			return err
		}
		execution = &StepFunctionExecution{Arn: req.ExecutionArn, Status: string(described.Status)}
		foldHistory(execution, events)
	}
	switch req.Mode {
	case ReplayExecution:
		s.replayExecution(ctx, machine, execution)
		return nil
	case ReplayState:
		for _, visit := range execution.Visits {
			if visit.ID == req.Visit {
				return s.replayState(ctx, machine, execution, visit)
			}
		}
		return fmt.Errorf("execution %s has no visit %d", req.ExecutionArn, req.Visit)
	}
	return fmt.Errorf("unknown replay mode %q", req.Mode)
}

func (s *stepFunctions) replayExecution(ctx context.Context, machine StateMachineResource, source *StepFunctionExecution) {
	id := newRunID()
	started := s.now()
	client := s.client(machine.Region())
	input := source.Input
	if input == "" {
		input = TestPayload
	}
	out, err := client.StartExecution(ctx, &sfn.StartExecutionInput{StateMachineArn: aws.String(machine.Arn), Input: aws.String(input)})
	evt := &StepFunctionReplayStartedEvent{ID: id, StateMachineArn: machine.Arn, Mode: ReplayExecution, Source: source.Arn, Input: clip(input), At: started}
	if err != nil {
		bus.Publish(evt)
		bus.Publish(&StepFunctionReplayEvent{ID: id, StateMachineArn: machine.Arn, Status: "FAILED", Error: err.Error(), Duration: s.now().Sub(started)})
		return
	}
	arn := aws.ToString(out.ExecutionArn)
	evt.ExecutionArn = arn
	bus.Publish(evt)
	go s.Refresh(ctx, machine.Arn)
	result := &StepFunctionReplayEvent{ID: id, StateMachineArn: machine.Arn, ExecutionArn: arn}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.poll):
		}
		described, err := client.DescribeExecution(ctx, &sfn.DescribeExecutionInput{ExecutionArn: aws.String(arn)})
		if err != nil {
			result.Status, result.Error = "FAILED", err.Error()
			break
		}
		if described.Status == sfntypes.ExecutionStatusRunning {
			continue
		}
		result.Status = string(described.Status)
		result.Output = clip(aws.ToString(described.Output))
		result.Error, result.Cause = aws.ToString(described.Error), clip(aws.ToString(described.Cause))
		break
	}
	result.Duration = s.now().Sub(started)
	slog.Info("step functions replay", "machine", machine.Name, "execution", arn, "status", result.Status)
	bus.Publish(result)
	s.Refresh(ctx, machine.Arn)
}

// replayState runs one state on its own with the input it got on a
// visit, through TestState, so a Lambda task reaches the live function.
func (s *stepFunctions) replayState(ctx context.Context, machine StateMachineResource, source *StepFunctionExecution, visit StepFunctionVisit) error {
	definition, err := ParseDefinition(machine.Definition)
	if err != nil {
		return fmt.Errorf("definition: %w", err)
	}
	state := definition.Find(visit.State)
	if state == nil {
		return fmt.Errorf("state %q is no longer in the definition", visit.State)
	}
	if !state.Testable() {
		return fmt.Errorf("TestState cannot run a %s state on its own", state.Type)
	}
	raw, err := standaloneState(machine.Definition, state.Raw)
	if err != nil {
		return err
	}
	id := newRunID()
	started := s.now()
	bus.Publish(&StepFunctionReplayStartedEvent{ID: id, StateMachineArn: machine.Arn, Mode: ReplayState, State: visit.State, Source: source.Arn, Input: clip(visit.Input), At: started})
	out, err := s.client(machine.Region()).TestState(ctx, &sfn.TestStateInput{
		Definition:      aws.String(raw),
		RoleArn:         aws.String(machine.RoleArn),
		Input:           aws.String(visit.Input),
		InspectionLevel: sfntypes.InspectionLevelInfo,
	})
	result := &StepFunctionReplayEvent{ID: id, StateMachineArn: machine.Arn, Duration: s.now().Sub(started)}
	if err != nil {
		result.Status, result.Error = "FAILED", err.Error()
	} else {
		result.Status = string(out.Status)
		result.Output = clip(aws.ToString(out.Output))
		result.Error, result.Cause = aws.ToString(out.Error), clip(aws.ToString(out.Cause))
		result.NextState = aws.ToString(out.NextState)
	}
	slog.Info("step functions state replay", "machine", machine.Name, "state", visit.State, "status", result.Status, "error", result.Error)
	bus.Publish(result)
	return nil
}

// standaloneState is a state's JSON as TestState takes it: a state of a
// JSONata machine carries the machine's QueryLanguage.
func standaloneState(definition string, state json.RawMessage) (string, error) {
	var top struct {
		QueryLanguage string `json:"QueryLanguage"`
	}
	json.Unmarshal([]byte(definition), &top)
	if top.QueryLanguage == "" {
		return string(state), nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(state, &fields); err != nil {
		return "", err
	}
	if _, ok := fields["QueryLanguage"]; !ok {
		fields["QueryLanguage"], _ = json.Marshal(top.QueryLanguage)
	}
	body, err := json.Marshal(fields)
	return string(body), err
}

// handleStepFunctionExecutions serves `POST /api/sfn/executions {arn}`:
// the executions arrive as a StepFunctionExecutionsEvent.
func handleStepFunctionExecutions(ctx context.Context, s *stepFunctions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Arn string `json:"arn"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Arn == "" {
			http.Error(w, "expected {\"arn\": \"<state machine arn>\"}", http.StatusBadRequest)
			return
		}
		if _, ok := s.machine(req.Arn); !ok {
			http.Error(w, "no deployed state machine "+req.Arn, http.StatusNotFound)
			return
		}
		// A changed list is published even when this pane saw the last one.
		s.mu.Lock()
		delete(s.published, req.Arn)
		s.mu.Unlock()
		go s.Refresh(ctx, req.Arn)
		w.WriteHeader(http.StatusAccepted)
	}
}

// handleStepFunctionReplay serves `POST /api/sfn/replay`. It answers once
// the replay is accepted; a replay that cannot start publishes a failed
// StepFunctionReplayEvent with the reason.
func handleStepFunctionReplay(ctx context.Context, s *stepFunctions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req StepFunctionReplayRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StateMachineArn == "" || req.ExecutionArn == "" {
			http.Error(w, "expected {\"stateMachineArn\", \"executionArn\", \"mode\", \"visit\"}", http.StatusBadRequest)
			return
		}
		if _, ok := s.machine(req.StateMachineArn); !ok {
			http.Error(w, "no deployed state machine "+req.StateMachineArn, http.StatusNotFound)
			return
		}
		go func() {
			if err := s.Replay(ctx, req); err != nil {
				slog.Error("step functions replay", "err", err)
				bus.Publish(&StepFunctionReplayEvent{ID: "", StateMachineArn: req.StateMachineArn, Status: "FAILED", Error: err.Error()})
			}
		}()
		w.WriteHeader(http.StatusAccepted)
	}
}

// watchStepFunctions keeps the deployed state machines current.
func watchStepFunctions(ctx context.Context, s *stepFunctions) {
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
			s.deployed(StateMachinesFromComplete(complete))
		}
	}
}

// startStepFunctions wires the Step Functions endpoints into the server.
func startStepFunctions(ctx context.Context, config aws.Config, srv *server.Server) {
	s := newStepFunctions(regionalStepFunctions(config))
	srv.Mux.HandleFunc("/api/sfn/executions", handleStepFunctionExecutions(ctx, s))
	srv.Mux.HandleFunc("/api/sfn/replay", handleStepFunctionReplay(ctx, s))
	go watchStepFunctions(ctx, s)
}
