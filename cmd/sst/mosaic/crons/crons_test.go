package crons

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/project"
)

const nightly = "rule/dev-nightly/Target0"

func deploy(names ...string) *project.CompleteEvent {
	resources := []apitype.ResourceV3{}
	for _, name := range names {
		resources = append(resources,
			apitype.ResourceV3{Type: "aws:cloudwatch/eventRule:EventRule", Outputs: map[string]interface{}{
				"name": name, "arn": "arn:aws:events:us-east-1:123:rule/" + name, "scheduleExpression": "rate(1 day)", "state": "ENABLED",
			}},
			apitype.ResourceV3{Type: "aws:cloudwatch/eventTarget:EventTarget", Outputs: map[string]interface{}{
				"rule": name, "targetId": "Target0", "arn": "arn:aws:lambda:us-east-1:123:function:" + name,
			}},
		)
	}
	return &project.CompleteEvent{Finished: true, Resources: resources}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestApp() (*App, *[]string, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	runs := &[]string{}
	app := NewApp(NewStore(), c.now, func(key string) { *runs = append(*runs, key) }, func(string) error { return nil })
	app.SetStage("dev")
	return app, runs, c
}

func key(r rune) *tcell.EventKey {
	return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone)
}

func screenText(screen tcell.SimulationScreen) string {
	cells, width, height := screen.GetContents()
	var b strings.Builder
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if runes := cells[y*width+x].Runes; len(runes) > 0 {
				b.WriteRune(runes[0])
			}
		}
		b.WriteRune('\n')
	}
	return b.String()
}

func TestRunNeedsConfirmationAndRunsTheCronUnderTheCursor(t *testing.T) {
	app, runs, _ := newTestApp()
	app.Apply(deploy("dev-nightly", "dev-weekly"))
	app.HandleKey(key('r'))
	app.HandleKey(key('n'))
	if len(*runs) != 0 {
		t.Fatal("any key but y or enter cancels")
	}
	app.HandleKey(key('r'))
	app.HandleKey(key('y'))
	if len(*runs) != 1 || (*runs)[0] != nightly {
		t.Fatalf("ran %v", *runs)
	}
	app.HandleKey(key('j'))
	app.HandleKey(key('r'))
	app.HandleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if (*runs)[1] != "rule/dev-weekly/Target0" {
		t.Fatalf("ran %v", *runs)
	}
}

func TestStoreTracksRunsAcrossRedeploys(t *testing.T) {
	app, _, c := newTestApp()
	app.Apply(deploy("dev-nightly"))
	if app.Apply(&aws.CronRunEvent{Key: nightly, RunID: "unknown"}) {
		t.Fatal("an unknown run changes nothing")
	}
	app.Apply(&aws.CronRunStartedEvent{Key: nightly, RunID: "1", At: c.now(), Payload: "{}"})
	cron := app.store.Cron(nightly)
	if cron.Last().Status != Running {
		t.Fatalf("unexpected %+v", cron.Last())
	}
	app.Apply(&aws.CronRunEvent{Key: nightly, RunID: "1", RequestID: "req", Duration: 1500 * time.Millisecond, Output: `"ok"`})
	if run := cron.Last(); run.Status != Done || run.RequestID != "req" || app.message != "nightly ran in 1.5s" {
		t.Fatalf("unexpected %+v / %q", run, app.message)
	}
	app.Apply(&aws.CronRunStartedEvent{Key: nightly, RunID: "2", At: c.now()})
	app.Apply(&aws.CronRunEvent{Key: nightly, RunID: "2", FunctionError: "Unhandled", Output: `{"errorMessage":"db down"}`})
	if run := cron.Last(); run.Status != Failed || run.Error != "db down" || app.message != "nightly failed: db down" {
		t.Fatalf("a function error shows its message: %+v / %q", run, app.message)
	}
	app.Apply(deploy("dev-nightly"))
	if len(app.store.Cron(nightly).Runs) != 2 {
		t.Fatal("a redeploy keeps the runs")
	}
	app.Apply(deploy("dev-weekly"))
	if app.store.Cron(nightly) != nil {
		t.Fatal("a removed cron leaves the table")
	}
}

func TestTableShowsCronsWithTheirLastResult(t *testing.T) {
	app, _, c := newTestApp()
	app.Apply(deploy("dev-nightly", "dev-weekly"))
	app.Apply(&aws.CronRunStartedEvent{Key: nightly, RunID: "1", At: c.now()})
	app.Apply(&aws.CronRunEvent{Key: nightly, RunID: "1", Error: "AccessDenied"})
	c.t = c.t.Add(5 * time.Second)
	screen := tcell.NewSimulationScreen("")
	screen.Init()
	screen.SetSize(140, 10)
	app.Draw(screen)
	text := screenText(screen)
	for _, want := range []string{"⏱ Crons", "nightly", "weekly", "rate(1 day)", "dev-nightly", "enabled", "failed", "r run now"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	app.HandleKey(key('/'))
	for _, r := range "week" {
		app.HandleKey(key(r))
	}
	if rows := app.table(); len(rows) != 1 || rows[0].Name != "dev-weekly" {
		t.Fatalf("filter matches the shown name, got %v", rows)
	}
}
