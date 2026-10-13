package stepfunctions

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
)

// NodeKind is what a graph node stands for.
type NodeKind int

const (
	KindState NodeKind = iota
	KindStart
	KindEnd
	// KindJoin is where the branches of a Parallel or Map meet again.
	KindJoin
	// KindDummy carries an edge across a layer.
	KindDummy
)

// laneGap is the space between the lanes loops run up.
const laneGap = 2

// Zoom is how densely a graph is drawn. A box is one row (a bracketed
// label) or three (a bordered one); the rows between layers hold the
// edges' channel and arrow.
type Zoom struct {
	Name      string
	BoxHeight int
	// Pitch is the rows from one layer to the next.
	Pitch int
	// Gap is the columns between nodes in a layer.
	Gap      int
	MaxLabel int
}

// Zooms run from the most zoomed out to the most zoomed in.
var Zooms = []Zoom{
	{Name: "overview", BoxHeight: 1, Pitch: 4, Gap: 1, MaxLabel: 10},
	{Name: "compact", BoxHeight: 1, Pitch: 4, Gap: 3, MaxLabel: 20},
	{Name: "detailed", BoxHeight: 3, Pitch: 6, Gap: 3, MaxLabel: 30},
	{Name: "wide", BoxHeight: 3, Pitch: 7, Gap: 6, MaxLabel: 60},
}

// DefaultZoom is the detailed zoom.
const DefaultZoom = 2

type Node struct {
	Kind  NodeKind
	State *aws.State
	Label string
	Layer int
	Order int
	X, Y  int
	W     int
}

// Center is the column edges attach to.
func (n *Node) Center() int {
	return n.X + n.W/2
}

// Box reports whether the node is drawn as a box.
func (n *Node) Box() bool {
	return n.Kind == KindState || n.Kind == KindStart || n.Kind == KindEnd
}

type edge struct {
	from, to int
	back     bool
}

// Graph is a laid-out state machine, drawn top to bottom like the console's
// graph view. Loops are drawn up the right side.
type Graph struct {
	Zoom   Zoom
	Nodes  []*Node
	edges  []edge
	layers [][]int
	byName map[string]int
	Width  int
	Height int
}

// Node returns the node of a state, or nil.
func (g *Graph) Node(state string) *Node {
	if i, ok := g.byName[state]; ok {
		return g.Nodes[i]
	}
	return nil
}

// NewGraph builds and lays out the graph of a definition at a zoom.
func NewGraph(m *aws.Machine, zoom Zoom) *Graph {
	g := &Graph{Zoom: zoom, byName: map[string]int{}}
	start := g.add(&Node{Kind: KindStart, Label: "Start"})
	end := g.add(&Node{Kind: KindEnd, Label: "End"})
	entry := g.machine(m, end)
	if entry >= 0 {
		g.link(start, entry)
	} else {
		g.link(start, end)
	}
	g.breakCycles(start)
	g.layer(end)
	g.splitLongEdges()
	g.order()
	g.place()
	return g
}

func (g *Graph) add(n *Node) int {
	switch {
	case n.Box() && g.Zoom.BoxHeight == 1:
		n.W = utf8.RuneCountInString(n.Label) + 2
	case n.Box():
		n.W = utf8.RuneCountInString(n.Label) + 4
	default:
		n.W = 1
	}
	g.Nodes = append(g.Nodes, n)
	return len(g.Nodes) - 1
}

func (g *Graph) link(from int, to int) {
	for _, e := range g.edges {
		if e.from == from && e.to == to {
			return
		}
	}
	g.edges = append(g.edges, edge{from: from, to: to})
}

