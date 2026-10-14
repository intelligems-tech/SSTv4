package main

import (
	"github.com/sst/sst/v3/cmd/sst/cli"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/cmd/sst/mosaic/dev"
	"github.com/sst/sst/v3/cmd/sst/mosaic/multiplexer"
	"github.com/sst/sst/v3/cmd/sst/mosaic/queues"
	"github.com/sst/sst/v3/pkg/project"
	"github.com/sst/sst/v3/pkg/server"
)

// queuesFilter is the `sst ui --filter` value that runs the Queues pane.
const queuesFilter = "queues"

// CmdQueues runs the Queues pane against the dev server.
func CmdQueues(c *cli.Cli) error {
	url, err := server.Discover("", "")
	if err != nil {
		return err
	}
	evts, err := dev.Stream(c.Context, url,
		project.CompleteEvent{},
		aws.FunctionInvokedEvent{},
		aws.FunctionResponseEvent{},
		aws.FunctionErrorEvent{},
		aws.QueueStallsEvent{},
		aws.QueueDepthsEvent{},
		aws.QueuePurgedEvent{},
	)
	if err != nil {
		return err
	}
	return queues.Run(c.Context, url, evts)
}

// queuesPane is the multiplexer pane for CmdQueues; killable so it can take
// focus for its keys.
func queuesPane(executable string, env []string, logPath string) multiplexer.PaneConfig {
	return multiplexer.PaneConfig{
		Key:       queuesFilter,
		Args:      []string{executable, "ui", "--filter=" + queuesFilter},
		Icon:      "≡",
		Title:     "Queues",
		Killable:  true,
		Autostart: true,
		Env:       append(env, "SST_LOG="+logPath),
	}
}
