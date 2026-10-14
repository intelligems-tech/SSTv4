package queues

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/cmd/sst/mosaic/panekit"
)

// Purger purges a queue by name.
type Purger func(queue string, region string)

// Copier puts text on the clipboard.
type Copier func(text string) error

type view int

const (
	viewQueues view = iota
	viewMessages
)

// App is the Queues pane: a list of queues and, for one queue, the messages
// its local consumers received.
type App struct {
	store *Store
	now   func() time.Time
	purge Purger
	copy  Copier

	view     view
	queue    string
	cursor   map[view]int
	expanded string
	filter   string
	editing  bool
	confirm  string
	message  string
	messaged time.Time
	bodyTop  int
	headerY  int
	height   int
	width    int

	// prefix is the stage prefix queue names share, hidden in the table.
	prefix         string
	showDLQs       bool
	sortColumn     int
	sortDescending bool
}

func NewApp(store *Store, now func() time.Time, purge Purger, copy Copier) *App {
	return &App{store: store, now: now, purge: purge, copy: copy, cursor: map[view]int{}, sortColumn: -1}
}

// SetStage hides the `<stage>-` prefix every queue name carries.
func (a *App) SetStage(stage string) {
	a.prefix = ""
	if stage != "" {
		a.prefix = stage + "-"
	}
}

func (a *App) Apply(evt interface{}) bool {
	changed := a.store.Apply(evt)
	if purged, ok := evt.(*aws.QueuePurgedEvent); ok {
		if purged.Error != "" {
			a.flash("purge " + purged.QueueName + " failed: " + purged.Error)
		} else {
			a.flash("purged " + purged.QueueName)
		}
	}
	return changed
}

func (a *App) flash(message string) {
	a.message, a.messaged = message, a.now()
}

// selected is the queue under the cursor, or the open one.
func (a *App) selected() *Queue {
	if a.view == viewMessages {
		return a.store.Queue(a.queue)
	}
	list := a.table()
	if len(list) == 0 {
		return nil
	}
	return list[min(a.cursor[viewQueues], len(list)-1)]
}

func (a *App) rows() int {
	if a.view == viewQueues {
		return len(a.table())
	}
	if q := a.store.Queue(a.queue); q != nil {
		return len(q.Messages)
	}
	return 0
}

func (a *App) move(delta int) {
	a.cursor[a.view] = min(max(0, a.cursor[a.view]+delta), max(0, a.rows()-1))
}

// HandleKey applies one key press; false exits the pane.
func (a *App) HandleKey(evt *tcell.EventKey) bool {
	if evt.Key() == tcell.KeyCtrlC {
		return false
	}
	if a.editing {
		switch evt.Key() {
		case tcell.KeyEnter:
			a.editing = false
		case tcell.KeyEscape:
			a.editing, a.filter = false, ""
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			if a.filter != "" {
				_, size := utf8.DecodeLastRuneInString(a.filter)
				a.filter = a.filter[:len(a.filter)-size]
			}
		case tcell.KeyRune:
			a.filter += string(evt.Rune())
		}
		a.cursor[viewQueues] = 0
		return true
	}
	if a.confirm != "" {
		queue := a.confirm
		a.confirm = ""
		if evt.Rune() == 'y' || evt.Key() == tcell.KeyEnter {
			region := ""
			if q := a.store.Queue(queue); q != nil {
				region = regionFromArn(q.Arn)
			}
			a.flash("purging " + queue + "…")
			a.purge(queue, region)
		}
		return true
	}
	half := max(1, (a.height-4)/2)
	switch evt.Key() {
	case tcell.KeyUp:
		a.move(-1)
	case tcell.KeyDown:
		a.move(1)
	case tcell.KeyCtrlU, tcell.KeyPgUp:
		a.move(-half)
	case tcell.KeyCtrlD, tcell.KeyPgDn:
		a.move(half)
	case tcell.KeyEnter, tcell.KeyRight:
		a.open()
	case tcell.KeyEscape, tcell.KeyLeft, tcell.KeyBackspace, tcell.KeyBackspace2:
		a.back()
	case tcell.KeyRune:
		switch evt.Rune() {
		case 'j':
			a.move(1)
		case 'k':
			a.move(-1)
		case 'g':
			a.cursor[a.view] = 0
		case 'G':
			a.cursor[a.view] = max(0, a.rows()-1)
		case 'l':
			a.open()
		case 'h':
			a.back()
		case '/':
			if a.view == viewQueues {
				a.editing = true
			}
		case 'p':
			if q := a.selected(); q != nil && q.Deployed {
				a.confirm = q.Name
			}
		case 'd':
			if q := a.selected(); q != nil && q.DLQ != "" {
				a.openQueue(q.DLQ)
			}
		case 'y':
			a.copySelection()
		case 's':
			if a.view == viewQueues {
				a.sortBy((a.sortColumn + 1) % len(a.columns()))
			}
		case 'S':
			if a.view == viewQueues && a.sortColumn >= 0 {
				a.sortDescending = !a.sortDescending
			}
		case '1', '2', '3', '4', '5', '6', '7':
			if a.view == viewQueues {
				a.sortBy(int(evt.Rune() - '1'))
			}
		case 'D':
			a.showDLQs = !a.showDLQs
			a.cursor[viewQueues] = 0
		}
	}
	return true
}

