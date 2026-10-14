package node

import (
	"strings"
	"testing"

	"github.com/sst/sst/v3/pkg/runtime"
)

func TestBuildCommandPooled(t *testing.T) {
	getenv := func(key string) string {
		if key == "NODE_OPTIONS" {
			return "--stack-size=2000"
		}
		return ""
	}
	input := &runtime.RunInput{
		CfgPath:  "/repo/sst.config.ts",
		Server:   "localhost:1234/lambda/pool-1",
		WorkerID: "pool-1",
		Build:    &runtime.BuildOutput{Out: "/repo/.sst/dev-bundle", Handler: "index.handler"},
		Env:      []string{"HANDLER_FN=a"},
		Pool:     &runtime.PoolRun{Concurrency: 10, MaxHeapMB: 1024, SourceMaps: false, DebugMemory: true},
	}
	cmd := buildCommand(input, getenv)
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "dist/nodejs-runtime/pool.js /repo/.sst/dev-bundle/index.handler pool-1 10") {
		t.Fatalf("pooled entry and argv missing: %s", args)
	}
	if strings.Contains(args, "--enable-source-maps") {
		t.Fatalf("source maps must be off unless requested: %s", args)
	}
	envs := strings.Join(cmd.Env, "\n")
	if !strings.Contains(envs, "NODE_OPTIONS=--stack-size=2000 --max-old-space-size=1024") {
		t.Fatalf("heap cap missing from NODE_OPTIONS: %s", envs)
	}
	if !strings.Contains(envs, "SST_DEBUG_MEMORY=true") || !strings.Contains(envs, "AWS_LAMBDA_RUNTIME_API=localhost:1234/lambda/pool-1") {
		t.Fatalf("pooled env missing: %s", envs)
	}
	if cmd.Dir != "/repo/.sst/dev-bundle" {
		t.Fatalf("cwd: %s", cmd.Dir)
	}
}

func TestBuildCommandUnpooledIsUnchanged(t *testing.T) {
	input := &runtime.RunInput{
		CfgPath:  "/repo/sst.config.ts",
		Server:   "localhost:1234/lambda/w",
		WorkerID: "w",
		Build:    &runtime.BuildOutput{Out: "/out", Handler: "index.handler"},
	}
	cmd := buildCommand(input, func(string) string { return "" })
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--enable-source-maps --no-warnings") || !strings.Contains(args, "dist/nodejs-runtime/index.js /out/index.handler w") {
		t.Fatalf("unpooled argv changed: %s", args)
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "max-old-space-size") {
		t.Fatalf("unpooled workers must not get a heap cap")
	}
}
