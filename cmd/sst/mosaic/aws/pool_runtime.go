package aws

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws/bridge"
	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/flag"
	"github.com/sst/sst/v3/pkg/runtime"
)

// poolRuntime connects the Pool to the world the dev loop lives in: the
// Runtime API handlers pooled workers call, the bridge messages stubs send,
// process start and stop, log attribution, the bundle watcher and stats.
// Every method returns false when the worker or source is not pooled, so
// function.go keeps its upstream path for everything else.
type poolRuntime struct {
	ctx     context.Context
	input   input
	log     *slog.Logger
	server  string
	pool    *Pool
	cfg     PoolConfig
	build   func(functionID string) *runtime.BuildOutput
	targets func(functionID string) (*runtime.BuildInput, bool)
	// Worker ids whose process exited.
	stopped chan string
}

func newPoolRuntime(ctx context.Context, in input, server string, cfg PoolConfig) *poolRuntime {
	r := &poolRuntime{
		ctx:     ctx,
		input:   in,
		log:     slog.Default().With("service", "aws.pool"),
		server:  server,
		cfg:     cfg,
		stopped: make(chan string, 1000),
	}
	r.pool = NewPool(cfg, r.startWorker, r.stopWorker)
	go r.statsLoop()
	return r
}

// pooledProperties is the slice of BuildInput.Properties the pool reads.
type pooledProperties struct {
	Pool string `json:"pool"`
}

// target registers a function's build input. A function is pooled when it
// runs a prebuilt bundle and asked for `pool: "bundle"`.
func (r *poolRuntime) target(evt *runtime.BuildInput) bool {
	if evt.Bundle == "" || len(evt.Properties) == 0 {
		return false
	}
	var props pooledProperties
	if err := json.Unmarshal(evt.Properties, &props); err != nil || props.Pool != "bundle" {
		return false
	}
	key, fresh := r.pool.Target(evt.FunctionID, evt.Runtime, evt.Bundle)
	if fresh {
		r.log.Info("pool created", "bundle", evt.Bundle, "runtime", evt.Runtime)
		go r.watch(key, evt.Bundle, evt.Handler)
	}
	return true
}

// init handles a stub's Init for a pooled function: the source is bound to
// its function and environment, and invocations that arrived before the Init
// (parked by the upstream loop) are dispatched. No worker starts per stub.
func (r *poolRuntime) init(source string, functionID string, env []string, parked <-chan io.Reader) bool {
	if !r.pool.IsPooled(functionID) {
		return false
	}
	env = ApplyDevEnvOverrides(env, os.Environ(), flag.SST_DEV_ENV_OVERRIDES)
	for {
		var reader io.Reader
		select {
		case reader = <-parked:
		default:
		}
		if reader == nil {
			break
		}
		if inv, ok := r.parse(source, reader); ok {
			r.pool.Orphan(source, inv)
		}
	}
	r.log.Info("pooled source registered", "source", source, "functionID", functionID)
	for _, inv := range r.pool.Register(source, functionID, env) {
		r.dispatch(inv)
	}
	return true
}

// next handles a stub's Next when the source is pooled.
func (r *poolRuntime) next(source string, body io.Reader) bool {
	if _, ok := r.pool.Source(source); !ok {
		return false
	}
	inv, ok := r.parse(source, body)
	if !ok {
		return true
	}
	r.dispatch(inv)
	return true
}

func (r *poolRuntime) dispatch(inv *pendingInvocation) {
	if err := r.pool.Dispatch(inv); err != nil {
		r.log.Error("dispatch failed", "source", inv.Source, "requestID", inv.RequestID, "error", err)
		r.fail(inv, bridge.MessageError, "PoolDispatchError", err.Error())
	}
}