// HandleMouse scrolls with the wheel and opens the clicked row.
func (a *App) HandleMouse(evt *tcell.EventMouse) {
	switch {
	case evt.Buttons()&tcell.WheelUp != 0:
		a.move(-3)
	case evt.Buttons()&tcell.WheelDown != 0:
		a.move(3)
	case evt.Buttons()&tcell.ButtonPrimary != 0:
		x, y := evt.Position()
		if a.view == viewQueues && y == a.headerY {
			a.sortBy(a.headerColumn(x, a.width))
			return
		}
		row := y - a.bodyTop
		if row < 0 || row >= a.rows() {
			return
		}
		if a.cursor[a.view] == row+a.offset() {
			a.open()
			return
		}
		a.cursor[a.view] = row + a.offset()
	}
}

func (a *App) open() {
	switch a.view {
	case viewQueues:
		if q := a.selected(); q != nil {
			a.openQueue(q.Name)
		}
	case viewMessages:
		q := a.store.Queue(a.queue)
		if q == nil {
			return
		}
		recent := q.Recent()
		if len(recent) == 0 {
			return
		}
		id := recent[min(a.cursor[viewMessages], len(recent)-1)].ID
		if a.expanded == id {
			a.expanded = ""
		} else {
			a.expanded = id
		}
	}
}

func (a *App) openQueue(name string) {
	a.queue, a.view, a.expanded = name, viewMessages, ""
	a.cursor[viewMessages] = 0
}

func (a *App) back() {
	switch {
	case a.view == viewMessages && a.expanded != "":
		a.expanded = ""
	case a.view == viewMessages:
		a.view = viewQueues
	default:
		a.filter = ""
	}
}

func (a *App) copySelection() {
	var text, what string
	switch a.view {
	case viewQueues:
		q := a.selected()
		if q == nil || q.URL == "" {
			return
		}
		text, what = q.URL, "queue URL"
	case viewMessages:
		q := a.store.Queue(a.queue)
		if q == nil {
			return
		}
		recent := q.Recent()
		if len(recent) == 0 {
			return
		}
		text, what = prettyBody(recent[min(a.cursor[viewMessages], len(recent)-1)].Body), "message body"
	}
	if err := a.copy(text); err != nil {
		a.flash("copy failed: " + err.Error())
		return
	}
	a.flash("copied " + what)
}

// regionFromArn is the region segment of an ARN.
func regionFromArn(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) >= 4 {
		return parts[3]
	}
	return ""
}

func prettyBody(body string) string {
	var value interface{}
	if json.Unmarshal([]byte(body), &value) != nil {
		return body
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return body
	}
	return string(pretty)
}

// ---- drawing ----

func (a *App) offset() int {
	visible := max(1, a.height-1-a.bodyTop)
	if a.cursor[a.view] >= visible {
		return a.cursor[a.view] - visible + 1
	}
	return 0
}

func (a *App) Draw(screen tcell.Screen) {
	screen.Clear()
	width, height := screen.Size()
	a.height, a.width = height, width
	y := a.drawHeader(screen, width)
	switch a.view {
	case viewQueues:
		a.drawQueues(screen, y, width)
	case viewMessages:
		a.drawMessages(screen, y, width)
	}
	a.drawFooter(screen, height-1)
	screen.Show()
}

func (a *App) drawHeader(screen tcell.Screen, width int) int {
	x := panekit.Text(screen, 0, 0, "≡ Queues", panekit.Title)
	if a.view == viewMessages {
		x = panekit.Text(screen, x, 0, "  › "+a.displayName(a.queue), panekit.Bold)
	}
	if a.view == viewQueues && a.prefix != "" {
		x = panekit.Text(screen, x, 0, "  "+a.prefix+"…", panekit.Dim)
	}
	if !a.store.DepthAt.IsZero() {
		note := "depth " + panekit.Ago(a.now(), a.store.DepthAt)
		panekit.Text(screen, max(x+2, width-utf8.RuneCountInString(note)), 0, note, panekit.Dim)
	}
	y := 1
	if a.confirm != "" {
		panekit.Fill(screen, y, panekit.Banner)
		panekit.Text(screen, 0, y, fmt.Sprintf(" purge every message in %s? y/enter confirm · any key cancel", a.confirm), panekit.Banner)
		y++
	}
	return y + 1
}

func depthText(q *Queue) (string, tcell.Style) {
	switch {
	case q.Depth == nil:
		if q.Deployed {
			return "…", panekit.Dim
		}
		return "not deployed", panekit.Dim
	case q.Depth.Error != "":
		return "depth unavailable", panekit.Dim
	}
	text := fmt.Sprintf("%d waiting · %d in flight", q.Waiting(), q.Depth.InFlight)
	style := panekit.Normal
	if q.Waiting() > 0 {
		style = panekit.Warning
	}
	if q.IsDLQ() && q.Waiting() > 0 {
		style = panekit.Danger
	}
	return text, style
}

