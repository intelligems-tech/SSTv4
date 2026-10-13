package crons

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

// Runner asks the dev server to run a cron by key.
type Runner func(key string)

// Copier puts text on the clipboard.
type Copier func(text string) error

type view int

const (
	viewCrons view = iota
	viewRuns
)

// Column widths of the cron table; Cron takes the rest.
const (
	widthSchedule = 22
	widthTarget   = 30
	widthState    = 8
	widthLast     = 9
	widthResult   = 8
)

var (
	styleRunning = tcell.StyleDefault.Foreground(tcell.ColorAqua)
	styleFailed  = panekit.Danger.Bold(true)
	// styleSelected highlights the cursor row without hiding cell colours.
	styleSelected = tcell.StyleDefault.Background(tcell.NewRGBColor(48, 58, 82))
)

// App is the Crons pane: a table of crons and, for one cron, its runs.
type App struct {
	store *Store
	now   func() time.Time
	run   Runner
	copy  Copier

	view     view
	cron     string
	cursor   map[view]int
	expanded string
	filter   string
	editing  bool
	confirm  string
	message  string
	messaged time.Time
	bodyTop  int
	height   int

	// prefix is the stage prefix cron names share, hidden in the table.
	prefix string
}

func NewApp(store *Store, now func() time.Time, run Runner, copy Copier) *App {
	return &App{store: store, now: now, run: run, copy: copy, cursor: map[view]int{}}
}

// SetStage hides the `<stage>-` prefix cron names carry.
func (a *App) SetStage(stage string) {
	a.prefix = ""
	if stage != "" {
		a.prefix = stage + "-"
	}
}

func (a *App) Apply(evt interface{}) bool {
	changed := a.store.Apply(evt)
	if done, ok := evt.(*aws.CronRunEvent); ok && changed {
		name := done.Key
		if c := a.store.Cron(done.Key); c != nil {
			name = a.displayName(c.Name)
		}
		if run := a.store.run(done.Key, done.RunID); run != nil && run.Status == Failed {
			a.flash(name + " failed: " + run.Error)
		} else {
			a.flash(name + " ran in " + panekit.FormatDuration(done.Duration))
		}
	}
	return changed
}

func (a *App) flash(message string) {
	a.message, a.messaged = message, a.now()
}

// displayName drops the stage prefix.
func (a *App) displayName(name string) string {
	if a.prefix != "" && strings.HasPrefix(name, a.prefix) && len(name) > len(a.prefix) {
		return name[len(a.prefix):]
	}
	return name
}

func (a *App) table() []*Cron {
	rows := []*Cron{}
	for _, c := range a.store.List("") {
		if a.filter == "" || strings.Contains(strings.ToLower(a.displayName(c.Name)), strings.ToLower(a.filter)) {
			rows = append(rows, c)
		}
	}
	return rows
}

// selected is the cron under the cursor, or the open one.
func (a *App) selected() *Cron {
	if a.view == viewRuns {
		return a.store.Cron(a.cron)
	}
	rows := a.table()
	if len(rows) == 0 {
		return nil
	}
	return rows[min(a.cursor[viewCrons], len(rows)-1)]
}

