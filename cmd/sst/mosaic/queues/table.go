package queues

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/sst/sst/v3/cmd/sst/mosaic/panekit"
)

// column is one sortable column of the queue table.
type column struct {
	title string
	width int
	// less orders two queues ascending by this column.
	less func(a, b *Queue) bool
	// cell renders the column for a queue.
	cell func(app *App, q *Queue) (string, tcell.Style)
	// descending is the direction a first press on the column sorts in.
	descending bool
}

const (
	colQueue = iota
	colWaiting
	colInFlight
	colDLQ
	colReceived
	colFailed
	colLast
)

func number(n int, warn tcell.Style) (string, tcell.Style) {
	if n < 0 {
		return "–", panekit.Dim
	}
	if n == 0 {
		return "0", panekit.Dim
	}
	return fmt.Sprint(n), warn
}

func inFlight(q *Queue) int {
	if q.Depth == nil || q.Depth.Error != "" {
		return -1
	}
	return q.Depth.InFlight
}

// dlqWaiting is the depth of a queue's DLQ, -1 when it has none or it is
// unknown.
func (a *App) dlqWaiting(q *Queue) int {
	dlq := a.store.Queue(q.DLQ)
	if dlq == nil {
		return -1
	}
	return dlq.Waiting()
}

func (a *App) columns() []column {
	return []column{
		{title: "Queue", less: func(x, y *Queue) bool { return x.Name < y.Name },
			cell: func(app *App, q *Queue) (string, tcell.Style) {
				name := app.displayName(q.Name)
				if q.IsDLQ() {
					return name + " (DLQ)", panekit.Dim
				}
				return name, panekit.Bold
			}},
		{title: "Waiting", width: 8, descending: true, less: func(x, y *Queue) bool { return x.Waiting() < y.Waiting() },
			cell: func(app *App, q *Queue) (string, tcell.Style) {
				if q.IsDLQ() {
					return number(q.Waiting(), panekit.Danger)
				}
				return number(q.Waiting(), panekit.Warning)
			}},
		{title: "Flight", width: 7, descending: true, less: func(x, y *Queue) bool { return inFlight(x) < inFlight(y) },
			cell: func(app *App, q *Queue) (string, tcell.Style) { return number(inFlight(q), panekit.Normal) }},
		{title: "DLQ", width: 6, descending: true, less: func(x, y *Queue) bool { return a.dlqWaiting(x) < a.dlqWaiting(y) },
			cell: func(app *App, q *Queue) (string, tcell.Style) { return number(app.dlqWaiting(q), panekit.Danger) }},
		{title: "Recv", width: 6, descending: true, less: func(x, y *Queue) bool { return x.Received < y.Received },
			cell: func(app *App, q *Queue) (string, tcell.Style) { return number(q.Received, panekit.Normal) }},
		{title: "Failed", width: 7, descending: true, less: func(x, y *Queue) bool { return x.Failed < y.Failed },
			cell: func(app *App, q *Queue) (string, tcell.Style) { return number(q.Failed, panekit.Danger) }},
		{title: "Last", width: 9, descending: true, less: func(x, y *Queue) bool { return x.Last.Before(y.Last) },
			cell: func(app *App, q *Queue) (string, tcell.Style) {
				if q.Last.IsZero() {
					return "–", panekit.Dim
				}
				return panekit.Ago(app.now(), q.Last), panekit.Dim
			}},
	}
}

// displayName drops the stage prefix every queue of the app shares.
func (a *App) displayName(name string) string {
	if a.prefix != "" && strings.HasPrefix(name, a.prefix) && len(name) > len(a.prefix) {
		return name[len(a.prefix):]
	}
	return name
}

