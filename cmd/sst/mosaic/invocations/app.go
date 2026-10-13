package invocations

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/prettyjson"
)

type view int

const (
	viewGroups view = iota
	viewInvocations
	viewDetail
	viewSearch
)

// Purger purges a queue; the pane posts to the dev server's purge endpoint.
type Purger func(queue string, region string)

// Copier puts text on the clipboard.
type Copier func(text string) error

// App holds the pane's navigation state. Key handling and drawing run on one
// goroutine; events reach it through Apply.
type App struct {
	store *Store
	now   func() time.Time
	purge Purger
	copy  Copier

	view    view
	group   string
	request string
	cursor  map[view]int
	scroll  int
	showIO  bool
	// Pretty JSON mode: decode JSON nested in strings and format JSON logs.
	pretty   bool
	follow   bool
	editing  bool
	filter   map[view]string
	query    string
	matches  []Match
	from     view
	confirm  bool
	dismiss  map[string]string
	message  string
	messaged time.Time
	width    int
	height   int
}

func NewApp(store *Store, now func() time.Time, purge Purger, copy Copier) *App {
	return &App{
		store:   store,
		now:     now,
		purge:   purge,
		copy:    copy,
		cursor:  map[view]int{},
		filter:  map[view]string{},
		dismiss: map[string]string{},
		follow:  true,
		pretty:  prettyjson.Enabled(),
	}
}

// Apply folds a dev event into the store.
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
	a.message = message
	a.messaged = a.now()
}

// stall is the queue stall the banner shows, unless it was dismissed.
func (a *App) stall() *aws.QueueStall {
	for i := range a.store.Stalls {
		stall := &a.store.Stalls[i]
		if a.dismiss[stall.QueueName] == stall.MessageID {
			continue
		}
		return stall
	}
	return nil
}

func (a *App) groups() []*Group {
	return a.store.Groups(a.filter[viewGroups])
}

func (a *App) invocations() []*Invocation {
	group := a.store.Group(a.group)
	if group == nil {
		return nil
	}
	return group.List(a.filter[viewInvocations])
}

func (a *App) invocation() *Invocation {
	group := a.store.Group(a.group)
	if group == nil {
		return nil
	}
	for _, inv := range group.Invocations {
		if inv.RequestID == a.request {
			return inv
		}
	}
	return nil
}

func (a *App) rows() int {
	switch a.view {
	case viewGroups:
		return len(a.groups())
	case viewInvocations:
		return len(a.invocations())
	case viewSearch:
		return len(a.matches)
	}
	return 0
}

func (a *App) bodyHeight() int {
	height := a.height - 3
	if a.stall() != nil {
		height--
	}
	return max(1, height)
}

func (a *App) move(delta int) {
	if a.view == viewDetail {
		a.scroll = max(0, a.scroll+delta)
		a.follow = false
		return
	}
	rows := a.rows()
	a.cursor[a.view] = min(max(0, a.cursor[a.view]+delta), max(0, rows-1))
}

// HandleKey applies one key press. It returns false when the pane should exit.
func (a *App) HandleKey(evt *tcell.EventKey) bool {
	if evt.Key() == tcell.KeyCtrlC {
		return false
	}
	if a.editing {
		a.edit(evt)
		return true
	}
	if a.confirm {
		a.confirm = false
		if evt.Rune() == 'y' || evt.Key() == tcell.KeyEnter {
			if stall := a.stall(); stall != nil {
				a.flash("purging " + stall.QueueName + "…")
				a.purge(stall.QueueName, stall.Region)
			}
		}
		return true
	}
	half := max(1, a.bodyHeight()/2)
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
	case tcell.KeyCtrlF:
		if a.view != viewSearch {
			a.from = a.view
		}
		a.view = viewSearch
		a.editing = true
		a.matches = a.store.Search(a.query)
	case tcell.KeyRune:
		a.rune(evt.Rune())
	}
	return true
}

