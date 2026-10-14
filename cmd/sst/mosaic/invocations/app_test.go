package invocations

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/prettyjson"
)

type harness struct {
	app    *App
	screen tcell.SimulationScreen
	purged []string
	copied []string
}

func newHarness(t *testing.T) *harness {
	h := &harness{}
	store, clock := newTestStore()
	h.app = NewApp(store, clock.now,
		func(queue string, region string) { h.purged = append(h.purged, queue+"@"+region) },
		func(text string) error { h.copied = append(h.copied, text); return nil })
	h.screen = tcell.NewSimulationScreen("")
	if err := h.screen.Init(); err != nil {
		t.Fatal(err)
	}
	h.screen.SetSize(100, 20)
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

func TestNavigatesFromHandlersToInvocationDetail(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "Pool", WorkerID: "w", RequestID: "req-1", Handler: "orders/get", Label: "GET /orders", Input: []byte(`{"id":1}`)})
	h.app.Apply(&aws.FunctionLogEvent{RequestID: "req-1", Line: "fetched order"})
	h.app.Apply(&aws.FunctionResponseEvent{RequestID: "req-1", Output: []byte(`{"total":3}`)})
	h.draw()
	if !strings.Contains(h.text(), "orders/get") {
		t.Fatalf("the handler list must show the group:\n%s", h.text())
	}

	h.key(tcell.KeyEnter)
	h.key(tcell.KeyEnter)
	if !strings.Contains(h.text(), "req-1") || !strings.Contains(h.text(), "fetched order") {
		t.Fatalf("the detail must show the request and its logs:\n%s", h.text())
	}
	h.press('i')
	if !strings.Contains(h.text(), `"total": 3`) {
		t.Fatalf("i must show the pretty output:\n%s", h.text())
	}
	h.press('Y')
	if len(h.copied) != 1 || !strings.Contains(h.copied[0], `"total": 3`) {
		t.Fatalf("Y must copy the output, copied %v", h.copied)
	}
	h.key(tcell.KeyEscape)
	h.key(tcell.KeyEscape)
	if h.app.view != viewGroups {
		t.Fatal("esc twice returns to the handler list")
	}
}

func TestFilterNarrowsTheHandlerList(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "A", RequestID: "1", Handler: "orders/get"})
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "A", RequestID: "2", Handler: "billing/sync"})
	h.press('/')
	for _, r := range "bill" {
		h.press(r)
	}
	h.key(tcell.KeyEnter)
	if strings.Contains(h.text(), "orders/get") || !strings.Contains(h.text(), "billing/sync") {
		t.Fatalf("the filter must hide other handlers:\n%s", h.text())
	}
	h.key(tcell.KeyEscape)
	if !strings.Contains(h.text(), "orders/get") {
		t.Fatal("esc clears the filter")
	}
}

func TestSearchJumpsToTheMatchingInvocation(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", RequestID: "req-9"})
	h.app.Apply(&aws.FunctionLogEvent{RequestID: "req-9", Line: "deadlock detected"})
	h.key(tcell.KeyCtrlF)
	for _, r := range "deadlock" {
		h.press(r)
	}
	h.key(tcell.KeyEnter)
	if !strings.Contains(h.text(), "1 matches") {
		t.Fatalf("search must count matches:\n%s", h.text())
	}
	h.key(tcell.KeyEnter)
	if h.app.view != viewDetail || h.app.request != "req-9" {
		t.Fatal("enter must open the matching invocation")
	}
}

