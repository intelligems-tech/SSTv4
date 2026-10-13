// Package panekit holds what the fork's full-screen `sst dev` panes share:
// the screen loop, drawing helpers and styles.
package panekit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gdamore/tcell/v2"
)

// Model is a pane: it folds dev events in, handles keys and draws.
type Model interface {
	// Apply folds one dev event in and reports whether a redraw is due.
	Apply(evt interface{}) bool
	// HandleKey applies a key press; false exits the pane.
	HandleKey(evt *tcell.EventKey) bool
	Draw(screen tcell.Screen)
}

// MouseModel is a Model that also takes mouse clicks and the wheel.
type MouseModel interface {
	HandleMouse(evt *tcell.EventMouse)
}

// TickModel is a Model that does periodic work, such as polling the dev
// server; Tick runs about every 100 ms and reports whether a redraw is due.
type TickModel interface {
	Tick() bool
}

type eventDev struct {
	tcell.EventTime
	evt interface{}
}

type eventTick struct {
	tcell.EventTime
}

// Run draws a pane until ctx ends, the event stream closes or the model
// exits. Dev events are drawn at most every 100 ms so a log storm stays
// cheap, and the pane redraws every second for clocks and durations.
func Run(ctx context.Context, evts <-chan interface{}, model Model) error {
	screen, err := tcell.NewScreen()
	if err != nil {
		return err
	}
	if err := screen.Init(); err != nil {
		return err
	}
	defer screen.Fini()
	mouse, hasMouse := model.(MouseModel)
	ticker, hasTick := model.(TickModel)
	if hasMouse {
		screen.EnableMouse(tcell.MouseButtonEvents)
	}

	go func() {
		for evt := range evts {
			screen.PostEvent(&eventDev{evt: evt})
		}
		screen.PostEvent(&eventDev{})
	}()
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				screen.PostEvent(&eventDev{})
				return
			case <-ticker.C:
				screen.PostEvent(&eventTick{})
			}
		}
	}()

	model.Draw(screen)
	dirty := false
	lastDraw := time.Now()
	for {
		switch evt := screen.PollEvent().(type) {
		case nil:
			return nil
		case *tcell.EventResize:
			screen.Sync()
			model.Draw(screen)
		case *tcell.EventKey:
			if !model.HandleKey(evt) {
				return nil
			}
			model.Draw(screen)
		case *tcell.EventMouse:
			if !hasMouse || evt.Buttons() == tcell.ButtonNone {
				continue
			}
			mouse.HandleMouse(evt)
			model.Draw(screen)
		case *eventDev:
			if evt.evt == nil {
				return nil
			}
			if model.Apply(evt.evt) {
				dirty = true
			}
		case *eventTick:
			if hasTick && ticker.Tick() {
				dirty = true
			}
			if dirty || time.Since(lastDraw) > time.Second {
				model.Draw(screen)
				dirty = false
				lastDraw = time.Now()
			}
		}
	}
}

// Purge asks the dev server to purge a queue. The outcome arrives as a
// QueuePurgedEvent on the stream.
func Purge(ctx context.Context, serverURL string, queue string, region string) {
	Post(ctx, serverURL, "/api/sqs/purge", map[string]string{"queue": queue, "region": region})
}

// Post sends a JSON request to the dev server in the background. Outcomes
// arrive as events on the stream, so the response is dropped.
func Post(ctx context.Context, serverURL string, path string, payload interface{}) {
	go func() {
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+path, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
}

var (
	Normal  = tcell.StyleDefault
	Dim     = tcell.StyleDefault.Foreground(tcell.ColorGray)
	Bold    = tcell.StyleDefault.Bold(true)
	Title   = tcell.StyleDefault.Foreground(tcell.ColorTeal).Bold(true)
	Cursor  = tcell.StyleDefault.Reverse(true)
	Danger  = tcell.StyleDefault.Foreground(tcell.ColorRed)
	Warning = tcell.StyleDefault.Foreground(tcell.ColorYellow)
	Success = tcell.StyleDefault.Foreground(tcell.ColorGreen)
	Banner  = tcell.StyleDefault.Background(tcell.ColorDarkRed).Foreground(tcell.ColorWhite).Bold(true)
)

// Text draws text from x on row y, clipped to the screen, and returns the
// column after it.
func Text(screen tcell.Screen, x int, y int, text string, style tcell.Style) int {
	width, _ := screen.Size()
	for _, r := range text {
		if x >= width {
			break
		}
		screen.SetContent(x, y, r, nil, style)
		x++
	}
	return x
}

// Fill paints row y.
func Fill(screen tcell.Screen, y int, style tcell.Style) {
	width, _ := screen.Size()
	for x := 0; x < width; x++ {
		screen.SetContent(x, y, ' ', nil, style)
	}
}

// DurationStyle colours a duration: under a second is fine, under three is
// slow, anything longer is a problem.
func DurationStyle(d time.Duration) tcell.Style {
	switch {
	case d < time.Second:
		return Success
	case d < 3*time.Second:
		return Warning
	default:
		return Danger
	}
}

func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// Ago is a coarse relative time.
func Ago(now time.Time, t time.Time) string {
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

// ShortID is the first eight characters of an id.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
