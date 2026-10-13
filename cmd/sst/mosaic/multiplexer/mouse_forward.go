package multiplexer

import "github.com/gdamore/tcell/v2"

// forwardMouse hands a mouse event to the selected pane when the program in
// it handles the mouse itself, in the pane's own coordinates. The wheel goes
// to a full-screen pane wherever the pointer is; clicks inside the pane go to
// a program that asked for mouse reports, and focus it so its keys work.
// Shift keeps the multiplexer's own text selection. It reports whether the
// event was consumed.
func (s *Multiplexer) forwardMouse(selected *pane, evt *tcell.EventMouse) bool {
	if selected == nil || selected.dead || s.filtering || evt.Modifiers()&tcell.ModShift != 0 {
		return false
	}
	x, y := evt.Position()
	localX := min(max(x-s.mainX(), 0), max(0, s.mainWidth()-1))
	localY := min(max(y-s.contentY(), 0), max(0, s.mainHeight()-1))
	local := tcell.NewEventMouse(localX, localY, evt.Buttons(), evt.Modifiers())

	if evt.Buttons()&(tcell.WheelUp|tcell.WheelDown) != 0 {
		if !selected.vt.WantsWheel() {
			return false
		}
		selected.vt.HandleEvent(local)
		return true
	}
	if !selected.vt.ReportsMouse() || s.dragging {
		return false
	}
	if evt.Buttons() != tcell.ButtonNone && x < s.mainX() {
		return false
	}
	selected.vt.HandleEvent(local)
	if evt.Buttons()&tcell.ButtonPrimary != 0 && !s.focused && selected.Killable {
		s.focus()
	}
	return true
}