func (a *App) rows() int {
	if a.view == viewCrons {
		return len(a.table())
	}
	if c := a.store.Cron(a.cron); c != nil {
		return len(c.Runs)
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
		a.cursor[viewCrons] = 0
		return true
	}
	if a.confirm != "" {
		key := a.confirm
		a.confirm = ""
		if evt.Rune() == 'y' || evt.Key() == tcell.KeyEnter {
			if c := a.store.Cron(key); c != nil {
				a.flash("running " + a.displayName(c.Name) + "…")
				a.run(key)
			}
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
			if a.view == viewCrons {
				a.editing = true
			}
		case 'r':
			if c := a.selected(); c != nil && c.Deployed {
				a.confirm = c.Key
			}
		case 'y':
			a.copySelection()
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
		_, y := evt.Position()
		row := y - a.bodyTop
		if row < 0 || row+a.offset() >= a.rows() {
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
	case viewCrons:
		if c := a.selected(); c != nil {
			a.cron, a.view, a.expanded = c.Key, viewRuns, ""
			a.cursor[viewRuns] = 0
		}
	case viewRuns:
		run := a.selectedRun()
		if run == nil {
			return
		}
		if a.expanded == run.ID {
			a.expanded = ""
		} else {
			a.expanded = run.ID
		}
	}
}

func (a *App) back() {
	switch {
	case a.view == viewRuns && a.expanded != "":
		a.expanded = ""
	case a.view == viewRuns:
		a.view = viewCrons
	default:
		a.filter = ""
	}
}

func (a *App) selectedRun() *Execution {
	c := a.store.Cron(a.cron)
	if c == nil {
		return nil
	}
	recent := c.Recent()
	if len(recent) == 0 {
		return nil
	}
	return recent[min(a.cursor[viewRuns], len(recent)-1)]
}

// copySelection copies the cron's target ARN, or the selected run's response.
func (a *App) copySelection() {
	var text, what string
	if run := a.selectedRun(); a.view == viewRuns && run != nil {
		text, what = prettyBody(run.Output), "response"
	} else if c := a.selected(); c != nil {
		text, what = c.TargetArn, "target ARN"
	} else {
		return
	}
	if err := a.copy(text); err != nil {
		a.flash("copy failed: " + err.Error())
		return
	}
	a.flash("copied " + what)
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

// errorMessage is the errorMessage of a Lambda error payload.
func errorMessage(output string) string {
	var payload struct {
		ErrorMessage string `json:"errorMessage"`
	}
	json.Unmarshal([]byte(output), &payload)
	return payload.ErrorMessage
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
	a.height = height
	y := a.drawHeader(screen)
	switch a.view {
	case viewCrons:
		a.drawCrons(screen, y, width)
	case viewRuns:
		a.drawRuns(screen, y)
	}
	a.drawFooter(screen, height-1)
	screen.Show()
}

func (a *App) drawHeader(screen tcell.Screen) int {
	x := panekit.Text(screen, 0, 0, "⏱ Crons", panekit.Title)
	if c := a.store.Cron(a.cron); a.view == viewRuns && c != nil {
		panekit.Text(screen, x, 0, "  › "+a.displayName(c.Name), panekit.Bold)
	}
	if a.view == viewCrons && a.prefix != "" {
		panekit.Text(screen, x, 0, "  "+a.prefix+"…", panekit.Dim)
	}
	y := 1
	if a.confirm != "" {
		name := a.confirm
		if c := a.store.Cron(a.confirm); c != nil {
			name = fmt.Sprintf("%s (%s)", a.displayName(c.Name), c.FunctionName())
		}
		panekit.Fill(screen, y, panekit.Banner)
		panekit.Text(screen, 0, y, " run "+name+" now? y/enter confirm · any key cancel", panekit.Banner)
		y++
	}
	return y + 1
}

func runDot(run *Execution) (string, tcell.Style) {
	switch {
	case run == nil:
		return "○", panekit.Dim
	case run.Status == Running:
		return "◌", styleRunning
	case run.Status == Failed:
		return "✗", panekit.Danger
	default:
		return "●", panekit.Success
	}
}

func (a *App) result(run *Execution) (string, tcell.Style) {
	switch {
	case run == nil:
		return "–", panekit.Dim
	case run.Status == Running:
		return panekit.FormatDuration(a.now().Sub(run.At)), styleRunning
	case run.Status == Failed:
		return "failed", styleFailed
	default:
		return panekit.FormatDuration(run.Duration), panekit.DurationStyle(run.Duration)
	}
}

func (a *App) drawCrons(screen tcell.Screen, y int, width int) {
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
	widths := []int{0, widthSchedule, widthTarget, widthState, widthLast, widthResult}
	fixed := 0
	for _, w := range widths[1:] {
		fixed += w + 1
	}
	widths[0] = max(12, width-2-fixed)
	draw := func(y int, cells []string, styles []tcell.Style) {
		x := 2
		for i, text := range cells {
			cell(screen, x, y, widths[i], text, styles[i], i >= 4)
			x += widths[i] + 1
		}
	}
	header := panekit.Dim.Bold(true)
	draw(y, []string{"Cron", "Schedule", "Target", "State", "Last", "Result"}, []tcell.Style{header, header, header, header, header, header})
	y++
	a.bodyTop = y
	rows := a.table()
	if len(rows) == 0 {
		panekit.Text(screen, 2, y, "No crons yet. They appear after the first deploy.", panekit.Dim)
		return
	}
	a.cursor[viewCrons] = min(a.cursor[viewCrons], len(rows)-1)
	for i := a.offset(); i < len(rows) && y < a.height-1; i++ {
		c := rows[i]
		selected := i == a.cursor[viewCrons]
		if selected {
			panekit.Fill(screen, y, styleSelected)
		}
		last := c.Last()
		dot, dotStyle := runDot(last)
		state, stateStyle := "enabled", panekit.Success
		if !c.Enabled {
			state, stateStyle = "disabled", panekit.Warning
		}
		lastText, lastStyle := "–", panekit.Dim
		if last != nil {
			lastText, lastStyle = panekit.Ago(a.now(), last.At), panekit.Normal
		}
		resultText, resultStyle := a.result(last)
		cells := []string{a.displayName(c.Name), c.Schedule, c.FunctionName(), state, lastText, resultText}
		styles := []tcell.Style{panekit.Bold, panekit.Normal, panekit.Dim, stateStyle, lastStyle, resultStyle}
		if selected {
			dotStyle = withBackground(dotStyle)
			for i := range styles {
				styles[i] = withBackground(styles[i])
			}
		}
		panekit.Text(screen, 0, y, dot, dotStyle)
		draw(y, cells, styles)
		y++
	}
}

func (a *App) drawRuns(screen tcell.Screen, y int) {
	screen.HideCursor()
	c := a.store.Cron(a.cron)
	if c == nil {
		panekit.Text(screen, 0, y, "This cron is no longer deployed.", panekit.Dim)
		return
	}
	x := panekit.Text(screen, 0, y, c.Schedule, panekit.Bold)
	if !c.Enabled {
		x = panekit.Text(screen, x+2, y, "disabled", panekit.Warning)
	}
	panekit.Text(screen, x+2, y, "→ "+c.TargetArn, panekit.Dim)
	y++
	panekit.Text(screen, 0, y, "r sends the test event "+aws.TestPayload+", not the scheduled payload", panekit.Dim)
	y += 2
	a.bodyTop = y
	recent := c.Recent()
	if len(recent) == 0 {
		panekit.Text(screen, 0, y, "Not run from here yet. r runs it now.", panekit.Dim)
		return
	}
	a.cursor[viewRuns] = min(a.cursor[viewRuns], len(recent)-1)
	for i := a.offset(); i < len(recent) && y < a.height-1; i++ {
		run := recent[i]
		selected := i == a.cursor[viewRuns]
		pick := func(style tcell.Style) tcell.Style {
			if selected {
				return withBackground(style)
			}
			return style
		}
		if selected {
			panekit.Fill(screen, y, styleSelected)
		}
		dot, dotStyle := runDot(run)
		x := panekit.Text(screen, 0, y, dot+" ", pick(dotStyle))
		x = panekit.Text(screen, x, y, run.At.Format("15:04:05")+"  ", pick(panekit.Dim))
		result, resultStyle := a.result(run)
		x = panekit.Text(screen, x, y, fmt.Sprintf("%-8s", result), pick(resultStyle))
		if run.RequestID != "" {
			x = panekit.Text(screen, x, y, panekit.ShortID(run.RequestID)+"  ", pick(panekit.Dim))
		}
		if run.Error != "" {
			panekit.Text(screen, x, y, run.Error, pick(panekit.Danger))
		} else {
			panekit.Text(screen, x, y, strings.Join(strings.Fields(run.Output), " "), pick(panekit.Dim))
		}
		y++
		if a.expanded != run.ID {
			continue
		}
		for _, section := range []struct{ title, body string }{{"payload", run.Payload}, {"response", run.Output}} {
			if section.body == "" || y >= a.height-1 {
				continue
			}
			panekit.Text(screen, 4, y, section.title, panekit.Dim.Bold(true))
			y++
			for _, line := range strings.Split(prettyBody(section.body), "\n") {
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
	case a.view == viewCrons:
		hints = []string{"j/k move", "r run now", "enter runs", "/ filter", "y copy ARN"}
	default:
		hints = []string{"j/k move", "r run now", "enter expand", "y copy response", "esc back"}
	}
	panekit.Text(screen, 0, y, strings.Join(hints, " · "), panekit.Dim)
}

// withBackground puts a cell on the cursor row's background.
func withBackground(style tcell.Style) tcell.Style {
	_, background, _ := styleSelected.Decompose()
	return style.Background(background)
}

// cell draws text in a column, truncated with an ellipsis, right aligned
// when asked.
func cell(screen tcell.Screen, x int, y int, width int, text string, style tcell.Style, right bool) {
	if width <= 0 {
		return
	}
	if utf8.RuneCountInString(text) > width {
		runes := []rune(text)
		text = string(runes[:max(0, width-1)]) + "…"
	}
	if right {
		x += width - utf8.RuneCountInString(text)
	}
	panekit.Text(screen, x, y, text, style)
}
