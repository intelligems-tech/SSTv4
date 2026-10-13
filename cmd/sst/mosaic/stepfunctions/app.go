package stepfunctions

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

// Fetcher asks the dev server for a state machine's recent executions.
type Fetcher func(arn string)

// Replayer asks the dev server to replay a state or an execution.
type Replayer func(req aws.StepFunctionReplayRequest)

// Copier puts text on the clipboard.
type Copier func(text string) error

type view int

const (
	viewMachines view = iota
	viewGraph
	viewState
)

// How often executions are read: the open machine often, the list rarely.
const (
	refreshOpen = 5 * time.Second
	refreshList = 30 * time.Second
)

// Column widths of the machine table; Name takes the rest.
const (
	widthType   = 9
	widthStates = 7
	widthLast   = 9
	widthStatus = 10
)

var (
	styleRunning = tcell.StyleDefault.Foreground(tcell.ColorAqua)
	styleCaught  = tcell.StyleDefault.Foreground(tcell.ColorOrange)
	styleFailed  = panekit.Danger.Bold(true)
	// stylePath draws the routes into the selected state.
	stylePath = tcell.StyleDefault.Foreground(tcell.NewRGBColor(199, 146, 234)).Bold(true)
	// styleSelected highlights the cursor row without hiding cell colours.
	styleSelected = tcell.StyleDefault.Background(tcell.NewRGBColor(48, 58, 82))
)

type pending struct {
	req  aws.StepFunctionReplayRequest
	text string
}

// App is the Step Functions pane: the state machines, one machine's graph,
// and one state's visits and replays.
type App struct {
	store  *Store
	now    func() time.Time
	fetch  Fetcher
	replay Replayer
	copy   Copier

	view    view
	machine string
	// state is the selected state of the graph.
	state string
	// execution colours the graph; "" follows the latest.
	execution string
	cursor    map[view]int
	expanded  string
	confirm   *pending
	message   string
	messaged  time.Time
	bodyTop   int
	height    int

	// The graph's viewport: scroll is the canvas cell at the top left, and
	// origin the screen cell the canvas starts at.
	scrollX, scrollY int
	originX, originY int
	follow           bool
	// zoom indexes Zooms; viewW and viewH are the graph's last viewport.
	zoom         int
	viewW, viewH int

	fetched     map[string]time.Time
	listFetched time.Time

	// prefix is the stage prefix names share, hidden in the pane.
	prefix string
}

func NewApp(store *Store, now func() time.Time, fetch Fetcher, replay Replayer, copy Copier) *App {
	return &App{store: store, now: now, fetch: fetch, replay: replay, copy: copy, cursor: map[view]int{}, fetched: map[string]time.Time{}, zoom: DefaultZoom}
}

// SetStage hides the `<stage>-` prefix names carry.
func (a *App) SetStage(stage string) {
	a.prefix = ""
	if stage != "" {
		a.prefix = stage + "-"
	}
}

func (a *App) displayName(name string) string {
	if a.prefix != "" && strings.HasPrefix(name, a.prefix) && len(name) > len(a.prefix) {
		return name[len(a.prefix):]
	}
	return name
}

func (a *App) Apply(evt interface{}) bool {
	changed := a.store.Apply(evt)
	if done, ok := evt.(*aws.StepFunctionReplayEvent); ok && changed {
		a.flashReplay(done)
	}
	return changed
}

func (a *App) flashReplay(done *aws.StepFunctionReplayEvent) {
	r := a.store.replay(done.ID)
	if r == nil {
		a.flash("replay failed: " + done.Error)
		return
	}
	what := "execution replay"
	if r.Mode == aws.ReplayState {
		what = r.State
	}
	if done.Error != "" {
		a.flash(fmt.Sprintf("%s %s: %s", what, strings.ToLower(done.Status), done.Error))
		return
	}
	a.flash(fmt.Sprintf("%s %s in %s", what, strings.ToLower(done.Status), panekit.FormatDuration(done.Duration)))
}

func (a *App) flash(message string) {
	a.message, a.messaged = message, a.now()
}

