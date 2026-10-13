package queues

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/project"
)

const jobsArn = "arn:aws:sqs:us-east-1:123:x-jobs.fifo"
const dlqArn = "arn:aws:sqs:us-east-1:123:x-jobs-dlq.fifo"

func deploy() *project.CompleteEvent {
	return &project.CompleteEvent{Finished: true, Resources: []apitype.ResourceV3{
		{Type: "aws:sqs/queue:Queue", Outputs: map[string]interface{}{
			"name": "x-jobs.fifo", "arn": jobsArn, "url": "https://sqs/x-jobs.fifo", "fifoQueue": true,
			"redrivePolicy": `{"deadLetterTargetArn":"` + dlqArn + `","maxReceiveCount":5}`,
		}},
		{Type: "aws:sqs/queue:Queue", Outputs: map[string]interface{}{"name": "x-jobs-dlq.fifo", "arn": dlqArn, "url": "https://sqs/x-jobs-dlq.fifo", "fifoQueue": true}},
		{Type: "aws:sqs/queue:Queue", Outputs: map[string]interface{}{"name": "x-idle", "arn": "arn:aws:sqs:us-east-1:123:x-idle", "url": "https://sqs/x-idle"}},
	}}
}

func batch(ids []string, receive int) []byte {
	records := []string{}
	for _, id := range ids {
		records = append(records, fmt.Sprintf(`{"messageId":%q,"eventSource":"aws:sqs","eventSourceARN":%q,"body":"{\"order\":1}","attributes":{"ApproximateReceiveCount":"%d","MessageGroupId":"org-1"}}`, id, jobsArn, receive))
	}
	return []byte(`{"Records":[` + strings.Join(records, ",") + `]}`)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestStore() (*Store, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	return NewStore(c.now), c
}

func TestStoreListsDeployedQueuesWithTheirDLQ(t *testing.T) {
	s, _ := newTestStore()
	s.Apply(deploy())
	jobs, dlq := s.Queue("x-jobs.fifo"), s.Queue("x-jobs-dlq.fifo")
	if jobs == nil || !jobs.Deployed || !jobs.Fifo || jobs.DLQ != "x-jobs-dlq.fifo" || jobs.MaxReceive != 5 {
		t.Fatalf("unexpected queue %+v", jobs)
	}
	if !dlq.IsDLQ() || dlq.Sources[0] != "x-jobs.fifo" || jobs.IsDLQ() {
		t.Fatal("the DLQ must know its source")
	}
	s.Apply(deploy())
	if len(s.Queue("x-jobs-dlq.fifo").Sources) != 1 {
		t.Fatal("a redeploy must not duplicate sources")
	}
	if s.Apply(&project.CompleteEvent{Old: true}) {
		t.Fatal("a replayed deploy changes nothing")
	}
}

func TestStoreTracksMessagesAndPartialBatchFailures(t *testing.T) {
	s, _ := newTestStore()
	s.Apply(deploy())
	if s.Apply(&aws.FunctionInvokedEvent{RequestID: "http", Input: []byte(`{"rawPath":"/"}`)}) {
		t.Fatal("non-SQS invocations do not touch queues")
	}
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "Pool", RequestID: "r1", Label: "sqs x-jobs.fifo", Input: batch([]string{"m1", "m2"}, 1)})
	jobs := s.Queue("x-jobs.fifo")
	if jobs.Received != 2 || jobs.Running != 2 || jobs.Consumers[0] != "sqs x-jobs.fifo" {
		t.Fatalf("unexpected %+v", jobs)
	}
	s.Apply(&aws.FunctionResponseEvent{RequestID: "r1", Output: []byte(`{"batchItemFailures":[{"itemIdentifier":"m2"}]}`)})
	recent := jobs.Recent()
	if jobs.Running != 0 || jobs.Failed != 1 || recent[0].ID != "m2" || recent[0].Status != Failed || recent[1].Status != Done {
		t.Fatalf("unexpected %+v / %+v %+v", jobs, recent[0], recent[1])
	}
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "Pool", RequestID: "r2", Input: batch([]string{"m2"}, 2)})
	s.Apply(&aws.FunctionErrorEvent{RequestID: "r2", ErrorType: "Error", ErrorMessage: "boom"})
	if jobs.Failed != 2 || jobs.Recent()[0].Error != "boom" || jobs.Recent()[0].Receive != 2 {
		t.Fatalf("a thrown error fails every message of the batch, got %+v", jobs.Recent()[0])
	}
}

