package multiplexer

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	tcellterm "github.com/sst/sst/v3/cmd/sst/mosaic/multiplexer/tcell-term"
)

func TestForwardMouseOnlyWhenTheProgramWantsIt(t *testing.T) {
	screen := tcell.NewSimulationScreen("")
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	screen.SetSize(120, 40)
	s := &Multiplexer{screen: screen, width: 120, height: 40}
	plain := newTestPane(t)

	click := tcell.NewEventMouse(s.mainX()+2, 3, tcell.ButtonPrimary, tcell.ModNone)
	if s.forwardMouse(plain, click) {
		t.Fatal("a pane that does not report the mouse keeps text selection")
	}
	if s.forwardMouse(nil, click) {
		t.Fatal("no pane, nothing to forward")
	}
	wheel := tcell.NewEventMouse(2, 3, tcell.WheelDown, tcell.ModNone)
	if s.forwardMouse(plain, wheel) {
		t.Fatal("the wheel scrolls a plain pane's scrollback")
	}
}

func newTestPane(t *testing.T) *pane {
	t.Helper()
	return &pane{vt: tcellterm.New()}
}