// Tick reads executions: the open machine every few seconds, every machine
// now and then while the list shows.
func (a *App) Tick() bool {
	now := a.now()
	if a.view == viewMachines {
		machines := a.store.List()
		if len(machines) > 0 && now.Sub(a.listFetched) >= refreshList {
			a.listFetched = now
			for _, m := range machines {
				a.fetched[m.Arn] = now
				a.fetch(m.Arn)
			}
		}
		return false
	}
	if a.store.Machine(a.machine) != nil && now.Sub(a.fetched[a.machine]) >= refreshOpen {
		a.fetched[a.machine] = now
		a.fetch(a.machine)
	}
	return false
}

func (a *App) refresh() {
	a.fetched[a.machine] = time.Time{}
	a.Tick()
}

// ---- selection ----

func (a *App) selectedMachine() *Machine {
	if a.view != viewMachines {
		return a.store.Machine(a.machine)
	}
	rows := a.store.List()
	if len(rows) == 0 {
		return nil
	}
	return rows[min(a.cursor[viewMachines], len(rows)-1)]
}

// shownExecution is the execution the graph is coloured by, and its index.
func (a *App) shownExecution(m *Machine) (*aws.StepFunctionExecution, int) {
	if m == nil || len(m.Executions) == 0 {
		return nil, -1
	}
	if a.execution != "" {
		for i := range m.Executions {
			if m.Executions[i].Arn == a.execution {
				return &m.Executions[i], i
			}
		}
	}
	return &m.Executions[0], 0
}

func (a *App) stepExecution(delta int) {
	m := a.store.Machine(a.machine)
	_, index := a.shownExecution(m)
	if index < 0 {
		return
	}
	next := min(max(0, index+delta), len(m.Executions)-1)
	a.execution = m.Executions[next].Arn
	if next == 0 {
		a.execution = ""
	}
}

// graph is a machine's graph at the current zoom, or nil.
func (a *App) graph(m *Machine) *Graph {
	if m == nil {
		return nil
	}
	return m.Graph(a.zoom)
}

// setZoom changes the zoom and keeps the selected state in view.
func (a *App) setZoom(zoom int) {
	zoom = min(max(0, zoom), len(Zooms)-1)
	if zoom != a.zoom {
		a.zoom, a.follow = zoom, true
	}
}

// zoomToFit picks the most zoomed-in level whose graph fits the viewport,
// or the overview when none does.
func (a *App) zoomToFit() {
	m := a.store.Machine(a.machine)
	if m == nil || m.Definition == nil {
		return
	}
	for zoom := len(Zooms) - 1; zoom >= 0; zoom-- {
		if g := m.Graph(zoom); zoom == 0 || g.Width <= a.viewW && g.Height <= a.viewH {
			a.setZoom(zoom)
			return
		}
	}
}

func (a *App) selectedNode(m *Machine) *Node {
	g := a.graph(m)
	if g == nil {
		return nil
	}
	if n := g.Node(a.state); n != nil {
		return n
	}
	if nodes := g.Selectable(); len(nodes) > 0 {
		a.state = nodes[0].State.Name
		return nodes[0]
	}
	return nil
}

func (a *App) selectNode(n *Node) {
	if n != nil && n.State != nil {
		a.state, a.follow = n.State.Name, true
	}
}

// row is a line of the state view: a replay or a visit.
type row struct {
	replay *Replay
	visit  *Visit
}

func (r row) key() string {
	if r.replay != nil {
		return "replay/" + r.replay.ID
	}
	return fmt.Sprintf("visit/%s/%d", r.visit.Execution.Arn, r.visit.ID)
}

func (a *App) stateRows() []row {
	rows := []row{}
	for _, r := range a.store.Replays(a.machine, a.state) {
		rows = append(rows, row{replay: r})
	}
	for _, v := range a.store.Visits(a.machine, a.state) {
		rows = append(rows, row{visit: &v})
	}
	return rows
}

func (a *App) selectedRow() *row {
	rows := a.stateRows()
	if len(rows) == 0 {
		return nil
	}
	r := rows[min(a.cursor[viewState], len(rows)-1)]
	return &r
}

func (a *App) rows() int {
	switch a.view {
	case viewMachines:
		return len(a.store.List())
	case viewState:
		return len(a.stateRows())
	}
	return 0
}

func (a *App) move(delta int) {
	a.cursor[a.view] = min(max(0, a.cursor[a.view]+delta), max(0, a.rows()-1))
}