func (a *App) rune(r rune) {
	switch r {
	case 'j':
		a.move(1)
	case 'k':
		a.move(-1)
	case 'g':
		a.cursor[a.view] = 0
		a.scroll = 0
		a.follow = false
	case 'G':
		a.cursor[a.view] = max(0, a.rows()-1)
		a.follow = true
	case 'l':
		a.open()
	case 'h':
		a.back()
	case '/':
		if a.view == viewGroups || a.view == viewInvocations {
			a.editing = true
		}
	case 'f':
		a.follow = !a.follow
	case 'J':
		a.pretty = !a.pretty
		if a.pretty {
			a.flash("pretty JSON on")
		} else {
			a.flash("pretty JSON off")
		}
	case 'i':
		if a.view == viewDetail {
			a.showIO = !a.showIO
			a.scroll = 0
		}
	case 'y', 'Y', 'c':
		a.copySelection(r)
	case 'p':
		if a.stall() != nil {
			a.confirm = true
		}
	case 'x':
		if stall := a.stall(); stall != nil {
			a.dismiss[stall.QueueName] = stall.MessageID
		}
	}
}

func (a *App) edit(evt *tcell.EventKey) {
	target := &a.query
	if a.view != viewSearch {
		value := a.filter[a.view]
		target = &value
		defer func() {
			a.filter[a.view] = value
			a.cursor[a.view] = 0
		}()
	}
	switch evt.Key() {
	case tcell.KeyEnter:
		a.editing = false
		if a.view == viewSearch {
			a.cursor[viewSearch] = 0
		}
	case tcell.KeyEscape:
		a.editing = false
		*target = ""
		if a.view == viewSearch {
			a.view = a.from
		}
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if *target != "" {
			_, size := utf8.DecodeLastRuneInString(*target)
			*target = (*target)[:len(*target)-size]
		}
	case tcell.KeyRune:
		*target += string(evt.Rune())
	}
	if a.view == viewSearch {
		a.matches = a.store.Search(a.query)
	}
}

func (a *App) open() {
	switch a.view {
	case viewGroups:
		groups := a.groups()
		if len(groups) == 0 {
			return
		}
		a.group = groups[a.cursor[viewGroups]].Key
		a.cursor[viewInvocations] = 0
		a.filter[viewInvocations] = ""
		a.view = viewInvocations
	case viewInvocations:
		list := a.invocations()
		if len(list) == 0 {
			return
		}
		a.request = list[a.cursor[viewInvocations]].RequestID
		a.view, a.scroll, a.showIO, a.follow = viewDetail, 0, false, false
	case viewSearch:
		if len(a.matches) == 0 {
			return
		}
		match := a.matches[a.cursor[viewSearch]]
		a.group, a.request = match.Group.Key, match.Invocation.RequestID
		a.view, a.showIO, a.follow = viewDetail, false, false
		a.scroll = max(0, match.Line)
	}
}

func (a *App) back() {
	switch a.view {
	case viewInvocations:
		if a.filter[viewInvocations] != "" {
			a.filter[viewInvocations] = ""
			return
		}
		a.view = viewGroups
	case viewDetail:
		a.view = viewInvocations
	case viewSearch:
		a.view = a.from
	case viewGroups:
		a.filter[viewGroups] = ""
	}
}

func (a *App) copySelection(r rune) {
	var text, what string
	switch a.view {
	case viewDetail:
		inv := a.invocation()
		if inv == nil {
			return
		}
		switch r {
		case 'y':
			text, what = a.json(inv.Input), "input"
		case 'Y':
			text, what = a.json(inv.Output), "output"
		case 'c':
			text, what = strings.Join(a.logLines(inv), "\n"), "logs"
		}
	case viewInvocations:
		list := a.invocations()
		if len(list) == 0 || r != 'y' {
			return
		}
		text, what = a.json(list[a.cursor[viewInvocations]].Input), "input"
	default:
		return
	}
	if err := a.copy(text); err != nil {
		a.flash("copy failed: " + err.Error())
		return
	}
	a.flash("copied " + what)
}

// ---- drawing ----

