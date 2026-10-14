package aws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	sfntypes "github.com/aws/aws-sdk-go-v2/service/sfn/types"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/project"
)

const (
	checkoutArn  = "arn:aws:states:us-east-1:123:stateMachine:dev-checkout"
	checkoutRole = "arn:aws:iam::123:role/dev-checkout"
	executionArn = "arn:aws:states:us-east-1:123:execution:dev-checkout:run-1"
)

const checkoutDefinition = `{"StartAt":"Charge","States":{
	"Charge":{"Type":"Task","Resource":"arn:aws:states:::lambda:invoke","Parameters":{"FunctionName":"arn:aws:lambda:us-east-1:123:function:dev-charge"},"Next":"Fan out","Catch":[{"ErrorEquals":["States.ALL"],"Next":"Give up"}]},
	"Fan out":{"Type":"Parallel","End":true,"Branches":[
		{"StartAt":"Email","States":{"Email":{"Type":"Task","Resource":"arn:aws:states:::sns:publish","End":true}}},
		{"StartAt":"Wait for ship","States":{"Wait for ship":{"Type":"Task","Resource":"arn:aws:states:::ecs:runTask.sync","End":true}}}]},
	"Give up":{"Type":"Fail","Error":"GaveUp"}}}`

func sfnDeploy(definition string) *project.CompleteEvent {
	return &project.CompleteEvent{Finished: true, Resources: []apitype.ResourceV3{
		{Type: stateMachineResourceType, Outputs: map[string]interface{}{
			"name": "dev-checkout", "arn": checkoutArn, "type": "STANDARD", "roleArn": checkoutRole, "definition": definition,
		}},
		{Type: "aws:lambda/function:Function", Outputs: map[string]interface{}{"name": "dev-charge", "arn": "arn:aws:lambda:us-east-1:123:function:dev-charge"}},
	}}
}

func TestStateMachinesFromCompleteListsStateMachines(t *testing.T) {
	machines := StateMachinesFromComplete(sfnDeploy(checkoutDefinition))
	if len(machines) != 1 {
		t.Fatalf("only state machines are listed, got %+v", machines)
	}
	m := machines[0]
	if m.Name != "dev-checkout" || m.RoleArn != checkoutRole || m.Region() != "us-east-1" || m.Definition != checkoutDefinition {
		t.Fatalf("unexpected %+v", m)
	}
}

func TestParseDefinitionKeepsOrderAndFindsNestedStates(t *testing.T) {
	m, err := ParseDefinition(checkoutDefinition)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.Order, ",") != "Charge,Fan out,Give up" {
		t.Fatalf("states keep the definition's order, got %v", m.Order)
	}
	charge := m.Find("Charge")
	if charge.Function != "arn:aws:lambda:us-east-1:123:function:dev-charge" || charge.Next != "Fan out" || charge.Catches[0] != "Give up" {
		t.Fatalf("unexpected %+v", charge)
	}
	if m.Find("Email") == nil || m.Count() != 5 {
		t.Fatalf("branch states are found and counted, count %d", m.Count())
	}
	if !charge.Testable() || m.Find("Fan out").Testable() || m.Find("Wait for ship").Testable() {
		t.Fatal("TestState runs a plain task, but not a Parallel or a .sync task")
	}
}

type historyBuilder struct {
	at     time.Time
	events []sfntypes.HistoryEvent
}

func (h *historyBuilder) add(previous int64, e sfntypes.HistoryEvent) int64 {
	e.Id = int64(len(h.events) + 1)
	e.PreviousEventId = previous
	at := h.at.Add(time.Duration(e.Id) * time.Second)
	e.Timestamp = &at
	h.events = append(h.events, e)
	return e.Id
}

func entered(name string, input string) sfntypes.HistoryEvent {
	return sfntypes.HistoryEvent{Type: sfntypes.HistoryEventTypeTaskStateEntered, StateEnteredEventDetails: &sfntypes.StateEnteredEventDetails{Name: aws.String(name), Input: aws.String(input)}}
}

