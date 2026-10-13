package main

import (
	"os"

	"github.com/sst/sst/v3/cmd/sst/cli"
	"github.com/sst/sst/v3/pkg/prettyjson"
)

// prettyJSONFlag is `sst dev --pretty-json`.
var prettyJSONFlag = cli.Flag{
	Name: "pretty-json",
	Type: "bool",
	Description: cli.Description{
		Short: "Pretty print JSON in function logs",
		Long:  "Pretty print JSON in function logs, inputs and outputs, decoding JSON nested in strings. Same as SST_PRETTY_JSON=1.",
	},
}

// withPrettyJSON turns pretty JSON on for this process and the panes it
// starts when `--pretty-json` is passed.
func withPrettyJSON(c *cli.Cli, env []string) []string {
	if !c.Bool("pretty-json") {
		return env
	}
	os.Setenv(prettyjson.Env, "true")
	return append(env, prettyjson.Env+"=true")
}
