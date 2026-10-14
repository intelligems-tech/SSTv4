package queues

import (
	"context"
	"os"
	"time"

	"github.com/atotto/clipboard"
	"github.com/sst/sst/v3/cmd/sst/mosaic/panekit"
)

// Run draws the Queues pane until ctx ends or the stream closes.
func Run(ctx context.Context, serverURL string, evts <-chan interface{}) error {
	purge := func(queue string, region string) { panekit.Purge(ctx, serverURL, queue, region) }
	app := NewApp(NewStore(time.Now), time.Now, purge, clipboard.WriteAll)
	app.SetStage(os.Getenv("SST_STAGE"))
	return panekit.Run(ctx, evts, app)
}
