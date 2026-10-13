package invocations

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/atotto/clipboard"
	"github.com/gdamore/tcell/v2"
)

type eventDev struct {
	tcell.EventTime
	evt interface{}
}

type eventTick struct {
	tcell.EventTime
}

// Run draws the pane until ctx ends, the event stream closes or ctrl-c.
// Dev events are drawn at most every 100 ms so a log storm stays cheap.
func Run(ctx context.Context, serverURL string, evts <-chan interface{}) error {
	screen, err := tcell.NewScreen()
	if err != nil {
		return err
	}
	if err := screen.Init(); err != nil {
		return err
	}
	defer screen.Fini()
	screen.EnableMouse(tcell.MouseButtonEvents)

	app := NewApp(NewStore(time.Now), time.Now, httpPurger(ctx, serverURL), clipboard.WriteAll)
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

	app.Draw(screen)
	dirty := false
	lastDraw := time.Now()
	for {
		switch evt := screen.PollEvent().(type) {
		case nil:
			return nil
		case *tcell.EventResize:
			screen.Sync()
			app.Draw(screen)
		case *tcell.EventKey:
			if !app.HandleKey(evt) {
				return nil
			}
			app.Draw(screen)
		case *tcell.EventMouse:
			if evt.Buttons() == tcell.ButtonNone {
				continue
			}
			app.HandleMouse(evt)
			app.Draw(screen)
		case *eventDev:
			if evt.evt == nil {
				return nil
			}
			if app.Apply(evt.evt) {
				dirty = true
			}
		case *eventTick:
			// Redraw for new events, and once a second for running durations.
			if dirty || time.Since(lastDraw) > time.Second {
				app.Draw(screen)
				dirty = false
				lastDraw = time.Now()
			}
		}
	}
}

// httpPurger asks the dev server to purge a queue; the outcome arrives as a
// QueuePurgedEvent on the stream.
func httpPurger(ctx context.Context, serverURL string) Purger {
	return func(queue string, region string) {
		go func() {
			body, _ := json.Marshal(map[string]string{"queue": queue, "region": region})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/api/sqs/purge", bytes.NewReader(body))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
	}
}