// machine adds a machine's states, returns its StartAt node, and sends its
// terminal states to exit.
func (g *Graph) machine(m *aws.Machine, exit int) int {
	for _, name := range m.Order {
		state := m.States[name]
		g.byName[name] = g.add(&Node{Kind: KindState, State: state, Label: label(state, g.Zoom.MaxLabel)})
	}
	for _, name := range m.Order {
		state := m.States[name]
		from := g.byName[name]
		after := from
		if children := state.Children(); len(children) > 0 {
			after = g.add(&Node{Kind: KindJoin})
			for _, child := range children {
				if entry := g.machine(child, after); entry >= 0 {
					g.link(from, entry)
				}
			}
		}
		targets := append([]string{state.Next}, state.Choices...)
		targets = append(targets, state.Default)
		for _, target := range targets {
			if i, ok := g.byName[target]; ok && target != "" {
				g.link(after, i)
			}
		}
		for _, target := range state.Catches {
			if i, ok := g.byName[target]; ok {
				g.link(from, i)
			}
		}
		if state.Terminal() {
			g.link(after, exit)
		}
	}
	if i, ok := g.byName[m.StartAt]; ok {
		return i
	}
	return -1
}

// label is a state's name behind a glyph for its type.
func label(state *aws.State, maxLabel int) string {
	name := state.Name
	if utf8.RuneCountInString(name) > maxLabel {
		name = string([]rune(name)[:maxLabel-1]) + "…"
	}
	return typeGlyph(state) + " " + name
}

func typeGlyph(state *aws.State) string {
	switch state.Type {
	case "Task":
		if state.Function != "" || strings.Contains(state.Resource, ":lambda:") {
			return "λ"
		}
		return "■"
	case "Choice":
		return "◇"
	case "Parallel":
		return "‖"
	case "Map":
		return "≡"
	case "Wait":
		return "◷"
	case "Pass":
		return "→"
	case "Succeed":
		return "✓"
	case "Fail":
		return "✗"
	}
	return "·"
}

// breakCycles marks the edges that close a loop, found depth first from
// the start, so the rest is acyclic.
func (g *Graph) breakCycles(start int) {
	out := g.outgoing()
	state := make([]int, len(g.Nodes)) // 0 new, 1 on the stack, 2 done
	var visit func(int)
	visit = func(n int) {
		state[n] = 1
		for _, i := range out[n] {
			switch state[g.edges[i].to] {
			case 0:
				visit(g.edges[i].to)
			case 1:
				g.edges[i].back = true
			}
		}
		state[n] = 2
	}
	visit(start)
	for n := range g.Nodes {
		if state[n] == 0 {
			visit(n)
		}
	}
}

func (g *Graph) outgoing() [][]int {
	out := make([][]int, len(g.Nodes))
	for i, e := range g.edges {
		out[e.from] = append(out[e.from], i)
	}
	return out
}

// layer puts each node one below its lowest predecessor, and End last.
func (g *Graph) layer(end int) {
	indegree := make([]int, len(g.Nodes))
	for _, e := range g.edges {
		if !e.back {
			indegree[e.to]++
		}
	}
	out := g.outgoing()
	queue := []int{}
	for n := range g.Nodes {
		if indegree[n] == 0 {
			queue = append(queue, n)
		}
	}
	deepest := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, i := range out[n] {
			e := g.edges[i]
			if e.back {
				continue
			}
			g.Nodes[e.to].Layer = max(g.Nodes[e.to].Layer, g.Nodes[n].Layer+1)
			deepest = max(deepest, g.Nodes[e.to].Layer)
			if indegree[e.to]--; indegree[e.to] == 0 {
				queue = append(queue, e.to)
			}
		}
	}
	g.Nodes[end].Layer = deepest
}

// splitLongEdges routes an edge that skips layers through dummy nodes, one
// per layer it crosses.
func (g *Graph) splitLongEdges() {
	edges := []edge{}
	for _, e := range g.edges {
		from, to := g.Nodes[e.from], g.Nodes[e.to]
		if e.back || to.Layer-from.Layer <= 1 {
			edges = append(edges, e)
			continue
		}
		previous := e.from
		for layer := from.Layer + 1; layer < to.Layer; layer++ {
			dummy := g.add(&Node{Kind: KindDummy, Layer: layer})
			edges = append(edges, edge{from: previous, to: dummy})
			previous = dummy
		}
		edges = append(edges, edge{from: previous, to: e.to})
	}
	g.edges = edges
}