func exited(name string, output string) sfntypes.HistoryEvent {
	return sfntypes.HistoryEvent{Type: sfntypes.HistoryEventTypeTaskStateExited, StateExitedEventDetails: &sfntypes.StateExitedEventDetails{Name: aws.String(name), Output: aws.String(output)}}
}

func scheduled() sfntypes.HistoryEvent {
	return sfntypes.HistoryEvent{Type: sfntypes.HistoryEventTypeTaskScheduled, TaskScheduledEventDetails: &sfntypes.TaskScheduledEventDetails{}}
}

func taskFailed(message string) sfntypes.HistoryEvent {
	return sfntypes.HistoryEvent{Type: sfntypes.HistoryEventTypeTaskFailed, TaskFailedEventDetails: &sfntypes.TaskFailedEventDetails{Error: aws.String(message), Cause: aws.String(message + " cause")}}
}

// checkoutHistory: Charge fails once and succeeds on retry, Email fails
// into a Catch, Notify runs, and the execution fails in Give up.
func checkoutHistory() []sfntypes.HistoryEvent {
	h := &historyBuilder{at: time.Unix(1_700_000_000, 0)}
	id := h.add(0, sfntypes.HistoryEvent{ExecutionStartedEventDetails: &sfntypes.ExecutionStartedEventDetails{Input: aws.String(`{"order":1}`)}})
	charge := h.add(id, entered("Charge", `{"order":1}`))
	id = h.add(charge, scheduled())
	id = h.add(id, taskFailed("Lambda.Timeout"))
	id = h.add(id, scheduled())
	id = h.add(id, sfntypes.HistoryEvent{TaskSucceededEventDetails: &sfntypes.TaskSucceededEventDetails{}})
	id = h.add(id, exited("Charge", `{"charged":true}`))
	email := h.add(id, entered("Email", `{"charged":true}`))
	id = h.add(email, scheduled())
	id = h.add(id, taskFailed("SNS.Throttled"))
	notify := h.add(id, entered("Notify", `{"Error":"SNS.Throttled"}`))
	id = h.add(notify, exited("Notify", `{}`))
	giveUp := h.add(id, entered("Give up", `{}`))
	h.add(giveUp, sfntypes.HistoryEvent{ExecutionFailedEventDetails: &sfntypes.ExecutionFailedEventDetails{Error: aws.String("GaveUp"), Cause: aws.String("no email")}})
	return h.events
}

func TestFoldHistorySortsOutRetriesCatchesAndFailures(t *testing.T) {
	execution := &StepFunctionExecution{Status: "FAILED"}
	foldHistory(execution, checkoutHistory())
	if execution.Input != `{"order":1}` || execution.Error != "GaveUp" {
		t.Fatalf("unexpected execution %+v", execution)
	}
	byState := map[string]StepFunctionVisit{}
	for _, visit := range execution.Visits {
		byState[visit.State] = visit
	}
	if charge := byState["Charge"]; charge.Status != VisitSucceeded || charge.Retries != 1 || charge.Output != `{"charged":true}` || charge.Exited.Sub(charge.Entered) != 5*time.Second {
		t.Fatalf("a retried state that exits succeeded: %+v", charge)
	}
	if email := byState["Email"]; email.Status != VisitCaught || email.Error != "SNS.Throttled" || email.Retries != 0 {
		t.Fatalf("a state the execution moved on from after a failure was caught: %+v", email)
	}
	if notify := byState["Notify"]; notify.Status != VisitSucceeded {
		t.Fatalf("unexpected %+v", notify)
	}
	if giveUp := byState["Give up"]; giveUp.Status != VisitFailed || giveUp.Error != "GaveUp" || giveUp.Cause != "no email" {
		t.Fatalf("the state the execution failed in takes its error: %+v", giveUp)
	}
}

