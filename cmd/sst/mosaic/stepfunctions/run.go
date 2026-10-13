package stepfunctions

import (
	"context"
	"os"
	"time"

	"github.com/atotto/clipboard"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/cmd/sst/mosaic/panekit"
)

// Run draws the Step Functions pane until ctx ends or the stream closes.
func Run(ctx context.Context, serverURL string, evts <-chan interface{}) error {
	fetch := func(arn string) { panekit.Post(ctx, serverURL, "/api/sfn/executions", map[string]string{"arn": arn}) }
	replay := func(req aws.StepFunctionReplayRequest) { panekit.Post(ctx, serverURL, "/api/sfn/replay", req) }
	app := NewApp(NewStore(), time.Now, fetch, replay, clipboard.WriteAll)
	app.SetStage(os.Getenv("SST_STAGE"))
	return panekit.Run(ctx, evts, app)
}