// table is the rows the queue table shows: filtered, DLQs folded into their
// source queue unless shown, sorted by the chosen column with stuck queues
// pinned first.
func (a *App) table() []*Queue {
	rows := []*Queue{}
	for _, q := range a.store.List("") {
		if a.filter != "" && !strings.Contains(strings.ToLower(a.displayName(q.Name)), strings.ToLower(a.filter)) {
			continue
		}
		if q.IsDLQ() && !a.showDLQs {
			continue
		}
		rows = append(rows, q)
	}
	columns := a.columns()
	sort.SliceStable(rows, func(i, j int) bool {
		x, y := rows[i], rows[j]
		if (x.Stall != nil) != (y.Stall != nil) {
			return x.Stall != nil
		}
		if a.sortColumn < 0 {
			return false
		}
		less := columns[a.sortColumn].less
		if a.sortDescending {
			return less(y, x)
		}
		return less(x, y)
	})
	return rows
}

// sortBy sorts on a column; choosing the current column flips direction.
func (a *App) sortBy(index int) {
	columns := a.columns()
	if index < 0 || index >= len(columns) {
		return
	}
	if a.sortColumn == index {
		a.sortDescending = !a.sortDescending
	} else {
		a.sortColumn, a.sortDescending = index, columns[index].descending
	}
	a.cursor[viewQueues] = 0
}

// layout gives each column its x and width: Queue takes what the numeric
// columns leave.
func (a *App) layout(width int) []int {
	columns := a.columns()
	fixed := 0
	for _, c := range columns[1:] {
		fixed += c.width + 1
	}
	xs := make([]int, len(columns)+1)
	xs[0] = 2
	xs[1] = max(xs[0]+12, width-fixed)
	for i := 1; i < len(columns); i++ {
		xs[i+1] = xs[i] + columns[i].width + 1
	}
	return xs
}

func (a *App) drawTable(screen tcell.Screen, y int, width int) {
	columns := a.columns()
	xs := a.layout(width)
	a.headerY = y
	for i, c := range columns {
		title := c.title
		if i == a.sortColumn {
			if a.sortDescending {
				title += "▼"
			} else {
				title += "▲"
			}
		}
		style := panekit.Dim.Bold(true)
		if i == a.sortColumn {
			style = panekit.Title
		}
		a.cell(screen, xs[i], y, xs[i+1]-xs[i]-1, title, style, i > 0)
	}
	y++
	a.bodyTop = y
	rows := a.table()
	if len(rows) == 0 {
		panekit.Text(screen, 2, y, "No queues yet. They appear after the first deploy or message.", panekit.Dim)
		return
	}
	a.cursor[viewQueues] = min(a.cursor[viewQueues], len(rows)-1)
	for i := a.offset(); i < len(rows) && y < a.height-1; i++ {
		q := rows[i]
		selected := i == a.cursor[viewQueues]
		if selected {
			panekit.Fill(screen, y, panekit.Cursor)
		}
		dot, dotStyle := "○", panekit.Dim
		switch {
		case q.Stall != nil:
			dot, dotStyle = "⚠", panekit.Danger
		case q.Running > 0:
			dot, dotStyle = "●", panekit.Warning
		case q.Received > 0:
			dot, dotStyle = "●", panekit.Success
		}
		if selected {
			dotStyle = panekit.Cursor
		}
		panekit.Text(screen, 0, y, dot, dotStyle)
		for c, col := range columns {
			text, style := col.cell(a, q)
			if selected {
				style = panekit.Cursor
			}
			a.cell(screen, xs[c], y, xs[c+1]-xs[c]-1, text, style, c > 0)
		}
		y++
	}
}

// cell draws text in a column, truncated with an ellipsis, right aligned
// for numbers.
func (a *App) cell(screen tcell.Screen, x int, y int, width int, text string, style tcell.Style, right bool) {
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

// headerColumn is the column under x on the header row, or -1.
func (a *App) headerColumn(x int, width int) int {
	xs := a.layout(width)
	for i := 0; i < len(xs)-1; i++ {
		if x >= xs[i] && x < xs[i+1] {
			return i
		}
	}
	return -1
}