func TestFoldHistoryLeavesARunningStateRunning(t *testing.T) {
	h := &historyBuilder{at: time.Unix(1_700_000_000, 0)}
	id := h.add(0, sfntypes.HistoryEvent{ExecutionStartedEventDetails: &sfntypes.ExecutionStartedEventDetails{Input: aws.String(`{}`)}})
	id = h.add(id, entered("Charge", `{}`))
	id = h.add(id, scheduled())
	h.add(id, taskFailed("Lambda.Timeout"))
	execution := &StepFunctionExecution{Status: "RUNNING"}
	foldHistory(execution, h.events)
	if visit := execution.Visits[0]; visit.Status != VisitRunning || visit.Error != "Lambda.Timeout" {
		t.Fatalf("a state waiting out a retry is still running: %+v", visit)
	}
}

type fakeSfn struct {
	history    []sfntypes.HistoryEvent
	histories  int
	started    *sfn.StartExecutionInput
	tested     *sfn.TestStateInput
	status     sfntypes.ExecutionStatus
	executions []sfntypes.ExecutionListItem
}

func (f *fakeSfn) ListExecutions(ctx context.Context, params *sfn.ListExecutionsInput, optFns ...func(*sfn.Options)) (*sfn.ListExecutionsOutput, error) {
	return &sfn.ListExecutionsOutput{Executions: f.executions}, nil
}

func (f *fakeSfn) GetExecutionHistory(ctx context.Context, params *sfn.GetExecutionHistoryInput, optFns ...func(*sfn.Options)) (*sfn.GetExecutionHistoryOutput, error) {
	f.histories++
	return &sfn.GetExecutionHistoryOutput{Events: f.history}, nil
}

func (f *fakeSfn) StartExecution(ctx context.Context, params *sfn.StartExecutionInput, optFns ...func(*sfn.Options)) (*sfn.StartExecutionOutput, error) {
	f.started = params
	return &sfn.StartExecutionOutput{ExecutionArn: aws.String(executionArn + "-replay")}, nil
}

func (f *fakeSfn) DescribeExecution(ctx context.Context, params *sfn.DescribeExecutionInput, optFns ...func(*sfn.Options)) (*sfn.DescribeExecutionOutput, error) {
	status := f.status
	f.status = sfntypes.ExecutionStatusSucceeded
	return &sfn.DescribeExecutionOutput{Status: status, Output: aws.String(`{"done":true}`)}, nil
}

func (f *fakeSfn) TestState(ctx context.Context, params *sfn.TestStateInput, optFns ...func(*sfn.Options)) (*sfn.TestStateOutput, error) {
	f.tested = params
	return &sfn.TestStateOutput{Status: sfntypes.TestExecutionStatusSucceeded, Output: aws.String(`{"charged":true}`), NextState: aws.String("Fan out")}, nil
}

func newFakeStepFunctions(definition string) (*stepFunctions, *fakeSfn) {
	fake := &fakeSfn{history: checkoutHistory(), executions: []sfntypes.ExecutionListItem{
		{ExecutionArn: aws.String(executionArn), Name: aws.String("run-1"), Status: sfntypes.ExecutionStatusFailed, StartDate: aws.Time(time.Unix(1_700_000_000, 0))},
	}}
	s := newStepFunctions(func(region string) stepFunctionsAPI { return fake })
	s.poll = time.Millisecond
	s.deployed(StateMachinesFromComplete(sfnDeploy(definition)))
	return s, fake
}

func TestRefreshCachesEndedExecutionsAndPublishesChanges(t *testing.T) {
	evts := bus.Subscribe(&StepFunctionExecutionsEvent{})
	s, fake := newFakeStepFunctions(checkoutDefinition)
	s.Refresh(context.Background(), checkoutArn)
	got := (<-evts).(*StepFunctionExecutionsEvent)
	if len(got.Executions) != 1 || len(got.Executions[0].Visits) != 4 || got.Executions[0].Input != `{"order":1}` {
		t.Fatalf("unexpected %+v", got)
	}
	s.Refresh(context.Background(), checkoutArn)
	if fake.histories != 1 {
		t.Fatalf("an ended execution's history is read once, read %d times", fake.histories)
	}
	select {
	case evt := <-evts:
		t.Fatalf("an unchanged list is not published again, got %+v", evt)
	default:
	}
}

