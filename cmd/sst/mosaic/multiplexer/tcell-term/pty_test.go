//go:build !windows

package tcellterm

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

type fakeSurface struct{}

func (fakeSurface) SetContent(int, int, rune, []rune, tcell.Style) {}
func (fakeSurface) Size() (int, int)                               { return 80, 24 }

// A full-screen program redraws after every wheel event, so a fast scroll
// fills both pty buffers at once: the program blocks writing its screen while
// input queues behind it. Writing that input must not stop the terminal from
// reading the program's output, or neither side ever moves again.
func TestInputDoesNotBlockOutput(t *testing.T) {
	vt := New()
	vt.SetSurface(fakeSurface{})
	err := vt.Start(exec.Command("sh", "-c", `stty raw -echo; printf ready; head -c 300000 /dev/zero | tr '\000' x; cat >/dev/null`))
	if err != nil {
		t.Fatal(err)
	}
	defer vt.Close()

	deadline := time.Now().Add(5 * time.Second)
	// Output only arrives after stty, so the pty is raw by the time any shows.
	for strings.TrimSpace(vt.String()) == "" {
		if time.Now().After(deadline) {
			t.Fatal("program never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	vt.mu.Lock()
	vt.mode |= smcup | mouseButtons | mouseSGR
	vt.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		wheel := tcell.NewEventMouse(10, 10, tcell.WheelDown, tcell.ModNone)
		for i := 0; i < 20000; i++ {
			vt.HandleEvent(wheel)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("wheel input deadlocked against the program's output")
	}
}