var (
	styleNormal  = tcell.StyleDefault
	styleDim     = tcell.StyleDefault.Foreground(tcell.ColorGray)
	styleBold    = tcell.StyleDefault.Bold(true)
	styleTitle   = tcell.StyleDefault.Foreground(tcell.ColorTeal).Bold(true)
	styleCursor  = tcell.StyleDefault.Reverse(true)
	styleDanger  = tcell.StyleDefault.Foreground(tcell.ColorRed)
	styleWarning = tcell.StyleDefault.Foreground(tcell.ColorYellow)
	styleSuccess = tcell.StyleDefault.Foreground(tcell.ColorGreen)
	styleBanner  = tcell.StyleDefault.Background(tcell.ColorDarkRed).Foreground(tcell.ColorWhite).Bold(true)
)

func durationStyle(d time.Duration) tcell.Style {
	switch {
	case d < time.Second:
		return styleSuccess
	case d < 3*time.Second:
		return styleWarning
	default:
		return styleDanger
	}
}

func statusDot(inv *Invocation) (string, tcell.Style) {
	switch inv.Status {
	case Failed:
		return "✗", styleDanger
	case Succeeded:
		return "●", styleSuccess
	default:
		return "◌", styleWarning
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func ago(now time.Time, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// Draw renders the pane.
func (a *App) Draw(screen tcell.Screen) {
	screen.Clear()
	a.width, a.height = screen.Size()
	y := 0
	a.drawHeader(screen, y)
	y++
	if stall := a.stall(); stall != nil {
		a.drawBanner(screen, y, stall)
		y++
	}
	y++
	switch a.view {
	case viewGroups:
		a.drawGroups(screen, y)
	case viewInvocations:
		a.drawInvocations(screen, y)
	case viewDetail:
		a.drawDetail(screen, y)
	case viewSearch:
		a.drawSearch(screen, y)
	}
	a.drawFooter(screen, a.height-1)
	screen.Show()
}

func (a *App) text(screen tcell.Screen, x int, y int, text string, style tcell.Style) int {
	for _, r := range text {
		if x >= a.width {
			break
		}
		screen.SetContent(x, y, r, nil, style)
		x++
	}
	return x
}

func (a *App) fill(screen tcell.Screen, y int, style tcell.Style) {
	for x := 0; x < a.width; x++ {
		screen.SetContent(x, y, ' ', nil, style)
	}
}

func (a *App) drawHeader(screen tcell.Screen, y int) {
	x := a.text(screen, 0, y, "◎ Invocations", styleTitle)
	switch a.view {
	case viewInvocations, viewDetail:
		if group := a.store.Group(a.group); group != nil {
			x = a.text(screen, x, y, "  › "+group.Title, styleBold)
		}
	case viewSearch:
		x = a.text(screen, x, y, "  › search", styleBold)
	}
	summary := fmt.Sprintf("Σ %d inv · %d err", a.store.Total, a.store.Errors)
	a.text(screen, max(x+2, a.width-utf8.RuneCountInString(summary)), y, summary, styleDim)
}

func (a *App) drawBanner(screen tcell.Screen, y int, stall *aws.QueueStall) {
	a.fill(screen, y, styleBanner)
	text := fmt.Sprintf(" ⚠ queue stuck: %s — msg %s, attempt %d", stall.QueueName, shortID(stall.MessageID), max(stall.ReceiveCount, stall.FailedAttempts))
	if stall.LastError != "" {
		text += " · " + stall.LastError
	}
	if a.confirm {
		text = fmt.Sprintf(" purge every message in %s? y/enter confirm · any key cancel", stall.QueueName)
	} else {
		text += " · p purge · x dismiss"
	}
	a.text(screen, 0, y, text, styleBanner)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (a *App) window(rows int) (int, int) {
	height := a.bodyHeight() - 1
	cursor := a.cursor[a.view]
	start := 0
	if cursor >= height {
		start = cursor - height + 1
	}
	return start, min(rows, start+height)
}

func (a *App) drawFilter(screen tcell.Screen, y int) int {
	filter := a.filter[a.view]
	if filter == "" && !a.editing {
		return y
	}
	x := a.text(screen, 0, y, "/ ", styleDim)
	x = a.text(screen, x, y, filter, styleNormal)
	if a.editing {
		screen.ShowCursor(x, y)
	}
	return y + 1
}

func (a *App) drawGroups(screen tcell.Screen, y int) {
	screen.HideCursor()
	y = a.drawFilter(screen, y)
	groups := a.groups()
	if len(groups) == 0 {
		a.text(screen, 0, y, "No invocations yet. Trigger a function to see it here.", styleDim)
		return
	}
	a.cursor[viewGroups] = min(a.cursor[viewGroups], len(groups)-1)
	start, end := a.window(len(groups))
	now := a.now()
	for i := start; i < end; i++ {
		group := groups[i]
		style := styleNormal
		if i == a.cursor[viewGroups] {
			a.fill(screen, y, styleCursor)
			style = styleCursor
		}
		dot, dotStyle := "○", styleDim
		if group.Running > 0 {
			dot, dotStyle = "●", styleWarning
		}
		if i == a.cursor[viewGroups] {
			dotStyle = styleCursor
		}
		x := a.text(screen, 0, y, dot+" ", dotStyle)
		x = a.text(screen, x, y, group.Title, style.Bold(true))
		if group.Subtitle != "" {
			x = a.text(screen, x, y, "  "+group.Subtitle, style.Dim(true))
		}
		stats := fmt.Sprintf("%d inv", group.Total)
		if group.Running > 0 {
			stats += fmt.Sprintf("  ⧗ %d", group.Running)
		}
		if group.Errors > 0 {
			stats += fmt.Sprintf("  ✗ %d", group.Errors)
		}
		stats += "  " + ago(now, group.Last)
		statsStyle := style
		if group.Errors > 0 && i != a.cursor[viewGroups] {
			statsStyle = styleDanger
		}
		a.text(screen, max(x+2, a.width-utf8.RuneCountInString(stats)), y, stats, statsStyle)
		y++
	}
}

func (a *App) drawInvocations(screen tcell.Screen, y int) {
	screen.HideCursor()
	y = a.drawFilter(screen, y)
	list := a.invocations()
	if len(list) == 0 {
		a.text(screen, 0, y, "No invocations match.", styleDim)
		return
	}
	if a.follow {
		a.cursor[viewInvocations] = 0
	}
	a.cursor[viewInvocations] = min(a.cursor[viewInvocations], len(list)-1)
	start, end := a.window(len(list))
	now := a.now()
	for i := start; i < end; i++ {
		inv := list[i]
		selected := i == a.cursor[viewInvocations]
		pick := func(style tcell.Style) tcell.Style {
			if selected {
				return styleCursor
			}
			return style
		}
		if selected {
			a.fill(screen, y, styleCursor)
		}
		dot, dotStyle := statusDot(inv)
		x := a.text(screen, 0, y, dot+" ", pick(dotStyle))
		cold := "  "
		if inv.Cold {
			cold = "❄ "
		}
		x = a.text(screen, x, y, cold, pick(styleTitle))
		x = a.text(screen, x, y, inv.Start.Format("15:04:05")+"  ", pick(styleDim))
		duration := formatDuration(inv.Duration(now))
		x = a.text(screen, x, y, fmt.Sprintf("%7s  ", duration), pick(durationStyle(inv.Duration(now))))
		preview := Preview(inv)
		if inv.Label != "" && !strings.Contains(preview, inv.Label) {
			preview = inv.Label + "  " + preview
		}
		x = a.text(screen, x, y, preview, pick(styleNormal))
		if inv.Status == Failed {
			a.text(screen, x+2, y, inv.ErrorMsg, pick(styleDanger))
		}
		y++
	}
}

// json formats an input or output for copying.
func (a *App) json(data []byte) string {
	if a.pretty {
		return prettyjson.FormatOr(data)
	}
	return PrettyJSON(data)
}

// jsonLines formats an input or output for display, coloured in pretty mode.
func (a *App) jsonLines(data []byte) []prettyjson.Line {
	if a.pretty {
		if lines, ok := prettyjson.FormatLines(data); ok {
			return lines
		}
	}
	return textLines(PrettyJSON(data), prettyjson.Text)
}

// logLines are an invocation's logs, with JSON pretty printed in pretty mode.
func (a *App) logLines(inv *Invocation) []string {
	lines := make([]string, 0, len(inv.Logs))
	for _, line := range a.coloredLogLines(inv) {
		lines = append(lines, prettyjson.Plain([]prettyjson.Line{line}))
	}
	return lines
}

func (a *App) coloredLogLines(inv *Invocation) []prettyjson.Line {
	lines := []prettyjson.Line{}
	for _, line := range inv.Logs {
		if a.pretty {
			if formatted, ok := prettyjson.FormatLogLine(line); ok {
				lines = append(lines, formatted...)
				continue
			}
		}
		lines = append(lines, textLines(line, prettyjson.Text)...)
	}
	return lines
}

func textLines(text string, kind prettyjson.Kind) []prettyjson.Line {
	var lines []prettyjson.Line
	for _, row := range strings.Split(text, "\n") {
		lines = append(lines, prettyjson.Line{{Kind: kind, Text: row}})
	}
	return lines
}

// jsonStyles colour pretty JSON in the pane.
var jsonStyles = map[prettyjson.Kind]tcell.Style{
	prettyjson.Punct:  tcell.StyleDefault.Foreground(tcell.ColorGray),
	prettyjson.Key:    tcell.StyleDefault.Foreground(tcell.ColorTeal),
	prettyjson.String: tcell.StyleDefault.Foreground(tcell.ColorGreen),
	prettyjson.Number: tcell.StyleDefault.Foreground(tcell.ColorOlive),
	prettyjson.Bool:   tcell.StyleDefault.Foreground(tcell.ColorPurple),
	prettyjson.Null:   tcell.StyleDefault.Foreground(tcell.ColorPurple),
}

type span struct {
	text  string
	style tcell.Style
}

// styledLine is one screen row of the detail view.
type styledLine []span

// wrapLine styles a formatted line and breaks it into rows no wider than
// width; text segments take base, JSON segments their kind's colour.
// Segments never hold a newline: formatted output is split into Lines.
func wrapLine(line prettyjson.Line, width int, base tcell.Style) []styledLine {
	rows := []styledLine{{}}
	used := 0
	for _, segment := range line {
		style, ok := jsonStyles[segment.Kind]
		if !ok {
			style = base
		}
		runes := []rune(strings.ReplaceAll(segment.Text, "\t", "  "))
		for width > 0 && used+len(runes) > width {
			take := width - used
			rows[len(rows)-1] = append(rows[len(rows)-1], span{string(runes[:take]), style})
			rows = append(rows, styledLine{})
			runes, used = runes[take:], 0
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], span{string(runes), style})
		used += len(runes)
	}
	return rows
}

func (a *App) detailLines(inv *Invocation) []styledLine {
	lines := []styledLine{}
	addLines := func(formatted []prettyjson.Line, base tcell.Style) {
		for _, line := range formatted {
			lines = append(lines, wrapLine(line, a.width, base)...)
		}
	}
	add := func(text string, style tcell.Style) {
		addLines(textLines(text, prettyjson.Text), style)
	}
	if inv.Status == Failed {
		add(inv.ErrorType+": "+inv.ErrorMsg, styleDanger.Bold(true))
		for _, frame := range inv.Trace {
			if strings.Contains(frame, inv.ErrorMsg) {
				continue
			}
			add("  ↳ "+strings.TrimSpace(frame), styleDanger)
		}
		add("", styleNormal)
	}
	if a.showIO {
		add("input", styleTitle)
		addLines(a.jsonLines(inv.Input), styleNormal)
		add("", styleNormal)
		add("output", styleTitle)
		if inv.Status == Running {
			add("(running)", styleDim)
		} else {
			addLines(a.jsonLines(inv.Output), styleNormal)
		}
		return lines
	}
	if len(inv.Logs) == 0 {
		add("No logs.", styleDim)
	}
	addLines(a.coloredLogLines(inv), styleNormal)
	return lines
}

func (a *App) drawDetail(screen tcell.Screen, y int) {
	screen.HideCursor()
	inv := a.invocation()
	if inv == nil {
		a.text(screen, 0, y, "This invocation was dropped from the history.", styleDim)
		return
	}
	now := a.now()
	dot, dotStyle := statusDot(inv)
	x := a.text(screen, 0, y, dot+" ", dotStyle)
	x = a.text(screen, x, y, inv.RequestID, styleBold)
	if inv.Cold {
		x = a.text(screen, x, y, "  ❄ cold", styleTitle)
	}
	x = a.text(screen, x, y, "  "+inv.Start.Format("15:04:05.000")+"  ", styleDim)
	x = a.text(screen, x, y, formatDuration(inv.Duration(now)), durationStyle(inv.Duration(now)))
	a.text(screen, x, y, fmt.Sprintf("  %d log lines", len(inv.Logs)), styleDim)
	y++
	a.text(screen, 0, y, Preview(inv), styleDim)
	y += 2

	lines := a.detailLines(inv)
	height := a.height - 1 - y
	if a.follow {
		a.scroll = max(0, len(lines)-height)
	}
	a.scroll = min(a.scroll, max(0, len(lines)-height))
	for i := a.scroll; i < len(lines) && y < a.height-1; i++ {
		x := 0
		for _, part := range lines[i] {
			x = a.text(screen, x, y, part.text, part.style)
		}
		y++
	}
}

func (a *App) drawSearch(screen tcell.Screen, y int) {
	x := a.text(screen, 0, y, "search logs: ", styleDim)
	x = a.text(screen, x, y, a.query, styleNormal)
	if a.editing {
		screen.ShowCursor(x, y)
	} else {
		screen.HideCursor()
	}
	a.text(screen, x+2, y, fmt.Sprintf("%d matches", len(a.matches)), styleDim)
	y += 2
	if len(a.matches) == 0 {
		return
	}
	a.cursor[viewSearch] = min(a.cursor[viewSearch], len(a.matches)-1)
	start, end := a.window(len(a.matches))
	for i := start; i < end && y < a.height-1; i++ {
		match := a.matches[i]
		style, dim := styleNormal, styleDim
		if i == a.cursor[viewSearch] {
			a.fill(screen, y, styleCursor)
			style, dim = styleCursor, styleCursor
		}
		x := a.text(screen, 0, y, match.Group.Title+"  "+shortID(match.Invocation.RequestID)+"  ", dim)
		a.text(screen, x, y, strings.TrimSpace(match.Text), style)
		y++
	}
}

func (a *App) drawFooter(screen tcell.Screen, y int) {
	if a.message != "" && a.now().Sub(a.messaged) < 4*time.Second {
		a.text(screen, 0, y, a.message, styleWarning)
		return
	}
	var hints []string
	switch {
	case a.editing:
		hints = []string{"enter apply", "esc clear"}
	case a.view == viewGroups:
		hints = []string{"j/k move", "enter open", "/ filter", "ctrl-f search"}
	case a.view == viewInvocations:
		hints = []string{"j/k move", "enter open", "esc back", "/ filter", "y copy input", "f follow"}
	case a.view == viewDetail:
		io := "i input/output"
		if a.showIO {
			io = "i logs"
		}
		hints = []string{"j/k scroll", io, "J pretty json", "y/Y copy in/out", "c copy logs", "f follow", "esc back"}
	case a.view == viewSearch:
		hints = []string{"j/k move", "enter open", "ctrl-f edit", "esc back"}
	}
	if a.stall() != nil {
		hints = append(hints, "p purge")
	}
	a.text(screen, 0, y, strings.Join(hints, " · "), styleDim)
}