func TestReplayStateRunsTheStateAloneWithTheFullVisitInput(t *testing.T) {
	evts := bus.Subscribe(&StepFunctionReplayStartedEvent{}, &StepFunctionReplayEvent{})
	long := `{"blob":"` + strings.Repeat("x", sfnPreviewLimit) + `"}`
	s, fake := newFakeStepFunctions(checkoutDefinition)
	fake.history[1].StateEnteredEventDetails.Input = aws.String(long)
	s.Refresh(context.Background(), checkoutArn)
	err := s.Replay(context.Background(), StepFunctionReplayRequest{StateMachineArn: checkoutArn, ExecutionArn: executionArn, Mode: ReplayState, Visit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(fake.tested.Input) != long || aws.ToString(fake.tested.RoleArn) != checkoutRole {
		t.Fatalf("TestState gets the full input and the machine's role: %+v", fake.tested)
	}
	var state map[string]interface{}
	json.Unmarshal([]byte(aws.ToString(fake.tested.Definition)), &state)
	if state["Type"] != "Task" || state["Next"] != "Fan out" {
		t.Fatalf("TestState gets the state's own definition, got %s", aws.ToString(fake.tested.Definition))
	}
	var started *StepFunctionReplayStartedEvent
	for started == nil {
		started, _ = (<-evts).(*StepFunctionReplayStartedEvent)
	}
	done := (<-evts).(*StepFunctionReplayEvent)
	if started.State != "Charge" || done.ID != started.ID || done.Status != "SUCCEEDED" || done.NextState != "Fan out" {
		t.Fatalf("unexpected %+v / %+v", started, done)
	}

	err = s.Replay(context.Background(), StepFunctionReplayRequest{StateMachineArn: checkoutArn, ExecutionArn: executionArn, Mode: ReplayState, Visit: 99})
	if err == nil {
		t.Fatal("an unknown visit cannot be replayed")
	}
}

func TestReplayStateCarriesAJSONataMachinesQueryLanguage(t *testing.T) {
	definition := `{"QueryLanguage":"JSONata","StartAt":"Charge","States":{"Charge":{"Type":"Pass","End":true}}}`
	s, fake := newFakeStepFunctions(definition)
	s.Refresh(context.Background(), checkoutArn)
	if err := s.Replay(context.Background(), StepFunctionReplayRequest{StateMachineArn: checkoutArn, ExecutionArn: executionArn, Mode: ReplayState, Visit: 2}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aws.ToString(fake.tested.Definition), `"QueryLanguage":"JSONata"`) {
		t.Fatalf("got %s", aws.ToString(fake.tested.Definition))
	}
}

func TestReplayExecutionStartsWithTheInputAndWaitsForTheEnd(t *testing.T) {
	evts := bus.Subscribe(&StepFunctionReplayEvent{})
	s, fake := newFakeStepFunctions(checkoutDefinition)
	fake.status = sfntypes.ExecutionStatusRunning
	if err := s.Replay(context.Background(), StepFunctionReplayRequest{StateMachineArn: checkoutArn, ExecutionArn: executionArn, Mode: ReplayExecution}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(fake.started.Input) != `{"order":1}` || aws.ToString(fake.started.StateMachineArn) != checkoutArn {
		t.Fatalf("unexpected %+v", fake.started)
	}
	var done *StepFunctionReplayEvent
	for done == nil {
		done, _ = (<-evts).(*StepFunctionReplayEvent)
	}
	if done.Status != "SUCCEEDED" || done.ExecutionArn != executionArn+"-replay" || done.Output != `{"done":true}` {
		t.Fatalf("unexpected %+v", done)
	}
}

func TestExecutionOfIsTheStateMachineArn(t *testing.T) {
	if got := executionOf(executionArn); got != checkoutArn {
		t.Fatalf("got %s", got)
	}
}
