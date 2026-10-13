package ui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/prettyjson"
)

func captureLog(t *testing.T, line string) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "ui")
	if err != nil {
		t.Fatal(err)
	}
	u := New(context.Background(), WithSilent, WithLog(file))
	u.Event(&aws.FunctionInvokedEvent{FunctionID: "Fn", WorkerID: "w", RequestID: "r"})
	u.Event(&aws.FunctionLogEvent{FunctionID: "Fn", WorkerID: "w", RequestID: "r", Line: line})
	data, _ := os.ReadFile(file.Name())
	return ansi.Strip(string(data))
}

func TestFunctionLogsPrettyPrintJSONWhenEnabled(t *testing.T) {
	line := `{"level":"info","payload":"{\"id\":1}"}`
	if out := captureLog(t, line); !strings.Contains(out, line) {
		t.Fatalf("pretty mode off must print the line as is:\n%s", out)
	}
	t.Setenv(prettyjson.Env, "1")
	out := captureLog(t, line)
	for _, want := range []string{`"level": "info",`, `"payload": {`, `"id": 1`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
}
