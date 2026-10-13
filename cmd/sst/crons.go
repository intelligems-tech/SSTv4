package main

import (
	"github.com/sst/sst/v3/cmd/sst/cli"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/cmd/sst/mosaic/crons"
	"github.com/sst/sst/v3/cmd/sst/mosaic/dev"
	"github.com/sst/sst/v3/cmd/sst/mosaic/multiplexer"
	"github.com/sst/sst/v3/pkg/project"
	"github.com/sst/sst/v3/pkg/server"
)

// cronsFilter is the `sst ui --filter` value that runs the Crons pane.
const cronsFilter = "crons"

// CmdCrons runs the Crons pane against the dev server.
func CmdCrons(c *cli.Cli) error {
	url, err := server.Discover("", "")
	if err != nil {
		return err
	}
	evts, err := dev.Stream(c.Context, url,
		project.CompleteEvent{},
		aws.CronRunStartedEvent{},
		aws.CronRunEvent{},
	)
	if err != nil {
		return err
	}
	return crons.Run(c.Context, url, evts)
}

// cronsPane is the multiplexer pane for CmdCrons; killable so it can take
// focus for its keys.
func cronsPane(executable string, env []string, logPath string) multiplexer.PaneConfig {
	return multiplexer.PaneConfig{
		Key:       cronsFilter,
		Args:      []string{executable, "ui", "--filter=" + cronsFilter},
		Icon:      "⏱",
		Title:     "Crons",
		Killable:  true,
		Autostart: true,
		Env:       append(env, "SST_LOG="+logPath),
	}
}