func (a *App) drawQueues(screen tcell.Screen, y int, width int) {
	if a.filter != "" || a.editing {
		x := panekit.Text(screen, 0, y, "/ ", panekit.Dim)
		x = panekit.Text(screen, x, y, a.filter, panekit.Normal)
		if a.editing {
			screen.ShowCursor(x, y)
		}
		y++
	} else {
		screen.HideCursor()
	}
	a.drawTable(screen, y, width)
}

func messageDot(m *Message) (string, tcell.Style) {
	switch m.Status {
	case Failed:
		return "✗", panekit.Danger
	case Done:
		return "●", panekit.Success
	default:
		return "◌", panekit.Warning
	}
}

func (a *App) drawMessages(screen tcell.Screen, y int, width int) {
	screen.HideCursor()
	q := a.store.Queue(a.queue)
	if q == nil {
		return
	}
	depth, depthStyle := depthText(q)
	x := panekit.Text(screen, 0, y, depth, depthStyle)
	if q.Fifo {
		x = panekit.Text(screen, x+2, y, "FIFO", panekit.Dim)
	}
	if q.DLQ != "" {
		dlq := fmt.Sprintf("DLQ %s after %d receives", q.DLQ, q.MaxReceive)
		x = panekit.Text(screen, x+2, y, dlq, panekit.Dim)
	}
	if len(q.Sources) > 0 {
		x = panekit.Text(screen, x+2, y, "DLQ of "+strings.Join(q.Sources, ", "), panekit.Dim)
	}
	y++
	if q.Stall != nil {
		panekit.Fill(screen, y, panekit.Banner)
		panekit.Text(screen, 0, y, fmt.Sprintf(" ⚠ stuck: msg %s, attempt %d · %s", panekit.ShortID(q.Stall.MessageID), max(q.Stall.ReceiveCount, q.Stall.FailedAttempts), q.Stall.LastError), panekit.Banner)
		y++
	}
	if len(q.Consumers) > 0 {
		panekit.Text(screen, 0, y, "consumers: "+strings.Join(q.Consumers, ", "), panekit.Dim)
		y++
	}
	y++
	a.bodyTop = y
	recent := q.Recent()
	if len(recent) == 0 {
		panekit.Text(screen, 0, y, "No messages received locally yet.", panekit.Dim)
		return
	}
	a.cursor[viewMessages] = min(a.cursor[viewMessages], len(recent)-1)
	for i := a.offset(); i < len(recent) && y < a.height-1; i++ {
		m := recent[i]
		selected := i == a.cursor[viewMessages]
		pick := func(style tcell.Style) tcell.Style {
			if selected {
				return panekit.Cursor
			}
			return style
		}
		if selected {
			panekit.Fill(screen, y, panekit.Cursor)
		}
		dot, dotStyle := messageDot(m)
		x := panekit.Text(screen, 0, y, dot+" ", pick(dotStyle))
		x = panekit.Text(screen, x, y, m.At.Format("15:04:05")+"  ", pick(panekit.Dim))
		x = panekit.Text(screen, x, y, panekit.ShortID(m.ID)+"  ", pick(panekit.Bold))
		receive := fmt.Sprintf("×%d  ", m.Receive)
		receiveStyle := panekit.Dim
		if m.Receive >= 3 {
			receiveStyle = panekit.Danger
		}
		x = panekit.Text(screen, x, y, receive, pick(receiveStyle))
		if m.GroupID != "" {
			x = panekit.Text(screen, x, y, m.GroupID+"  ", pick(panekit.Dim))
		}
		x = panekit.Text(screen, x, y, m.Consumer, pick(panekit.Normal))
		if m.Error != "" {
			panekit.Text(screen, x+2, y, m.Error, pick(panekit.Danger))
		} else {
			panekit.Text(screen, x+2, y, strings.Join(strings.Fields(m.Body), " "), pick(panekit.Dim))
		}
		y++
		if a.expanded == m.ID {
			for _, line := range strings.Split(prettyBody(m.Body), "\n") {
				if y >= a.height-1 {
					break
				}
				panekit.Text(screen, 4, y, line, panekit.Normal)
				y++
			}
		}
	}
}

func (a *App) drawFooter(screen tcell.Screen, y int) {
	if a.message != "" && a.now().Sub(a.messaged) < 4*time.Second {
		panekit.Text(screen, 0, y, a.message, panekit.Warning)
		return
	}
	var hints []string
	switch {
	case a.editing:
		hints = []string{"enter apply", "esc clear"}
	case a.view == viewQueues:
		dlqs := "D show DLQs"
		if a.showDLQs {
			dlqs = "D hide DLQs"
		}
		hints = []string{"j/k move", "enter messages", "s/1-7 sort", "S flip", "/ filter", dlqs, "p purge", "d DLQ", "y copy URL"}
	default:
		hints = []string{"j/k move", "enter body", "y copy body", "p purge", "d DLQ", "esc back"}
	}
	panekit.Text(screen, 0, y, strings.Join(hints, " · "), panekit.Dim)
}
