package main

import (
	"github.com/sst/sst/v3/cmd/sst/cli"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/cmd/sst/mosaic/dev"
	"github.com/sst/sst/v3/cmd/sst/mosaic/multiplexer"
	"github.com/sst/sst/v3/cmd/sst/mosaic/stepfunctions"
	"github.com/sst/sst/v3/pkg/project"
	"github.com/sst/sst/v3/pkg/server"
)

// stepFunctionsFilter is the `sst ui --filter` value that runs the Step
// Functions pane.
const stepFunctionsFilter = "stepfunctions"

// CmdStepFunctions runs the Step Functions pane against the dev server.
func CmdStepFunctions(c *cli.Cli) error {
	url, err := server.Discover("", "")
	if err != nil {
		return err
	}
	evts, err := dev.Stream(c.Context, url,
		project.CompleteEvent{},
		aws.StepFunctionExecutionsEvent{},
		aws.StepFunctionReplayStartedEvent{},
		aws.StepFunctionReplayEvent{},
	)
	if err != nil {
		return err
	}
	return stepfunctions.Run(c.Context, url, evts)
}

// stepFunctionsPane is the multiplexer pane for CmdStepFunctions; killable
// so it can take focus for its keys.
func stepFunctionsPane(executable string, env []string, logPath string) multiplexer.PaneConfig {
	return multiplexer.PaneConfig{
		Key:       stepFunctionsFilter,
		Args:      []string{executable, "ui", "--filter=" + stepFunctionsFilter},
		Icon:      "⛓",
		Title:     "Step Functions",
		Killable:  true,
		Autostart: true,
		Env:       append(env, "SST_LOG="+logPath),
	}
}
