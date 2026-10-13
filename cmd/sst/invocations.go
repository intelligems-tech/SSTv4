package main

import (
	"github.com/sst/sst/v3/cmd/sst/cli"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/cmd/sst/mosaic/dev"
	"github.com/sst/sst/v3/cmd/sst/mosaic/invocations"
	"github.com/sst/sst/v3/cmd/sst/mosaic/multiplexer"
	"github.com/sst/sst/v3/pkg/project"
	"github.com/sst/sst/v3/pkg/server"
)

// invocationsFilter is the `sst ui --filter` value that runs the pane.
const invocationsFilter = "invocations"

// CmdInvocations runs the Invocations pane against the dev server.
func CmdInvocations(c *cli.Cli) error {
	url, err := server.Discover("", "")
	if err != nil {
		return err
	}
	evts, err := dev.Stream(c.Context, url,
		project.CompleteEvent{},
		aws.FunctionInvokedEvent{},
		aws.FunctionLogEvent{},
		aws.FunctionResponseEvent{},
		aws.FunctionErrorEvent{},
		aws.QueueStallsEvent{},
		aws.QueuePurgedEvent{},
	)
	if err != nil {
		return err
	}
	return invocations.Run(c.Context, url, evts)
}

// invocationsPane is the multiplexer pane for CmdInvocations. It is
// killable so it can take focus for its keys.
func invocationsPane(executable string, env []string, logPath string) multiplexer.PaneConfig {
	return multiplexer.PaneConfig{
		Key:       invocationsFilter,
		Args:      []string{executable, "ui", "--filter=" + invocationsFilter},
		Icon:      "◎",
		Title:     "Invocations",
		Killable:  true,
		Autostart: true,
		Env:       append(env, "SST_LOG="+logPath),
	}
}
