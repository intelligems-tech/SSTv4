package stepfunctions

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/project"
)

const (
	machineArn   = "arn:aws:states:us-east-1:123:stateMachine:dev-checkout"
	executionArn = "arn:aws:states:us-east-1:123:execution:dev-checkout:run-1"
)

const definition = `{"StartAt":"Validate","States":{
	"Validate":{"Type":"Task","Resource":"arn:aws:states:::lambda:invoke","Parameters":{"FunctionName":"arn:aws:lambda:us-east-1:123:function:dev-validate"},"Next":"Route","Catch":[{"ErrorEquals":["States.ALL"],"Next":"Give up"}]},
	"Route":{"Type":"Choice","Choices":[{"Variable":"$.ship","BooleanEquals":true,"Next":"Fan out"},{"Variable":"$.wait","BooleanEquals":true,"Next":"Wait a bit"}],"Default":"Done"},
	"Wait a bit":{"Type":"Wait","Seconds":5,"Next":"Validate"},
	"Fan out":{"Type":"Parallel","Next":"Done","Branches":[
		{"StartAt":"Charge","States":{"Charge":{"Type":"Task","Resource":"arn:aws:lambda:us-east-1:123:function:dev-charge","End":true}}},
		{"StartAt":"Email","States":{"Email":{"Type":"Task","Resource":"arn:aws:states:::sns:publish","Next":"Log"},"Log":{"Type":"Pass","End":true}}}]},
	"Give up":{"Type":"Fail"},
	"Done":{"Type":"Succeed"}}}`

func deploy() *project.CompleteEvent {
	return &project.CompleteEvent{Finished: true, Resources: []apitype.ResourceV3{
		{Type: "aws:sfn/stateMachine:StateMachine", Outputs: map[string]interface{}{
			"name": "dev-checkout", "arn": machineArn, "type": "STANDARD", "roleArn": "arn:aws:iam::123:role/x", "definition": definition,
		}},
	}}
}

func executions(at time.Time) *aws.StepFunctionExecutionsEvent {
	return &aws.StepFunctionExecutionsEvent{StateMachineArn: machineArn, Executions: []aws.StepFunctionExecution{{
		Arn: executionArn, Name: "run-1", Status: "FAILED", Start: at, Stop: at.Add(3 * time.Second), Input: `{"order":1}`, Error: "Boom",
		Visits: []aws.StepFunctionVisit{
			{ID: 2, State: "Validate", Status: aws.VisitSucceeded, Entered: at, Exited: at.Add(time.Second), Input: `{"order":1}`, Output: `{"ship":true}`},
			{ID: 7, State: "Route", Status: aws.VisitSucceeded, Entered: at.Add(time.Second), Exited: at.Add(time.Second)},
			{ID: 9, State: "Fan out", Status: aws.VisitFailed, Entered: at.Add(time.Second), Error: "Boom"},
			{ID: 11, State: "Charge", Status: aws.VisitFailed, Entered: at.Add(time.Second), Input: `{"ship":true}`, Error: "Boom", Cause: "card declined"},
		},
	}}}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type recorder struct {
	fetches []string
	replays []aws.StepFunctionReplayRequest
}

func newTestApp() (*App, *recorder, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	r := &recorder{}
	app := NewApp(NewStore(), c.now,
		func(arn string) { r.fetches = append(r.fetches, arn) },
		func(req aws.StepFunctionReplayRequest) { r.replays = append(r.replays, req) },
		func(string) error { return nil })
	app.SetStage("dev")
	return app, r, c
}

func key(r rune) *tcell.EventKey {
	return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone)
}

func special(k tcell.Key) *tcell.EventKey {
	return tcell.NewEventKey(k, 0, tcell.ModNone)
}

func draw(app *App, width int, height int) (tcell.SimulationScreen, string) {
	screen := tcell.NewSimulationScreen("")
	screen.Init()
	screen.SetSize(width, height)
	app.Draw(screen)
	cells, w, h := screen.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if runes := cells[y*w+x].Runes; len(runes) > 0 {
				b.WriteRune(runes[0])
			}
		}
		b.WriteRune('\n')
	}
	return screen, b.String()
}