func TestStoreBoundsMessagesButKeepsRunningCounts(t *testing.T) {
	s, _ := newTestStore()
	for i := 0; i < perQueue+3; i++ {
		s.Apply(&aws.FunctionInvokedEvent{RequestID: fmt.Sprint(i), Input: batch([]string{fmt.Sprint("m", i)}, 1)})
	}
	s.Apply(&aws.FunctionResponseEvent{RequestID: "0"})
	jobs := s.Queue("x-jobs.fifo")
	if len(jobs.Messages) != perQueue || jobs.Running != perQueue+2 {
		t.Fatalf("kept %d, running %d", len(jobs.Messages), jobs.Running)
	}
}

func TestListPutsStuckQueuesFirstThenActivity(t *testing.T) {
	s, c := newTestStore()
	s.Apply(deploy())
	s.Apply(&aws.FunctionInvokedEvent{RequestID: "r1", Input: batch([]string{"m1"}, 1)})
	list := s.List("")
	if list[0].Name != "x-jobs.fifo" || list[1].Name != "x-idle" {
		t.Fatalf("active queues come first, got %s %s", list[0].Name, list[1].Name)
	}
	c.t = c.t.Add(time.Minute)
	s.Apply(&aws.QueueStallsEvent{Stalls: []aws.QueueStall{{QueueName: "x-idle"}}})
	if s.List("")[0].Name != "x-idle" {
		t.Fatal("a stuck queue comes first")
	}
	if got := s.List("dlq"); len(got) != 1 || got[0].Name != "x-jobs-dlq.fifo" {
		t.Fatalf("the filter matches names, got %d", len(got))
	}
	s.Apply(&aws.QueueStallsEvent{})
	if s.Queue("x-idle").Stall != nil {
		t.Fatal("a recovered queue clears its stall")
	}
}

type harness struct {
	t      *testing.T
	app    *App
	screen tcell.SimulationScreen
	purged []string
	copied []string
}

func newHarness(t *testing.T) *harness {
	h := &harness{}
	s, c := newTestStore()
	h.app = NewApp(s, c.now,
		func(queue string, region string) { h.purged = append(h.purged, queue+"@"+region) },
		func(text string) error { h.copied = append(h.copied, text); return nil })
	h.screen = tcell.NewSimulationScreen("")
	if err := h.screen.Init(); err != nil {
		t.Fatal(err)
	}
	h.screen.SetSize(140, 24)
	return h
}

func (h *harness) key(k tcell.Key) { h.app.HandleKey(tcell.NewEventKey(k, 0, tcell.ModNone)); h.draw() }
func (h *harness) press(r rune) {
	h.app.HandleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	h.draw()
}
func (h *harness) draw() { h.app.Draw(h.screen) }

func (h *harness) text() string {
	cells, width, _ := h.screen.GetContents()
	var b strings.Builder
	for i, cell := range cells {
		if len(cell.Runes) > 0 {
			b.WriteRune(cell.Runes[0])
		}
		if (i+1)%width == 0 {
			b.WriteRune('\n')
		}
	}
	return b.String()
}

// row returns the fields of the table row whose queue column is name.
func (h *harness) row(name string) []string {
	for _, line := range strings.Split(h.text(), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] == name {
			return fields
		}
	}
	return nil
}

func depths() *aws.QueueDepthsEvent {
	return &aws.QueueDepthsEvent{At: time.Unix(1_700_000_000, 0), Depths: []aws.QueueDepth{
		{QueueName: "x-jobs.fifo", Visible: 12, InFlight: 3},
		{QueueName: "x-jobs-dlq.fifo", Visible: 4},
		{QueueName: "x-idle", Error: "AccessDenied"},
	}}
}