// ---- input ----

// HandleKey applies one key press; false exits the pane.
func (a *App) HandleKey(evt *tcell.EventKey) bool {
	if evt.Key() == tcell.KeyCtrlC {
		return false
	}
	if a.confirm != nil {
		confirm := a.confirm
		a.confirm = nil
		if evt.Rune() == 'y' || evt.Key() == tcell.KeyEnter {
			a.flash("replaying…")
			a.replay(confirm.req)
			if confirm.req.Mode == aws.ReplayExecution {
				a.execution = ""
			}
		}
		return true
	}
	if a.view == viewGraph {
		a.graphKey(evt)
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
		case 'f':
			if a.view == viewState {
				a.refresh()
			}
		case 'r':
			a.askReplayState()
		case 'R':
			a.askReplayExecution()
		case 'y':
			a.copySelection()
		}
	}
	return true
}

func (a *App) graphKey(evt *tcell.EventKey) {
	m := a.store.Machine(a.machine)
	current := a.selectedNode(m)
	if current == nil {
		if evt.Key() == tcell.KeyEscape || evt.Key() == tcell.KeyBackspace || evt.Key() == tcell.KeyBackspace2 {
			a.back()
		}
		return
	}
	step := func(dx int, dy int) {
		if n := a.graph(m).Neighbour(current, dx, dy); n != nil {
			a.selectNode(n)
		}
	}
	cycle := func(delta int) {
		nodes := a.graph(m).Selectable()
		for i, n := range nodes {
			if n == current {
				a.selectNode(nodes[(i+delta+len(nodes))%len(nodes)])
				return
			}
		}
	}
	switch evt.Key() {
	case tcell.KeyUp:
		step(0, -1)
	case tcell.KeyDown:
		step(0, 1)
	case tcell.KeyLeft:
		step(-1, 0)
	case tcell.KeyRight:
		step(1, 0)
	case tcell.KeyTab:
		cycle(1)
	case tcell.KeyBacktab:
		cycle(-1)
	case tcell.KeyEnter:
		a.open()
	case tcell.KeyEscape, tcell.KeyBackspace, tcell.KeyBackspace2:
		a.back()
	case tcell.KeyRune:
		switch evt.Rune() {
		case 'k':
			step(0, -1)
		case 'j':
			step(0, 1)
		case 'h':
			step(-1, 0)
		case 'l':
			step(1, 0)
		case '[':
			a.stepExecution(1)
		case ']':
			a.stepExecution(-1)
		case 'f':
			a.refresh()
		case 'R':
			a.askReplayExecution()
		case '+', '=':
			a.setZoom(a.zoom + 1)
		case '-', '_':
			a.setZoom(a.zoom - 1)
		case '0':
			a.zoomToFit()
		}
	}
}

// HandleMouse scrolls with the wheel and opens the clicked row or node.
func (a *App) HandleMouse(evt *tcell.EventMouse) {
	x, y := evt.Position()
	wheel := 0
	switch {
	case evt.Buttons()&tcell.WheelUp != 0:
		wheel = -3
	case evt.Buttons()&tcell.WheelDown != 0:
		wheel = 3
	}
	if a.view == viewGraph {
		switch {
		case wheel != 0 && evt.Modifiers()&tcell.ModCtrl != 0:
			// Ctrl+wheel zooms; wheel up zooms in.
			a.setZoom(a.zoom - wheel/3)
			return
		case wheel != 0:
			a.scrollY += wheel
			return
		}
		if evt.Buttons()&tcell.ButtonPrimary == 0 {
			return
		}
		g := a.graph(a.store.Machine(a.machine))
		if g == nil {
			return
		}
		n := g.At(x-a.originX+a.scrollX, y-a.originY+a.scrollY)
		if n == nil {
			return
		}
		if n.State.Name == a.state {
			a.open()
			return
		}
		a.state = n.State.Name
		return
	}
	if wheel != 0 {
		a.move(wheel)
		return
	}
	if evt.Buttons()&tcell.ButtonPrimary == 0 {
		return
	}
	line := y - a.bodyTop
	index := a.lineToRow(line)
	if index < 0 {
		return
	}
	if a.cursor[a.view] == index {
		a.open()
		return
	}
	a.cursor[a.view] = index
}

