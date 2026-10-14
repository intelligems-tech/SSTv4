package multiplexer

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/gdamore/tcell/v2/views"
)

// Status is the block drawn at the bottom of the sidebar: the stage, the
// segments a launcher passes in SST_DEV_STATUS, pooled worker counts and
// alerts such as a stuck queue.
type Status struct {
	Stage    string
	Tone     string
	Segments []StatusSegment
	Pool     *PoolStatus
	Alerts   []string
}

type StatusSegment struct {
	Key   string
	Value string
}

type PoolStatus struct {
	Workers  int
	InFlight int
	Queued   int
}

// ParseStatusSegments reads SST_DEV_STATUS: comma separated `key=value`
// pairs; a pair without `=` is shown as a bare value.
func ParseStatusSegments(raw string) []StatusSegment {
	var segments []StatusSegment
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			segments = append(segments, StatusSegment{Value: key})
			continue
		}
		segments = append(segments, StatusSegment{Key: strings.TrimSpace(key), Value: strings.TrimSpace(value)})
	}
	return segments
}

// StageStyle colours the stage by tone: prod is red, shared stages are
// yellow and anything local is green.
func StageStyle(tone string) tcell.Style {
	switch tone {
	case "prod":
		return tcell.StyleDefault.Foreground(tcell.ColorRed).Bold(true)
	case "shared":
		return tcell.StyleDefault.Foreground(tcell.ColorYellow).Bold(true)
	default:
		return tcell.StyleDefault.Foreground(tcell.ColorGreen).Bold(true)
	}
}

// lines renders the block as sidebar rows, newest alert first.
func (st Status) lines() []statusLine {
	gray := tcell.StyleDefault.Foreground(tcell.NewRGBColor(138, 138, 138))
	var lines []statusLine
	for _, alert := range st.Alerts {
		lines = append(lines, statusLine{left: "⚠ " + alert, leftStyle: tcell.StyleDefault.Foreground(tcell.ColorRed).Bold(true)})
	}
	if st.Stage != "" {
		lines = append(lines, statusLine{left: "stage", leftStyle: gray, right: st.Stage, rightStyle: StageStyle(st.Tone)})
	}
	for _, segment := range st.Segments {
		lines = append(lines, statusLine{left: segment.Key, leftStyle: gray, right: segment.Value, rightStyle: tcell.StyleDefault})
	}
	if st.Pool != nil {
		value := fmt.Sprintf("%dw %d▶ %d⧗", st.Pool.Workers, st.Pool.InFlight, st.Pool.Queued)
		style := tcell.StyleDefault
		if st.Pool.Queued > 0 {
			style = style.Foreground(tcell.ColorYellow)
		}
		lines = append(lines, statusLine{left: "pool", leftStyle: gray, right: value, rightStyle: style})
	}
	return lines
}

type statusLine struct {
	left       string
	leftStyle  tcell.Style
	right      string
	rightStyle tcell.Style
}

func (s *Multiplexer) drawStatus() {
	lines := s.status.lines()
	if len(lines) == 0 {
		return
	}
	divider := views.NewTextBar()
	divider.SetLeft(strings.Repeat("─", s.sidebarWidth()), tcell.StyleDefault.Foreground(tcell.ColorGray).Dim(true))
	s.stack.AddWidget(divider, 0)
	for _, line := range lines {
		bar := views.NewTextBar()
		bar.SetLeft(line.left, line.leftStyle)
		if line.right != "" {
			bar.SetRight(line.right+" ", line.rightStyle)
		}
		s.stack.AddWidget(bar, 0)
	}
	blank := views.NewTextBar()
	s.stack.AddWidget(blank, 0)
}

type EventStatus struct {
	tcell.EventTime
	Status Status
}

// SetStatus replaces the sidebar status block.
func (s *Multiplexer) SetStatus(status Status) {
	evt := &EventStatus{Status: status}
	evt.SetEventNow()
	s.screen.PostEvent(evt)
}