func TestGraphLaysOutEveryStateWithoutOverlap(t *testing.T) {
	m, err := aws.ParseDefinition(definition)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGraph(m)
	for _, name := range []string{"Validate", "Route", "Wait a bit", "Fan out", "Charge", "Email", "Log", "Give up", "Done"} {
		if g.Node(name) == nil {
			t.Fatalf("no node for %s", name)
		}
	}
	if !(g.Node("Validate").Y < g.Node("Route").Y && g.Node("Fan out").Y < g.Node("Charge").Y && g.Node("Log").Y < g.Node("Done").Y) {
		t.Fatal("states flow top to bottom, branches below their Parallel and above what follows it")
	}
	for _, a := range g.Nodes {
		for _, b := range g.Nodes {
			if a != b && a.Box() && b.Box() && a.Y == b.Y && a.X < b.X+b.W && b.X < a.X+a.W {
				t.Fatalf("%q and %q overlap", a.Label, b.Label)
			}
		}
	}
	text := g.Draw(func(*Node, bool) tcell.Style { return tcell.StyleDefault }, nil, tcell.StyleDefault).String()
	for _, want := range []string{"Start", "λ Validate", "◇ Route", "‖ Fan out", "✓ Done", "◀", "▼"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if n := g.Neighbour(g.Node("Validate"), 0, 1); n == nil || n.State.Name != "Route" && n.State.Name != "Give up" {
		t.Fatalf("down from Validate reaches the next layer, got %+v", n)
	}
}

func TestOpeningAMachineDrawsItsGraphColouredByTheLatestExecution(t *testing.T) {
	app, rec, c := newTestApp()
	app.Apply(deploy())
	_, text := draw(app, 120, 10)
	if !strings.Contains(text, "checkout") || !strings.Contains(text, "standard") {
		t.Fatalf("the list shows the machine\n%s", text)
	}
	app.HandleKey(special(tcell.KeyEnter))
	if len(rec.fetches) != 1 || rec.fetches[0] != machineArn {
		t.Fatalf("opening a machine reads its executions, got %v", rec.fetches)
	}
	app.Apply(executions(c.now()))
	screen, text := draw(app, 160, 60)
	for _, want := range []string{"› checkout", "execution 1/1", "run-1", "failed", "λ Validate", "dev-validate"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	m := app.store.Machine(machineArn)
	charge := m.Graph.Node("Charge")
	_, _, style, _ := screen.GetContent(app.originX+charge.X-app.scrollX, app.originY+charge.Y-app.scrollY)
	if fg, _, _ := style.Decompose(); fg != tcell.ColorRed {
		t.Fatalf("a failed state is red, got %v", fg)
	}

	c.t = c.t.Add(refreshOpen)
	app.Tick()
	if len(rec.fetches) != 2 {
		t.Fatalf("the open machine is read again every %s, got %v", refreshOpen, rec.fetches)
	}
}

func TestClickingANodeShowsItsVisitsAndReplaysThem(t *testing.T) {
	app, rec, c := newTestApp()
	app.Apply(deploy())
	app.HandleKey(special(tcell.KeyEnter))
	app.Apply(executions(c.now()))
	draw(app, 160, 60)

	charge := app.store.Machine(machineArn).Graph.Node("Charge")
	click := func() {
		app.HandleMouse(tcell.NewEventMouse(app.originX+charge.X+1-app.scrollX, app.originY+charge.Y+1-app.scrollY, tcell.ButtonPrimary, tcell.ModNone))
	}
	click()
	if app.state != "Charge" || app.view != viewGraph {
		t.Fatal("a click selects the node")
	}
	click()
	if app.view != viewState {
		t.Fatal("a second click opens the node")
	}
	_, text := draw(app, 160, 20)
	for _, want := range []string{"› Charge", "failed", "run-1", "Boom", "TestState"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}

	app.HandleKey(key('r'))
	app.HandleKey(key('n'))
	if len(rec.replays) != 0 {
		t.Fatal("any key but y or enter cancels")
	}
	app.HandleKey(key('r'))
	app.HandleKey(key('y'))
	want := aws.StepFunctionReplayRequest{StateMachineArn: machineArn, ExecutionArn: executionArn, Mode: aws.ReplayState, Visit: 11}
	if len(rec.replays) != 1 || rec.replays[0] != want {
		t.Fatalf("r replays the visit's state, got %+v", rec.replays)
	}
	app.HandleKey(key('R'))
	app.HandleKey(special(tcell.KeyEnter))
	if rec.replays[1].Mode != aws.ReplayExecution || rec.replays[1].ExecutionArn != executionArn {
		t.Fatalf("R replays the visit's execution, got %+v", rec.replays[1])
	}

	app.Apply(&aws.StepFunctionReplayStartedEvent{ID: "x", StateMachineArn: machineArn, Mode: aws.ReplayState, State: "Charge", Source: executionArn, Input: `{"ship":true}`, At: c.now()})
	app.Apply(&aws.StepFunctionReplayEvent{ID: "x", StateMachineArn: machineArn, Status: "SUCCEEDED", Output: `{"charged":true}`, Duration: 1200 * time.Millisecond})
	if app.message != "Charge succeeded in 1.2s" {
		t.Fatalf("got %q", app.message)
	}
	_, text = draw(app, 160, 20)
	if !strings.Contains(text, "↻ replay") || !strings.Contains(text, `{"charged":true}`) {
		t.Fatalf("the replay is listed with its output\n%s", text)
	}
}

func TestAParallelCannotBeReplayedAlone(t *testing.T) {
	app, rec, c := newTestApp()
	app.Apply(deploy())
	app.HandleKey(special(tcell.KeyEnter))
	app.Apply(executions(c.now()))
	app.state = "Fan out"
	app.HandleKey(special(tcell.KeyEnter))
	app.HandleKey(key('r'))
	if app.confirm != nil || !strings.Contains(app.message, "TestState cannot") {
		t.Fatalf("got confirm %+v, message %q", app.confirm, app.message)
	}
	app.HandleKey(key('R'))
	app.HandleKey(key('y'))
	if len(rec.replays) != 1 || rec.replays[0].Mode != aws.ReplayExecution {
		t.Fatalf("the execution can still be replayed, got %+v", rec.replays)
	}
}

func TestStateStatusPrefersFailures(t *testing.T) {
	execution := &aws.StepFunctionExecution{Visits: []aws.StepFunctionVisit{
		{State: "Item", Status: aws.VisitSucceeded},
		{State: "Item", Status: aws.VisitFailed},
		{State: "Item", Status: aws.VisitRunning},
	}}
	if got := StateStatus(execution, "Item"); got != aws.VisitFailed {
		t.Fatalf("got %q", got)
	}
	if got := StateStatus(execution, "Other"); got != "" {
		t.Fatalf("got %q", got)
	}
}