// lineToRow maps a body line to a row, skipping the lines of an expanded
// row; -1 is no row.
func (a *App) lineToRow(line int) int {
	if line < 0 {
		return -1
	}
	if a.view != viewState || a.expanded == "" {
		if line+a.offset() >= a.rows() {
			return -1
		}
		return line + a.offset()
	}
	rows := a.stateRows()
	l := 0
	for i := a.offset(); i < len(rows); i++ {
		height := 1
		if rows[i].key() == a.expanded {
			height += len(a.details(rows[i]))
		}
		if line < l+height {
			return i
		}
		l += height
	}
	return -1
}

func (a *App) open() {
	switch a.view {
	case viewMachines:
		m := a.selectedMachine()
		if m == nil {
			return
		}
		a.machine, a.view, a.execution, a.state = m.Arn, viewGraph, "", ""
		a.scrollX, a.scrollY, a.follow = 0, 0, true
		a.refresh()
	case viewGraph:
		if n := a.selectedNode(a.store.Machine(a.machine)); n != nil {
			a.view, a.expanded = viewState, ""
			a.cursor[viewState] = 0
		}
	case viewState:
		r := a.selectedRow()
		if r == nil {
			return
		}
		if a.expanded == r.key() {
			a.expanded = ""
		} else {
			a.expanded = r.key()
		}
	}
}

func (a *App) back() {
	switch {
	case a.view == viewState && a.expanded != "":
		a.expanded = ""
	case a.view == viewState:
		a.view, a.follow = viewGraph, true
	case a.view == viewGraph:
		a.view = viewMachines
	}
}

func (a *App) askReplayState() {
	if a.view != viewState {
		return
	}
	m := a.store.Machine(a.machine)
	r := a.selectedRow()
	if m == nil || r == nil || r.visit == nil {
		return
	}
	if state := m.Definition.Find(a.state); state == nil || !state.Testable() {
		a.flash("TestState cannot run this state alone; R replays the whole execution")
		return
	}
	a.confirm = &pending{
		req:  aws.StepFunctionReplayRequest{StateMachineArn: m.Arn, ExecutionArn: r.visit.Execution.Arn, Mode: aws.ReplayState, Visit: r.visit.ID},
		text: fmt.Sprintf("replay %s alone with its input from %s?", a.state, r.visit.Execution.Name),
	}
}

func (a *App) askReplayExecution() {
	m := a.store.Machine(a.machine)
	if m == nil {
		return
	}
	var execution *aws.StepFunctionExecution
	switch a.view {
	case viewGraph:
		execution, _ = a.shownExecution(m)
	case viewState:
		if r := a.selectedRow(); r != nil && r.visit != nil {
			execution = r.visit.Execution
		}
	}
	if execution == nil {
		return
	}
	a.confirm = &pending{
		req:  aws.StepFunctionReplayRequest{StateMachineArn: m.Arn, ExecutionArn: execution.Arn, Mode: aws.ReplayExecution},
		text: fmt.Sprintf("start a new execution of %s with the input of %s?", a.displayName(m.Name), execution.Name),
	}
}