func TestStuckQueueBannerPurgesAfterConfirmation(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(&aws.QueueStallsEvent{Stalls: []aws.QueueStall{{QueueName: "jobs.fifo", Region: "us-east-1", MessageID: "abcdef123456", ReceiveCount: 4, LastError: "boom"}}})
	h.draw()
	if !strings.Contains(h.text(), "queue stuck: jobs.fifo") {
		t.Fatalf("the banner must show:\n%s", h.text())
	}
	h.press('p')
	h.press('n')
	if len(h.purged) != 0 {
		t.Fatal("any key but y cancels")
	}
	h.press('p')
	h.press('y')
	if len(h.purged) != 1 || h.purged[0] != "jobs.fifo@us-east-1" {
		t.Fatalf("purged %v", h.purged)
	}
	h.app.Apply(&aws.QueuePurgedEvent{QueueName: "jobs.fifo"})
	h.draw()
	if !strings.Contains(h.text(), "purged jobs.fifo") {
		t.Fatalf("the outcome must show:\n%s", h.text())
	}
	h.press('x')
	if strings.Contains(h.text(), "queue stuck") {
		t.Fatal("x dismisses the banner for this message")
	}
	h.app.Apply(&aws.QueueStallsEvent{Stalls: []aws.QueueStall{{QueueName: "jobs.fifo", MessageID: "other"}}})
	h.draw()
	if !strings.Contains(h.text(), "queue stuck") {
		t.Fatal("a different stuck message shows again")
	}
}

func TestCtrlCExits(t *testing.T) {
	h := newHarness(t)
	if h.app.HandleKey(tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModNone)) {
		t.Fatal("ctrl-c must exit the pane")
	}
}

func TestPrettyJSONModeUnwrapsNestedJSONInDetail(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", RequestID: "r1", Input: []byte(`{"body":"{\"orderId\":42}"}`)})
	h.app.Apply(&aws.FunctionLogEvent{RequestID: "r1", Line: `INFO {"msg":"saved","meta":"{\"n\":1}"}`})
	h.key(tcell.KeyEnter)
	h.key(tcell.KeyEnter)
	if strings.Contains(h.text(), `"n": 1`) {
		t.Fatal("pretty mode starts off without SST_PRETTY_JSON")
	}
	h.press('J')
	if !strings.Contains(h.text(), `"n": 1`) {
		t.Fatalf("J must pretty print JSON log lines:\n%s", h.text())
	}
	h.press('i')
	if !strings.Contains(h.text(), `"orderId": 42`) {
		t.Fatalf("the input body must be decoded:\n%s", h.text())
	}
	h.press('y')
	if !strings.Contains(h.copied[len(h.copied)-1], `"orderId": 42`) {
		t.Fatal("copy must use the displayed format")
	}
}

func TestWrapLineColoursSegmentsAndBreaksAtWidth(t *testing.T) {
	line := prettyjson.Line{{Kind: prettyjson.Key, Text: `"abcdef"`}, {Kind: prettyjson.Text, Text: "xyz"}}
	rows := wrapLine(line, 5, styleDanger)
	if len(rows) != 3 {
		t.Fatalf("11 runes at width 5 make 3 rows, got %+v", rows)
	}
	if rows[0][0].text != `"abcd` || rows[0][0].style != jsonStyles[prettyjson.Key] {
		t.Fatalf("first row %+v", rows[0])
	}
	last := rows[2][len(rows[2])-1]
	if last.text != "z" || last.style != styleDanger {
		t.Fatalf("text segments take the base style, got %+v", last)
	}
}

func TestPrettyModeColoursJSONInTheDetail(t *testing.T) {
	h := newHarness(t)
	h.app.pretty = true
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", RequestID: "r1", Input: []byte(`{"orderId":42}`)})
	h.key(tcell.KeyEnter)
	h.key(tcell.KeyEnter)
	h.press('i')
	cells, width, _ := h.screen.GetContents()
	text := h.text()
	row := strings.Split(text, "\n")
	for y, line := range row {
		x := strings.Index(line, `"orderId": 42`)
		if x < 0 {
			continue
		}
		x = len([]rune(line[:x]))
		if cells[y*width+x].Style != jsonStyles[prettyjson.Key] {
			t.Fatal("keys must be coloured")
		}
		return
	}
	t.Fatalf("the input must show:\n%s", text)
}

func (h *harness) click(x, y int) {
	h.app.HandleMouse(tcell.NewEventMouse(x, y, tcell.ButtonPrimary, tcell.ModNone))
	h.draw()
}

func (h *harness) wheel(button tcell.ButtonMask) {
	h.app.HandleMouse(tcell.NewEventMouse(0, 5, button, tcell.ModNone))
	h.draw()
}