func TestTableShowsOneRowPerQueueWithTheDLQFolded(t *testing.T) {
	h := newHarness(t)
	h.app.SetStage("x")
	h.app.Apply(deploy())
	h.app.Apply(depths())
	h.app.Apply(&aws.FunctionInvokedEvent{RequestID: "r1", Input: batch([]string{"msg-0001"}, 3)})
	h.app.Apply(&aws.FunctionErrorEvent{RequestID: "r1", ErrorMessage: "boom"})
	h.draw()

	header := strings.Fields(strings.Split(h.text(), "\n")[2])
	if strings.Join(header, " ") != "Queue Waiting Flight DLQ Recv Failed Last" {
		t.Fatalf("unexpected header %v:\n%s", header, h.text())
	}
	jobs := h.row("jobs.fifo")
	if strings.Join(jobs[2:7], " ") != "12 3 4 1 1" {
		t.Fatalf("jobs row %v:\n%s", jobs, h.text())
	}
	if idle := h.row("idle"); strings.Join(idle[2:7], " ") != "– – – 0 0" {
		t.Fatalf("an unreadable depth shows a dash, got %v", idle)
	}
	if h.row("jobs-dlq.fifo") != nil {
		t.Fatal("a DLQ folds into its source row by default")
	}
	h.press('D')
	if dlq := h.row("jobs-dlq.fifo"); dlq == nil || dlq[2] != "(DLQ)" {
		t.Fatalf("D shows DLQ rows:\n%s", h.text())
	}
}

func TestSortingByColumnKeepsStuckQueuesFirst(t *testing.T) {
	h := newHarness(t)
	h.app.SetStage("x")
	h.app.Apply(deploy())
	h.app.Apply(depths())
	h.app.Apply(&aws.QueueDepthsEvent{Depths: []aws.QueueDepth{{QueueName: "x-jobs.fifo", Visible: 1}, {QueueName: "x-idle", Visible: 9}}})
	h.draw()
	order := func() []string {
		names := []string{}
		for _, q := range h.app.table() {
			names = append(names, h.app.displayName(q.Name))
		}
		return names
	}

	h.press('2')
	if got := order(); got[0] != "idle" || !strings.Contains(h.text(), "Waiting▼") {
		t.Fatalf("2 sorts by waiting, most first, got %v", got)
	}
	h.press('2')
	if got := order(); got[0] != "jobs.fifo" || !strings.Contains(h.text(), "Waiting▲") {
		t.Fatalf("choosing the column again flips it, got %v", got)
	}
	h.press('S')
	if got := order(); got[0] != "idle" {
		t.Fatalf("S flips the direction, got %v", got)
	}
	h.app.Apply(&aws.QueueStallsEvent{Stalls: []aws.QueueStall{{QueueName: "x-jobs.fifo"}}})
	if got := order(); got[0] != "jobs.fifo" {
		t.Fatalf("a stuck queue stays on top of any sort, got %v", got)
	}

	h.app.HandleMouse(tcell.NewEventMouse(3, h.app.headerY, tcell.ButtonPrimary, tcell.ModNone))
	if h.app.sortColumn != colQueue {
		t.Fatalf("clicking a header sorts by it, got column %d", h.app.sortColumn)
	}
}

func TestMessagesViewShowsRedriveAndBodies(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(deploy())
	h.app.Apply(depths())
	h.app.Apply(&aws.FunctionInvokedEvent{RequestID: "r1", Label: "sqs x-jobs.fifo", Input: batch([]string{"msg-0001"}, 3)})
	h.app.Apply(&aws.FunctionErrorEvent{RequestID: "r1", ErrorMessage: "boom"})
	h.draw()

	h.key(tcell.KeyEnter)
	text := h.text()
	if !strings.Contains(text, "DLQ x-jobs-dlq.fifo after 5 receives") || !strings.Contains(text, "msg-0001") || !strings.Contains(text, "×3") || !strings.Contains(text, "boom") {
		t.Fatalf("the message view must show the redrive and the message:\n%s", text)
	}
	h.key(tcell.KeyEnter)
	if !strings.Contains(h.text(), `"order": 1`) {
		t.Fatalf("enter expands the body:\n%s", h.text())
	}
	h.press('y')
	if len(h.copied) != 1 || !strings.Contains(h.copied[0], `"order": 1`) {
		t.Fatalf("y copies the body, got %v", h.copied)
	}
	h.press('d')
	if h.app.queue != "x-jobs-dlq.fifo" {
		t.Fatal("d opens the DLQ")
	}
}

