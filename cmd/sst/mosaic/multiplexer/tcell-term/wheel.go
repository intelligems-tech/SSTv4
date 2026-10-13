package tcellterm

// WantsWheel reports whether the program owns mouse wheel events: it runs
// on the alternate screen (a full-screen program has no scrollback to
// scroll), or it asked for mouse reporting. HandleEvent then turns the wheel
// into mouse reports or, with alt-scroll, into arrow keys.
func (vt *VT) WantsWheel() bool {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	if vt.mode&smcup != 0 {
		return true
	}
	return vt.mode&(mouseButtons|mouseDrag|mouseMotion) != 0
}

// ReportsMouse reports whether the program asked for mouse button events.
func (vt *VT) ReportsMouse() bool {
	vt.mu.Lock()
	defer vt.mu.Unlock()
	return vt.mode&(mouseButtons|mouseDrag|mouseMotion) != 0
}