// parse reads the raw Runtime API `/next` response a stub forwarded.
func (r *poolRuntime) parse(source string, body io.Reader) (*pendingInvocation, bool) {
	resp, err := http.ReadResponse(bufio.NewReader(body), nil)
	if err != nil {
		r.log.Error("cannot parse forwarded invocation", "source", source, "error", err)
		return nil, false
	}
	defer resp.Body.Close()
	event, err := io.ReadAll(resp.Body)
	if err != nil {
		r.log.Error("cannot read forwarded event", "source", source, "error", err)
		return nil, false
	}
	return &pendingInvocation{
		RequestID: resp.Header.Get("lambda-runtime-aws-request-id"),
		Source:    source,
		Header:    resp.Header,
		Body:      event,
	}, true
}

// fail answers a request on behalf of a worker that cannot run it.
func (r *poolRuntime) fail(inv *pendingInvocation, message bridge.MessageType, errorType string, errorMessage string) {
	writer := r.input.client.NewWriter(message, r.input.prefix+"/"+inv.Source+"/in")
	writer.SetID(inv.RequestID)
	json.NewEncoder(writer).Encode(map[string]interface{}{
		"errorType":    errorType,
		"errorMessage": errorMessage,
		"trace":        []string{},
	})
	writer.Close()
	bus.Publish(&FunctionErrorEvent{
		FunctionID:   inv.FunctionID,
		WorkerID:     inv.Source,
		RequestID:    inv.RequestID,
		ErrorType:    errorType,
		ErrorMessage: errorMessage,
	})
}

// startWorker is the pool's StartWorker: it runs the bundle with the pooled
// entry point, seeded with the environment of one of the functions it serves,
// and forwards its log lines under the request they belong to.
func (r *poolRuntime) startWorker(key poolKey, functionID string, id string) (runtime.Worker, error) {
	build := r.build(functionID)
	if build == nil {
		return nil, errBuildFailed
	}
	target, ok := r.targets(functionID)
	if !ok {
		return nil, errBuildFailed
	}
	env := r.seedEnv(functionID)
	worker, err := r.input.project.Runtime.Run(r.ctx, &runtime.RunInput{
		CfgPath:    r.input.project.PathConfig(),
		Runtime:    target.Runtime,
		Server:     r.server + id,
		WorkerID:   id,
		FunctionID: functionID,
		Build:      build,
		Env:        append(env, "SST_POOL=bundle"),
		Pool: &runtime.PoolRun{
			Concurrency: r.cfg.Concurrency,
			MaxHeapMB:   r.cfg.MaxHeapMB,
			SourceMaps:  r.cfg.SourceMaps,
			DebugMemory: r.cfg.DebugMemory,
		},
	})
	if err != nil {
		return nil, err
	}
	r.log.Info("pooled worker started", "workerID", id, "functionID", functionID)
	go func() {
		scanner := bufio.NewScanner(worker.Logs())
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			inv, text, memory := r.pool.Attribute(id, scanner.Text())
			if memory {
				r.log.Debug("worker memory", "workerID", id, "report", text)
				continue
			}
			evt := &FunctionLogEvent{WorkerID: id, Line: text}
			if inv != nil {
				evt.FunctionID = inv.FunctionID
				evt.WorkerID = inv.Source
				evt.RequestID = inv.RequestID
			} else {
				evt.FunctionID = functionID
			}
			bus.Publish(evt)
		}
		r.stopped <- id
	}()
	return worker, nil
}

func (r *poolRuntime) stopWorker(w *pooledWorker) {
	r.log.Info("pooled worker stopped", "workerID", w.ID)
	if w.Worker != nil {
		w.Worker.Stop()
	}
}

// seedEnv is the process environment of a new worker: the forwarded env of
// any registered stub of the function, so module-scope reads see realistic
// values. Per-invocation env arrives with each request.
func (r *poolRuntime) seedEnv(functionID string) []string {
	r.pool.mu.Lock()
	defer r.pool.mu.Unlock()
	for _, info := range r.pool.sources {
		if info.FunctionID == functionID {
			return append([]string{}, info.Env...)
		}
	}
	for _, info := range r.pool.sources {
		return append([]string{}, info.Env...)
	}
	return ApplyDevEnvOverrides(nil, os.Environ(), flag.SST_DEV_ENV_OVERRIDES)
}

