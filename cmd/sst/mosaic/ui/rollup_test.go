package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/pkg/project"
)

const urnPrefix = "urn:pulumi:dev::app::"

func outputs(urn string, typ string, op apitype.OpType) *apitype.ResOutputsEvent {
	return &apitype.ResOutputsEvent{Metadata: apitype.StepEventMetadata{URN: urnPrefix + urn, Type: typ, Op: op}}
}

func TestDeployRollsUpdatesUpAndListsOtherChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ui.log")
	file, _ := os.Create(path)
	u := New(context.Background(), WithSilent, WithLog(file))
	u.Event(&project.StackCommandEvent{Command: "deploy", App: "app", Stage: "dev"})

	fn := "sst:aws:Function::OrdersFn"
	u.parents[urnPrefix+"sst:aws:Function$aws:lambda/function:Function::OrdersFnFunction"] = urnPrefix + fn
	u.parents[urnPrefix+"sst:aws:Function$aws:iam/role:Role::OrdersFnRole"] = urnPrefix + fn
	u.Event(outputs("sst:aws:Function$aws:lambda/function:Function::OrdersFnFunction", "aws:lambda/function:Function", apitype.OpUpdate))
	u.Event(outputs("sst:aws:Function$aws:iam/role:Role::OrdersFnRole", "aws:iam/role:Role", apitype.OpUpdate))
	u.Event(outputs("sst:aws:Function::BillingFn", "sst:aws:Function", apitype.OpUpdate))
	u.Event(outputs("sst:aws:Function::NewFn", "sst:aws:Function", apitype.OpCreate))
	u.Event(outputs("sst:aws:Function::NewFn", "sst:aws:Function", apitype.OpCreate))
	u.Event(outputs("aws:s3/bucketObjectv2:BucketObjectv2::DevBridgeCode", "aws:s3/bucketObjectv2:BucketObjectv2", apitype.OpDelete))
	u.Event(&project.CompleteEvent{Finished: true})
	file.Close()

	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{"+ λ NewFn", "created", "- ▣ DevBridgeCode", "deleted", "~ 2 updated   λ 2", "slowest"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Updated") || strings.Count(text, "NewFn") != 1 {
		t.Fatalf("updates must fold and a creation print once:\n%s", text)
	}
	if strings.Count(text, "OrdersFn") != 1 {
		t.Fatalf("children must count once under their component:\n%s", text)
	}
}

func TestRefreshKeepsPerResourceLines(t *testing.T) {
	u := New(context.Background(), WithSilent)
	u.mode = ProgressModeRefresh
	if u.rollupOutputs(outputs("sst:aws:Function::OrdersFn", "sst:aws:Function", apitype.OpSame), 0) {
		t.Fatal("refresh output is not rolled up")
	}
}