func TestPurgeAsksBeforePurgingTheSelectedQueue(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(deploy())
	h.app.Apply(&aws.FunctionInvokedEvent{RequestID: "r1", Input: batch([]string{"m1"}, 1)})
	h.draw()
	h.press('p')
	if !strings.Contains(h.text(), "purge every message in x-jobs.fifo?") {
		t.Fatalf("p must ask first:\n%s", h.text())
	}
	h.press('n')
	if len(h.purged) != 0 {
		t.Fatal("any key but y cancels")
	}
	h.press('p')
	h.press('y')
	if len(h.purged) != 1 || h.purged[0] != "x-jobs.fifo@us-east-1" {
		t.Fatalf("purged %v", h.purged)
	}
	h.app.Apply(&aws.QueuePurgedEvent{QueueName: "x-jobs.fifo", Error: "retry in 50s"})
	h.draw()
	if !strings.Contains(h.text(), "purge x-jobs.fifo failed: retry in 50s") {
		t.Fatalf("the outcome must show:\n%s", h.text())
	}
}

func TestFilterAndCopyURL(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(deploy())
	h.draw()
	h.press('/')
	for _, r := range "idle" {
		h.press(r)
	}
	h.key(tcell.KeyEnter)
	if strings.Contains(h.text(), "x-jobs.fifo") {
		t.Fatalf("the filter hides other queues:\n%s", h.text())
	}
	h.press('y')
	if len(h.copied) != 1 || h.copied[0] != "https://sqs/x-idle" {
		t.Fatalf("y copies the queue URL, got %v", h.copied)
	}
}

// styleAt is the style of the first cell of text on the row of name.
func (h *harness) styleAt(name string, text string) tcell.Style {
	cells, width, height := h.screen.GetContents()
	for y := 0; y < height; y++ {
		line := ""
		for x := 0; x < width; x++ {
			if r := cells[y*width+x].Runes; len(r) > 0 {
				line += string(r[0])
			} else {
				line += " "
			}
		}
		if !strings.Contains(line, " "+name+" ") {
			continue
		}
		at := strings.LastIndex(line, text)
		_, _, style, _ := h.screen.GetContent(len([]rune(line[:at])), y)
		return style
	}
	h.t.Fatalf("no row %q", name)
	return tcell.StyleDefault
}

func TestCellsAreColouredByMeaningOnEveryRow(t *testing.T) {
	h := newHarness(t)
	h.t = t
	h.app.SetStage("x")
	h.app.Apply(deploy())
	h.app.Apply(depths())
	h.app.Apply(&aws.FunctionInvokedEvent{RequestID: "r1", Input: batch([]string{"m1"}, 1)})
	h.app.Apply(&aws.FunctionErrorEvent{RequestID: "r1", ErrorMessage: "boom"})
	h.press('2')

	fg := func(style tcell.Style) tcell.Color { color, _, _ := style.Decompose(); return color }
	// jobs.fifo is the cursor row: failed stays red on the highlight.
	failed := h.styleAt("jobs.fifo", "1")
	if fg(failed) != tcell.ColorRed {
		t.Fatalf("failed count must be red, got %v", fg(failed))
	}
	if _, bg, _ := failed.Decompose(); bg == tcell.ColorDefault {
		t.Fatal("the cursor row keeps a highlight background")
	}
	if fg(h.styleAt("jobs.fifo", "12")) != tcell.ColorYellow {
		t.Fatal("a backlog is yellow")
	}
	if fg(h.styleAt("jobs.fifo", "4")) != tcell.ColorRed {
		t.Fatal("dead letters are red")
	}
	if fg(h.styleAt("jobs.fifo", "3")) != tcell.ColorAqua {
		t.Fatal("in-flight messages are cyan")
	}
}