// copySelection copies a visit's input or a replay's output.
func (a *App) copySelection() {
	var text, what string
	switch a.view {
	case viewMachines:
		if m := a.selectedMachine(); m != nil {
			text, what = m.Arn, "state machine ARN"
		}
	case viewState:
		r := a.selectedRow()
		switch {
		case r == nil:
		case r.visit != nil:
			text, what = prettyBody(r.visit.Input), "input"
		default:
			text, what = prettyBody(r.replay.Result.Output), "output"
		}
	}
	if what == "" {
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
	screen.HideCursor()
	width, height := screen.Size()
	a.height = height
	y := a.drawHeader(screen)
	switch a.view {
	case viewMachines:
		a.drawMachines(screen, y, width)
	case viewGraph:
		a.drawGraph(screen, y, width, height)
	case viewState:
		a.drawState(screen, y)
	}
	a.drawFooter(screen, height-1)
	screen.Show()
}

func (a *App) drawHeader(screen tcell.Screen) int {
	x := panekit.Text(screen, 0, 0, "⛓ Step Functions", panekit.Title)
	if m := a.store.Machine(a.machine); a.view != viewMachines && m != nil {
		x = panekit.Text(screen, x, 0, "  › "+a.displayName(m.Name), panekit.Bold)
		if a.view == viewState {
			x = panekit.Text(screen, x, 0, " › "+a.state, panekit.Bold)
		}
	}
	if a.view == viewMachines && a.prefix != "" {
		panekit.Text(screen, x, 0, "  "+a.prefix+"…", panekit.Dim)
	}
	y := 1
	if a.confirm != nil {
		panekit.Fill(screen, y, panekit.Banner)
		panekit.Text(screen, 0, y, " "+a.confirm.text+" y/enter confirm · any key cancel", panekit.Banner)
		y++
	}
	return y + 1
}

func executionStyle(status string) tcell.Style {
	switch status {
	case "SUCCEEDED":
		return panekit.Success
	case "RUNNING", "PENDING_REDRIVE":
		return styleRunning
	case "":
		return panekit.Dim
	}
	return styleFailed
}

func visitStyle(status string) tcell.Style {
	switch status {
	case aws.VisitSucceeded:
		return panekit.Success
	case aws.VisitRunning:
		return styleRunning
	case aws.VisitCaught:
		return styleCaught
	case aws.VisitFailed:
		return styleFailed
	}
	return panekit.Dim
}

func visitDot(status string) string {
	switch status {
	case aws.VisitSucceeded:
		return "●"
	case aws.VisitRunning:
		return "◌"
	case aws.VisitCaught:
		return "◐"
	case aws.VisitFailed:
		return "✗"
	}
	return "○"
}

func (a *App) drawMachines(screen tcell.Screen, y int, width int) {
	widths := []int{0, widthType, widthStates, widthLast, widthStatus}
	fixed := 0
	for _, w := range widths[1:] {
		fixed += w + 1
	}
	widths[0] = max(12, width-2-fixed)
	draw := func(y int, cells []string, styles []tcell.Style) {
		x := 2
		for i, text := range cells {
			cell(screen, x, y, widths[i], text, styles[i], i == 2 || i == 3)
			x += widths[i] + 1
		}
	}
	header := panekit.Dim.Bold(true)
	draw(y, []string{"State machine", "Type", "States", "Last", "Status"}, []tcell.Style{header, header, header, header, header})
	y++
	a.bodyTop = y
	rows := a.store.List()
	if len(rows) == 0 {
		panekit.Text(screen, 2, y, "No state machines yet. They appear after the first deploy.", panekit.Dim)
		return
	}
	a.cursor[viewMachines] = min(a.cursor[viewMachines], len(rows)-1)
	for i := a.offset(); i < len(rows) && y < a.height-1; i++ {
		m := rows[i]
		selected := i == a.cursor[viewMachines]
		if selected {
			panekit.Fill(screen, y, styleSelected)
		}
		states, last, status := "–", "–", "–"
		lastStyle, statusStyle := panekit.Dim, panekit.Dim
		if m.Definition != nil {
			states = fmt.Sprint(m.Definition.Count())
		}
		switch {
		case m.Error != "":
			status, statusStyle = "error", styleFailed
		case len(m.Executions) > 0:
			latest := m.Executions[0]
			last, lastStyle = panekit.Ago(a.now(), latest.Start), panekit.Normal
			status, statusStyle = strings.ToLower(latest.Status), executionStyle(latest.Status)
		case m.Loaded:
			status = "never run"
		}
		cells := []string{a.displayName(m.Name), strings.ToLower(m.Type), states, last, status}
		styles := []tcell.Style{panekit.Bold, panekit.Dim, panekit.Normal, lastStyle, statusStyle}
		dot, dotStyle := "○", panekit.Dim
		if len(m.Executions) > 0 {
			dot, dotStyle = "●", executionStyle(m.Executions[0].Status)
		}
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

func (a *App) drawGraph(screen tcell.Screen, y int, width int, height int) {
	m := a.store.Machine(a.machine)
	if m == nil {
		panekit.Text(screen, 0, y, "This state machine is no longer deployed.", panekit.Dim)
		return
	}
	g := a.graph(m)
	if g == nil {
		panekit.Text(screen, 0, y, "Cannot read the definition: "+m.ParseError, panekit.Danger)
		return
	}
	execution, index := a.shownExecution(m)
	switch {
	case m.Error != "":
		panekit.Text(screen, 0, y, m.Error, panekit.Danger)
	case execution != nil:
		x := panekit.Text(screen, 0, y, fmt.Sprintf("execution %d/%d  ", index+1, len(m.Executions)), panekit.Dim)
		x = panekit.Text(screen, x, y, execution.Name+"  ", panekit.Bold)
		x = panekit.Text(screen, x, y, strings.ToLower(execution.Status), executionStyle(execution.Status))
		x = panekit.Text(screen, x, y, "  "+panekit.Ago(a.now(), execution.Start), panekit.Dim)
		if !execution.Stop.IsZero() {
			x = panekit.Text(screen, x, y, " · "+panekit.FormatDuration(execution.Stop.Sub(execution.Start)), panekit.Dim)
		}
		if execution.Error != "" {
			panekit.Text(screen, x+2, y, execution.Error, panekit.Danger)
		}
	case m.Loaded:
		panekit.Text(screen, 0, y, "No executions yet.", panekit.Dim)
	default:
		panekit.Text(screen, 0, y, "Reading executions…", panekit.Dim)
	}
	y += 2

	selected := a.selectedNode(m)
	path, asRun := a.pathTo(g, selected, execution)
	canvas := g.Draw(func(n *Node, isSelected bool) tcell.Style {
		style := panekit.Normal
		switch {
		case n.Kind != KindState:
			style = panekit.Dim
		case execution != nil:
			style = visitStyle(StateStatus(execution, n.State.Name))
		}
		if isSelected {
			style = withBackground(style).Bold(true)
		}
		return style
	}, selected, panekit.Dim, path, stylePath)

	viewW, viewH := width, max(1, height-2-y)
	a.viewW, a.viewH = viewW, viewH
	a.scrollX, a.originX = a.fit(a.scrollX, canvas.W, viewW, selected, g.Zoom.BoxHeight, true)
	a.scrollY, a.originY = a.fit(a.scrollY, canvas.H, viewH, selected, g.Zoom.BoxHeight, false)
	a.originY += y
	a.follow = false
	for row := 0; row < viewH; row++ {
		for col := 0; col < viewW; col++ {
			r, style := canvas.Cell(col+a.scrollX, row+a.scrollY)
			if r != 0 {
				screen.SetContent(a.originX+col, a.originY+row, r, nil, style)
			}
		}
	}
	if selected != nil {
		x := a.drawNodeInfo(screen, height-2, m, selected)
		note := "  ┃ every route here"
		if asRun {
			note = "  ┃ path as run"
		}
		panekit.Text(screen, x, height-2, note, stylePath)
	}
}

// pathTo is the routes into the selected state: the transitions the shown
// execution took to reach it, or every route when it did not reach it.
func (a *App) pathTo(g *Graph, selected *Node, execution *aws.StepFunctionExecution) (Path, bool) {
	if selected == nil {
		return nil, false
	}
	if execution == nil || StateStatus(execution, selected.State.Name) == "" {
		return g.PathTo(selected, nil), false
	}
	return g.PathTo(selected, func(n *Node) bool {
		switch n.Kind {
		case KindState, KindJoin:
			return StateStatus(execution, n.State.Name) != ""
		case KindEnd:
			return !execution.Running()
		}
		return true
	}), true
}

// fit clamps a scroll offset, follows the selection when asked, and centres
// a drawing smaller than the view. It returns the scroll and the screen
// origin of the drawing on that axis.
func (a *App) fit(scroll int, size int, view int, selected *Node, boxHeight int, horizontal bool) (int, int) {
	if size <= view {
		if horizontal {
			return 0, (view - size) / 2
		}
		return 0, 0
	}
	if a.follow && selected != nil {
		from, to := selected.Y-1, selected.Y+boxHeight+1
		if horizontal {
			from, to = selected.X-2, selected.X+selected.W+2
		}
		if from < scroll {
			scroll = from
		}
		if to > scroll+view {
			scroll = to - view
		}
	}
	return min(max(0, scroll), size-view), 0
}

func (a *App) drawNodeInfo(screen tcell.Screen, y int, m *Machine, n *Node) int {
	state := n.State
	x := panekit.Text(screen, 0, y, typeGlyph(state)+" "+state.Type, panekit.Bold)
	if state.Function != "" {
		x = panekit.Text(screen, x, y, "  "+functionName(state.Function), panekit.Dim)
	} else if state.Resource != "" {
		x = panekit.Text(screen, x, y, "  "+state.Resource, panekit.Dim)
	}
	visits := a.store.Visits(m.Arn, state.Name)
	text := fmt.Sprintf("  · %d visits", len(visits))
	if len(visits) == 1 {
		text = "  · 1 visit"
	}
	x = panekit.Text(screen, x, y, text, panekit.Dim)
	if len(visits) > 0 {
		last := visits[0]
		x = panekit.Text(screen, x, y, " · last ", panekit.Dim)
		x = panekit.Text(screen, x, y, last.Status, visitStyle(last.Status))
		x = panekit.Text(screen, x, y, " "+panekit.Ago(a.now(), last.Entered), panekit.Dim)
	}
	return x
}

// functionName shortens a function ARN to its name and qualifier.
func functionName(function string) string {
	if _, name, ok := strings.Cut(function, ":function:"); ok {
		return name
	}
	return function
}

func (a *App) drawState(screen tcell.Screen, y int) {
	m := a.store.Machine(a.machine)
	if m == nil || m.Definition == nil {
		panekit.Text(screen, 0, y, "This state machine is no longer deployed.", panekit.Dim)
		return
	}
	state := m.Definition.Find(a.state)
	if state == nil {
		panekit.Text(screen, 0, y, "This state is no longer in the definition.", panekit.Dim)
		return
	}
	x := panekit.Text(screen, 0, y, typeGlyph(state)+" "+state.Type, panekit.Bold)
	if state.Function != "" {
		x = panekit.Text(screen, x, y, "  → "+state.Function, panekit.Dim)
	} else if state.Resource != "" {
		x = panekit.Text(screen, x, y, "  → "+state.Resource, panekit.Dim)
	}
	y++
	hint := "r runs this state alone (TestState) with the visit's input · R starts the whole execution again"
	if !state.Testable() {
		hint = "TestState cannot run a " + state.Type + " state alone · R starts the whole execution again"
	}
	panekit.Text(screen, 0, y, hint, panekit.Dim)
	y += 2
	a.bodyTop = y
	rows := a.stateRows()
	if len(rows) == 0 {
		text := "No visits in the last executions."
		if !m.Loaded {
			text = "Reading executions…"
		}
		panekit.Text(screen, 0, y, text, panekit.Dim)
		return
	}
	a.cursor[viewState] = min(a.cursor[viewState], len(rows)-1)
	for i := a.offset(); i < len(rows) && y < a.height-1; i++ {
		r := rows[i]
		selected := i == a.cursor[viewState]
		pick := func(style tcell.Style) tcell.Style {
			if selected {
				return withBackground(style)
			}
			return style
		}
		if selected {
			panekit.Fill(screen, y, styleSelected)
		}
		if r.replay != nil {
			a.drawReplayRow(screen, y, r.replay, pick)
		} else {
			a.drawVisitRow(screen, y, r.visit, pick)
		}
		y++
		if a.expanded != r.key() {
			continue
		}
		for _, line := range a.details(r) {
			if y >= a.height-1 {
				break
			}
			panekit.Text(screen, 4, y, line.text, line.style)
			y++
		}
	}
}

func (a *App) drawVisitRow(screen tcell.Screen, y int, v *Visit, pick func(tcell.Style) tcell.Style) {
	x := panekit.Text(screen, 0, y, visitDot(v.Status)+" ", pick(visitStyle(v.Status)))
	x = panekit.Text(screen, x, y, v.Entered.Local().Format("15:04:05")+"  ", pick(panekit.Dim))
	x = panekit.Text(screen, x, y, fmt.Sprintf("%-10s", v.Status), pick(visitStyle(v.Status)))
	duration := v.Duration(a.now())
	x = panekit.Text(screen, x, y, fmt.Sprintf("%-8s", panekit.FormatDuration(duration)), pick(panekit.DurationStyle(duration)))
	x = panekit.Text(screen, x, y, panekit.ShortID(v.Execution.Name)+"  ", pick(panekit.Dim))
	if v.Retries > 0 {
		x = panekit.Text(screen, x, y, fmt.Sprintf("↻%d  ", v.Retries), pick(panekit.Warning))
	}
	if v.Error != "" && v.Status != aws.VisitSucceeded {
		panekit.Text(screen, x, y, v.Error, pick(panekit.Danger))
		return
	}
	panekit.Text(screen, x, y, oneLine(v.Input), pick(panekit.Normal))
}

func (a *App) drawReplayRow(screen tcell.Screen, y int, r *Replay, pick func(tcell.Style) tcell.Style) {
	status, style, dot := "running", styleRunning, "◌"
	duration := a.now().Sub(r.At)
	if r.Done {
		status, duration = strings.ToLower(r.Result.Status), r.Result.Duration
		style, dot = panekit.Success, "●"
		if r.Result.Error != "" || r.Result.Status == "FAILED" {
			style, dot = styleFailed, "✗"
		} else if r.Result.Status == "CAUGHT_ERROR" || r.Result.Status == "RETRIABLE" {
			style, dot = styleCaught, "◐"
		}
	}
	x := panekit.Text(screen, 0, y, dot+" ", pick(style))
	x = panekit.Text(screen, x, y, r.At.Local().Format("15:04:05")+"  ", pick(panekit.Dim))
	x = panekit.Text(screen, x, y, fmt.Sprintf("%-10s", status), pick(style))
	x = panekit.Text(screen, x, y, fmt.Sprintf("%-8s", panekit.FormatDuration(duration)), pick(panekit.DurationStyle(duration)))
	x = panekit.Text(screen, x, y, "↻ replay  ", pick(panekit.Title))
	switch {
	case r.Result.Error != "":
		panekit.Text(screen, x, y, r.Result.Error, pick(panekit.Danger))
	case r.Result.NextState != "":
		x = panekit.Text(screen, x, y, "→ "+r.Result.NextState+"  ", pick(panekit.Bold))
		panekit.Text(screen, x, y, oneLine(r.Result.Output), pick(panekit.Normal))
	default:
		panekit.Text(screen, x, y, oneLine(r.Result.Output), pick(panekit.Normal))
	}
}

type detailLine struct {
	text  string
	style tcell.Style
}

// details are the lines an expanded row shows.
func (a *App) details(r row) []detailLine {
	type section struct{ title, body string }
	sections := []section{}
	if r.replay != nil {
		sections = append(sections, section{"input", r.replay.Input}, section{"output", r.replay.Result.Output})
		if r.replay.Result.NextState != "" {
			sections = append(sections, section{"next state", r.replay.Result.NextState})
		}
		sections = append(sections, section{"error", strings.TrimSpace(r.replay.Result.Error + "\n" + r.replay.Result.Cause)})
	} else {
		sections = append(sections,
			section{"execution", r.visit.Execution.Name},
			section{"input", r.visit.Input},
			section{"output", r.visit.Output},
			section{"error", strings.TrimSpace(r.visit.Error + "\n" + r.visit.Cause)},
		)
	}
	lines := []detailLine{}
	for _, s := range sections {
		if s.body == "" {
			continue
		}
		lines = append(lines, detailLine{s.title, panekit.Dim.Bold(true)})
		style := panekit.Normal
		if s.title == "error" {
			style = panekit.Danger
		}
		for _, line := range strings.Split(prettyBody(s.body), "\n") {
			lines = append(lines, detailLine{line, style})
		}
	}
	return lines
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func (a *App) drawFooter(screen tcell.Screen, y int) {
	if a.message != "" && a.now().Sub(a.messaged) < 4*time.Second {
		panekit.Text(screen, 0, y, a.message, panekit.Warning)
		return
	}
	var hints []string
	switch a.view {
	case viewMachines:
		hints = []string{"j/k move", "enter graph", "y copy ARN"}
	case viewGraph:
		hints = []string{"arrows/hjkl move", "enter visits", "+/- zoom (" + Zooms[a.zoom].Name + ")", "0 fit", "[ ] execution", "R replay execution", "f refresh", "esc back"}
	default:
		hints = []string{"j/k move", "enter expand", "r replay state", "R replay execution", "y copy", "esc back"}
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