func rowOf(t *testing.T, h *harness, needle string) int {
	for y, line := range strings.Split(h.text(), "\n") {
		if strings.Contains(line, needle) {
			return y
		}
	}
	t.Fatalf("%q is not on screen:\n%s", needle, h.text())
	return -1
}

func TestClickSelectsThenOpensARow(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "A", RequestID: "1", Handler: "orders/get"})
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "A", RequestID: "2", Handler: "billing/sync"})
	h.draw()

	y := rowOf(t, h, "orders/get")
	h.click(4, y)
	if h.app.view != viewGroups || h.app.groups()[h.app.cursor[viewGroups]].Title != "orders/get" {
		t.Fatal("the first click selects the row")
	}
	h.click(4, y)
	if h.app.view != viewInvocations || h.app.group != "handler:orders/get" {
		t.Fatal("a click on the selected row opens it")
	}
	h.click(0, 0)
	if h.app.view != viewGroups {
		t.Fatal("a click on the title line goes back")
	}
	h.click(4, h.app.height-2)
	if h.app.view != viewGroups {
		t.Fatal("a click on an empty row does nothing")
	}
}

func TestWheelMovesTheSelectionAndStopsFollowing(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", RequestID: id})
	}
	h.draw()
	h.key(tcell.KeyEnter)
	h.wheel(tcell.WheelDown)
	if h.app.cursor[viewInvocations] != 3 || h.app.follow {
		t.Fatalf("the wheel moves three rows and stops following, cursor %d", h.app.cursor[viewInvocations])
	}
	h.wheel(tcell.WheelUp)
	if h.app.cursor[viewInvocations] != 0 {
		t.Fatal("the wheel moves back up")
	}
}

func TestKeysMoveTheInvocationSelectionWhileFollowing(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", RequestID: "r1"})
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", RequestID: "r2"})
	h.draw()
	h.key(tcell.KeyEnter)
	h.press('j')
	if h.app.cursor[viewInvocations] != 1 {
		t.Fatal("j must not be undone by follow on the next draw")
	}
}

func TestLogsKeepTheirTerminalColoursButCopyAsPlainText(t *testing.T) {
	h := newHarness(t)
	h.app.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", RequestID: "r1"})
	h.app.Apply(&aws.FunctionLogEvent{RequestID: "r1", Line: "\x1b[34mDEBUG\x1b[39m: \x1b[1;36mSTAGE\x1b[0m done"})
	h.key(tcell.KeyEnter)
	h.key(tcell.KeyEnter)

	text := h.text()
	if strings.Contains(text, "[34m") || !strings.Contains(text, "DEBUG: STAGE done") {
		t.Fatalf("the detail must render the line without escape codes:\n%s", text)
	}
	cells, width, _ := h.screen.GetContents()
	styleAt := func(needle string) tcell.Style {
		for y, line := range strings.Split(text, "\n") {
			if x := strings.Index(line, needle); x >= 0 {
				return cells[y*width+len([]rune(line[:x]))].Style
			}
		}
		t.Fatalf("%q not drawn", needle)
		return tcell.StyleDefault
	}
	if styleAt("DEBUG") != styleNormal.Foreground(tcell.PaletteColor(4)) {
		t.Fatal("DEBUG must be blue")
	}
	if styleAt("STAGE") != styleNormal.Foreground(tcell.PaletteColor(6)).Bold(true) {
		t.Fatal("STAGE must be bold cyan")
	}
	if styleAt(": STAGE") != styleNormal || styleAt(" done") != styleNormal {
		t.Fatal("39 and 0 must return to the pane's own style")
	}

	h.press('c')
	if len(h.copied) != 1 || h.copied[0] != "DEBUG: STAGE done" {
		t.Fatalf("c must copy plain text, copied %q", h.copied)
	}
}

func TestApplySGRReadsExtendedColours(t *testing.T) {
	got := applySGR(styleNormal, styleNormal, []int{38, 5, 208, 48, 2, 10, 20, 30, 4})
	want := styleNormal.Foreground(tcell.PaletteColor(208)).Background(tcell.NewRGBColor(10, 20, 30)).Underline(true)
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}
