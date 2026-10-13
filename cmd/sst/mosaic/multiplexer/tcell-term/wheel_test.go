package tcellterm

import "testing"

func TestWantsWheel(t *testing.T) {
	cases := []struct {
		name string
		mode mode
		want bool
	}{
		{"plain output scrolls its scrollback", 0, false},
		{"a full-screen program", smcup | altScroll, true},
		{"a program reporting the mouse", mouseButtons, true},
		{"sgr encoding alone reports nothing", mouseSGR, false},
	}
	for _, c := range cases {
		vt := New()
		vt.mode = c.mode
		if got := vt.WantsWheel(); got != c.want {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