// workerDied handles a pooled worker process exit.
func (r *poolRuntime) workerDied(id string) {
	for _, inv := range r.pool.WorkerDied(id) {
		r.fail(inv, bridge.MessageError, "WorkerExited", "the pooled worker exited while running this invocation")
	}
	r.log.Info("pooled worker died", "workerID", id)
}

// ---- Runtime API handlers ----

type pooledNextBody struct {
	Event json.RawMessage   `json:"event"`
	Env   map[string]string `json:"env"`
}

// handleNext serves a pooled worker's `/next`: the event is wrapped with the
// per-invocation environment, and the function is named in headers.
func (r *poolRuntime) handleNext(w http.ResponseWriter, req *http.Request, workerID string) bool {
	ch, ok := r.pool.Next(workerID)
	if !ok {
		return false
	}
	select {
	case <-req.Context().Done():
		return true
	case <-r.ctx.Done():
		return true
	case inv := <-ch:
		info, _ := r.pool.Source(inv.Source)
		env := map[string]string{}
		if info != nil {
			env = envMap(info.Env)
		}
		for key, values := range inv.Header {
			// The body is re-wrapped below, so the stub's framing headers do not apply.
			switch strings.ToLower(key) {
			case "content-length", "content-type", "transfer-encoding", "connection":
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.Header().Set("Lambda-Runtime-Sst-Function-Id", inv.FunctionID)
		w.Header().Set("Lambda-Runtime-Sst-Source", inv.Source)
		w.Header().Set("Lambda-Runtime-Function-Name", env["AWS_LAMBDA_FUNCTION_NAME"])
		w.Header().Set("Lambda-Runtime-Function-Version", env["AWS_LAMBDA_FUNCTION_VERSION"])
		w.Header().Set("Lambda-Runtime-Function-Memory-Size", env["AWS_LAMBDA_FUNCTION_MEMORY_SIZE"])
		event := inv.Body
		if !json.Valid(event) {
			quoted, _ := json.Marshal(string(event))
			event = quoted
		}
		body, _ := json.Marshal(pooledNextBody{Event: event, Env: env})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		w.Write(body)
		// A worker that describes requests posts the handler label before it
		// runs the handler; the event is published then (handleDescribe).
		if req.Header.Get(describeHeader) == "" {
			r.announce(workerID, inv.RequestID, describeBody{})
		}
		return true
	}
}

// describeHeader is set on `/next` by a worker whose bundle exports
// `describe(event)`.
const describeHeader = "Lambda-Runtime-Sst-Describe"

type describeBody struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// announce publishes a held request's FunctionInvokedEvent once.
func (r *poolRuntime) announce(workerID string, requestID string, desc describeBody) {
	inv, ok := r.pool.Announce(workerID, requestID)
	if !ok {
		return
	}
	bus.Publish(&FunctionInvokedEvent{
		FunctionID: inv.FunctionID,
		WorkerID:   inv.Source,
		RequestID:  inv.RequestID,
		Input:      inv.Body,
		Handler:    desc.Key,
		Label:      desc.Label,
	})
}

// handleDescribe receives the handler a pooled worker resolved for a request.
func (r *poolRuntime) handleDescribe(w http.ResponseWriter, req *http.Request, workerID string, requestID string) bool {
	if _, ok := r.pool.Next(workerID); !ok {
		return false
	}
	var desc describeBody
	json.NewDecoder(req.Body).Decode(&desc)
	r.announce(workerID, requestID, desc)
	w.WriteHeader(202)
	return true
}

// handleResponse forwards a pooled worker's response to the stub that asked.
func (r *poolRuntime) handleResponse(w http.ResponseWriter, req *http.Request, workerID string, requestID string) bool {
	r.announce(workerID, requestID, describeBody{})
	inv, ok := r.pool.Complete(workerID, requestID)
	if !ok {
		return false
	}
	writer := r.input.client.NewWriter(bridge.MessageResponse, r.input.prefix+"/"+inv.Source+"/in")
	writer.SetID(requestID)
	var buf bytes.Buffer
	io.Copy(writer, io.TeeReader(req.Body, &buf))
	writer.Close()
	w.WriteHeader(202)
	bus.Publish(&FunctionResponseEvent{
		FunctionID: inv.FunctionID,
		WorkerID:   inv.Source,
		RequestID:  requestID,
		Output:     buf.Bytes(),
	})
	return true
}

// handleError forwards a pooled worker's invocation error to the stub.
func (r *poolRuntime) handleError(w http.ResponseWriter, req *http.Request, workerID string, requestID string) bool {
	r.announce(workerID, requestID, describeBody{})
	inv, ok := r.pool.Complete(workerID, requestID)
	if !ok {
		return false
	}
	writer := r.input.client.NewWriter(bridge.MessageError, r.input.prefix+"/"+inv.Source+"/in")
	writer.SetID(requestID)
	var buf bytes.Buffer
	io.Copy(writer, io.TeeReader(req.Body, &buf))
	writer.Close()
	w.WriteHeader(202)
	fee := &FunctionErrorEvent{FunctionID: inv.FunctionID, WorkerID: inv.Source, RequestID: requestID}
	json.Unmarshal(buf.Bytes(), fee)
	bus.Publish(fee)
	return true
}

// handleInitError handles a pooled worker that failed to load the bundle:
// every request it holds is failed with an init error, and the worker is
// forgotten so a later invocation starts a fresh one.
func (r *poolRuntime) handleInitError(w http.ResponseWriter, req *http.Request, workerID string) bool {
	if _, ok := r.pool.Next(workerID); !ok {
		return false
	}
	body, _ := io.ReadAll(req.Body)
	message := strings.TrimSpace(string(body))
	w.WriteHeader(202)
	for _, inv := range r.pool.WorkerDied(workerID) {
		writer := r.input.client.NewWriter(bridge.MessageInitError, r.input.prefix+"/"+inv.Source+"/in")
		writer.SetID(inv.RequestID)
		writer.Write(body)
		writer.Close()
		fee := &FunctionErrorEvent{FunctionID: inv.FunctionID, WorkerID: inv.Source, RequestID: inv.RequestID}
		json.Unmarshal(body, fee)
		bus.Publish(fee)
	}
	r.log.Error("pooled worker failed to initialize", "workerID", workerID, "error", message)
	return true
}

// ---- bundle watcher and stats ----

// watch polls the bundle's rebuild stamp so the pool can retire workers that
// loaded an older bundle and warm workers once a rebuild has settled. The
// stamp is `.last-rebuild` when the bundler writes one, else the entry file.
func (r *poolRuntime) watch(key poolKey, bundle string, handler string) {
	entry := handlerFile(bundle, handler)
	stampFile := filepath.Join(bundle, ".last-rebuild")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		ready := false
		if _, err := os.Stat(entry); err == nil {
			ready = true
		}
		var stamp time.Time
		if info, err := os.Stat(stampFile); err == nil {
			stamp = info.ModTime()
		} else if info, err := os.Stat(entry); err == nil {
			stamp = info.ModTime()
		}
		r.pool.Tick(key, stamp, ready)
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// handlerFile resolves `index.handler` inside a bundle to its file.
func handlerFile(bundle string, handler string) string {
	base := handler
	if i := strings.LastIndex(handler, "."); i >= 0 {
		base = handler[:i]
	}
	for _, ext := range []string{".mjs", ".js", ".cjs"} {
		file := filepath.Join(bundle, base+ext)
		if _, err := os.Stat(file); err == nil {
			return file
		}
	}
	return filepath.Join(bundle, base+".mjs")
}

func (r *poolRuntime) statsLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			for _, stat := range r.pool.Stats() {
				bus.Publish(&stat)
				r.log.Info("pool", "bundle", stat.Bundle, "workers", stat.Workers, "inFlight", stat.InFlight, "queued", stat.Queued)
			}
		}
	}
}

var errBuildFailed = errPoolBuild("function failed to build")

type errPoolBuild string

func (e errPoolBuild) Error() string { return string(e) }