// order orders each layer by the barycenter of its neighbours, sweeping
// down and up a few times to cut crossings.
func (g *Graph) order() {
	depth := 0
	for _, n := range g.Nodes {
		depth = max(depth, n.Layer)
	}
	g.layers = make([][]int, depth+1)
	for i, n := range g.Nodes {
		g.layers[n.Layer] = append(g.layers[n.Layer], i)
	}
	preds := make([][]int, len(g.Nodes))
	succs := make([][]int, len(g.Nodes))
	for _, e := range g.edges {
		if !e.back {
			preds[e.to] = append(preds[e.to], e.from)
			succs[e.from] = append(succs[e.from], e.to)
		}
	}
	g.renumber()
	sweep := func(layer []int, neighbours [][]int) {
		key := map[int]float64{}
		for _, n := range layer {
			key[n] = float64(g.Nodes[n].Order)
			if len(neighbours[n]) == 0 {
				continue
			}
			sum := 0.0
			for _, m := range neighbours[n] {
				sum += float64(g.Nodes[m].Order)
			}
			key[n] = sum / float64(len(neighbours[n]))
		}
		sort.SliceStable(layer, func(i, j int) bool { return key[layer[i]] < key[layer[j]] })
		for i, n := range layer {
			g.Nodes[n].Order = i
		}
	}
	for pass := 0; pass < 4; pass++ {
		for l := 1; l < len(g.layers); l++ {
			sweep(g.layers[l], preds)
		}
		for l := len(g.layers) - 2; l >= 0; l-- {
			sweep(g.layers[l], succs)
		}
	}
	for l := 1; l < len(g.layers); l++ {
		sweep(g.layers[l], preds)
	}
}

func (g *Graph) renumber() {
	for _, layer := range g.layers {
		for i, n := range layer {
			g.Nodes[n].Order = i
		}
	}
}

// place gives each node its column and row: a node sits under the mean of
// its predecessors, a layer is packed without overlap and shifted back onto
// those means, and the drawing starts at column 0.
func (g *Graph) place() {
	preds := make([][]int, len(g.Nodes))
	for _, e := range g.edges {
		if !e.back {
			preds[e.to] = append(preds[e.to], e.from)
		}
	}
	for l, layer := range g.layers {
		desired := make([]float64, len(layer))
		cursor := 0
		for i, n := range layer {
			node := g.Nodes[n]
			node.Y = l * g.Zoom.Pitch
			if len(preds[n]) == 0 || l == 0 {
				desired[i] = float64(cursor + node.W/2)
				cursor += node.W + g.Zoom.Gap
				continue
			}
			sum := 0
			for _, p := range preds[n] {
				sum += g.Nodes[p].Center()
			}
			desired[i] = float64(sum) / float64(len(preds[n]))
		}
		shift := 0.0
		right := 0
		for i, n := range layer {
			node := g.Nodes[n]
			x := int(desired[i]) - node.W/2
			if i > 0 && x < right+g.Zoom.Gap {
				x = right + g.Zoom.Gap
			}
			node.X = x
			right = x + node.W
			shift += desired[i] - float64(node.Center())
		}
		if len(layer) > 0 {
			move := int(shift / float64(len(layer)))
			for _, n := range layer {
				g.Nodes[n].X += move
			}
		}
	}
	left := 0
	for _, n := range g.Nodes {
		left = min(left, n.X)
	}
	g.Width, g.Height = 0, len(g.layers)*g.Zoom.Pitch-(g.Zoom.Pitch-g.Zoom.BoxHeight)
	for _, n := range g.Nodes {
		n.X -= left
		g.Width = max(g.Width, n.X+n.W)
	}
	if backs := g.backEdges(); len(backs) > 0 {
		g.Width += laneGap * (len(backs) + 1)
	}
}

func (g *Graph) backEdges() []edge {
	result := []edge{}
	for _, e := range g.edges {
		if e.back {
			result = append(result, e)
		}
	}
	return result
}

// Selectable lists the state nodes, top to bottom then left to right.
func (g *Graph) Selectable() []*Node {
	result := []*Node{}
	for _, layer := range g.layers {
		row := []*Node{}
		for _, n := range layer {
			if g.Nodes[n].Kind == KindState {
				row = append(row, g.Nodes[n])
			}
		}
		sort.Slice(row, func(i, j int) bool { return row[i].X < row[j].X })
		result = append(result, row...)
	}
	return result
}

