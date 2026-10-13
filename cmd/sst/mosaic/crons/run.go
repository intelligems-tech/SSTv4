package crons

import (
	"context"
	"os"
	"time"

	"github.com/atotto/clipboard"
	"github.com/sst/sst/v3/cmd/sst/mosaic/panekit"
)

// Run draws the Crons pane until ctx ends or the stream closes.
func Run(ctx context.Context, serverURL string, evts <-chan interface{}) error {
	run := func(key string) { panekit.Post(ctx, serverURL, "/api/cron/run", map[string]string{"key": key}) }
	app := NewApp(NewStore(), time.Now, run, clipboard.WriteAll)
	app.SetStage(os.Getenv("SST_STAGE"))
	return panekit.Run(ctx, evts, app)
}