// At is the state node drawn at a cell, or nil.
func (g *Graph) At(x int, y int) *Node {
	for _, n := range g.Nodes {
		if n.Kind == KindState && x >= n.X && x < n.X+n.W && y >= n.Y && y < n.Y+g.Zoom.BoxHeight {
			return n
		}
	}
	return nil
}

// Neighbour is the nearest state node from n in a direction: dx or dy is
// -1 or 1.
func (g *Graph) Neighbour(n *Node, dx int, dy int) *Node {
	var best *Node
	bestScore := 0
	cx, cy := n.Center(), n.Y
	for _, m := range g.Nodes {
		if m == n || m.Kind != KindState {
			continue
		}
		mx, my := m.Center(), m.Y
		var primary, secondary int
		if dy != 0 {
			primary, secondary = (my-cy)*dy, mx-cx
		} else {
			primary, secondary = (mx-cx)*dx, my-cy
		}
		if primary <= 0 {
			continue
		}
		if secondary < 0 {
			secondary = -secondary
		}
		score := primary*4 + secondary
		if dx != 0 {
			score = primary + secondary*4
		}
		if best == nil || score < bestScore {
			best, bestScore = m, score
		}
	}
	return best
}

// ---- drawing ----

const (
	lineN uint8 = 1 << iota
	lineE
	lineS
	lineW
)

var lineGlyphs = map[uint8]rune{
	lineN: '│', lineS: '│', lineN | lineS: '│',
	lineE: '─', lineW: '─', lineE | lineW: '─',
	lineS | lineE: '╭', lineS | lineW: '╮', lineN | lineE: '╰', lineN | lineW: '╯',
	lineN | lineS | lineE: '├', lineN | lineS | lineW: '┤',
	lineE | lineW | lineS: '┬', lineE | lineW | lineN: '┴',
	lineN | lineE | lineS | lineW: '┼',
}

// Canvas is a drawn graph: one rune and style per cell.
type Canvas struct {
	W, H   int
	lines  []uint8
	runes  []rune
	styles []tcell.Style
}

func newCanvas(w int, h int) *Canvas {
	return &Canvas{W: w, H: h, lines: make([]uint8, w*h), runes: make([]rune, w*h), styles: make([]tcell.Style, w*h)}
}

func (c *Canvas) in(x int, y int) bool {
	return x >= 0 && y >= 0 && x < c.W && y < c.H
}

// Cell is the rune and style at a cell; 0 is empty.
func (c *Canvas) Cell(x int, y int) (rune, tcell.Style) {
	if !c.in(x, y) {
		return 0, tcell.StyleDefault
	}
	i := y*c.W + x
	if c.runes[i] != 0 {
		return c.runes[i], c.styles[i]
	}
	if glyph, ok := lineGlyphs[c.lines[i]]; ok {
		return glyph, c.styles[i]
	}
	return 0, tcell.StyleDefault
}

func (c *Canvas) put(x int, y int, r rune, style tcell.Style) {
	if c.in(x, y) {
		c.runes[y*c.W+x], c.styles[y*c.W+x] = r, style
	}
}

func (c *Canvas) text(x int, y int, text string, style tcell.Style) {
	for _, r := range text {
		c.put(x, y, r, style)
		x++
	}
}

func (c *Canvas) connect(x int, y int, bits uint8, style tcell.Style) {
	if c.in(x, y) {
		c.lines[y*c.W+x] |= bits
		c.styles[y*c.W+x] = style
	}
}

func (c *Canvas) vline(x int, y0 int, y1 int, style tcell.Style) {
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	for y := y0; y <= y1; y++ {
		var bits uint8
		if y > y0 {
			bits |= lineN
		}
		if y < y1 {
			bits |= lineS
		}
		if y0 == y1 {
			bits = lineN | lineS
		}
		c.connect(x, y, bits, style)
	}
}

func (c *Canvas) hline(y int, x0 int, x1 int, style tcell.Style) {
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if x0 == x1 {
		return
	}
	for x := x0; x <= x1; x++ {
		var bits uint8
		if x > x0 {
			bits |= lineW
		}
		if x < x1 {
			bits |= lineE
		}
		c.connect(x, y, bits, style)
	}
}

// Styler colours a node; selected is the cursor's node.
type Styler func(n *Node, selected bool) tcell.Style

// Draw renders the graph.
func (g *Graph) Draw(styleFor Styler, selected *Node, edgeStyle tcell.Style) *Canvas {
	c := newCanvas(g.Width, g.Height)
	boxHeight := g.Zoom.BoxHeight
	for _, e := range g.edges {
		if e.back {
			continue
		}
		from, to := g.Nodes[e.from], g.Nodes[e.to]
		bottom := from.Y + boxHeight - 1
		channel := bottom + g.Zoom.Pitch - boxHeight - 1
		c.vline(from.Center(), bottom, channel, edgeStyle)
		c.hline(channel, from.Center(), to.Center(), edgeStyle)
		c.vline(to.Center(), channel, to.Y, edgeStyle)
	}
	lane := g.Width - laneGap
	for _, e := range g.backEdges() {
		from, to := g.Nodes[e.from], g.Nodes[e.to]
		fromRow, toRow := from.Y+boxHeight/2, to.Y+boxHeight/2
		if g.blockedRight(from) {
			// Leave from below so the line does not run through a neighbour.
			fromRow = from.Y + boxHeight
			c.vline(from.Center(), from.Y+boxHeight-1, fromRow, edgeStyle)
			c.hline(fromRow, from.Center(), lane, edgeStyle)
		} else {
			c.hline(fromRow, from.X+from.W, lane, edgeStyle)
		}
		c.vline(lane, fromRow, toRow, edgeStyle)
		c.hline(toRow, to.X+to.W, lane, edgeStyle)
		c.put(to.X+to.W, toRow, '◀', edgeStyle)
		lane -= laneGap
	}
	for _, n := range g.Nodes {
		switch {
		case n.Box():
			g.drawBox(c, n, styleFor(n, n == selected))
		default:
			c.vline(n.Center(), n.Y, n.Y+boxHeight-1, edgeStyle)
		}
	}
	for _, e := range g.edges {
		from, to := g.Nodes[e.from], g.Nodes[e.to]
		if e.back {
			if from.Box() && boxHeight > 1 && g.blockedRight(from) {
				c.put(from.Center(), from.Y+boxHeight-1, '┬', styleFor(from, from == selected))
			}
			continue
		}
		if from.Box() && boxHeight > 1 {
			c.put(from.Center(), from.Y+boxHeight-1, '┬', styleFor(from, from == selected))
		}
		if to.Box() {
			c.put(to.Center(), to.Y-1, '▼', edgeStyle)
		}
	}
	return c
}

// blockedRight reports whether another node sits right of n in its layer.
func (g *Graph) blockedRight(n *Node) bool {
	for _, m := range g.layers[n.Layer] {
		if g.Nodes[m] != n && g.Nodes[m].X > n.X {
			return true
		}
	}
	return false
}

func (g *Graph) drawBox(c *Canvas, n *Node, style tcell.Style) {
	if g.Zoom.BoxHeight == 1 {
		c.text(n.X, n.Y, "["+n.Label+"]", style)
		return
	}
	inner := n.W - 2
	c.text(n.X, n.Y, "╭"+strings.Repeat("─", inner)+"╮", style)
	c.text(n.X, n.Y+1, "│ "+n.Label+" │", style)
	c.text(n.X, n.Y+2, "╰"+strings.Repeat("─", inner)+"╯", style)
}

// String draws the graph as plain text, for tests.
func (c *Canvas) String() string {
	var b strings.Builder
	for y := 0; y < c.H; y++ {
		line := make([]rune, c.W)
		for x := 0; x < c.W; x++ {
			r, _ := c.Cell(x, y)
			if r == 0 {
				r = ' '
			}
			line[x] = r
		}
		b.WriteString(strings.TrimRight(string(line), " "))
		b.WriteByte('\n')
	}
	return b.String()
}
