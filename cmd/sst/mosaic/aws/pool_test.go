package aws

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sst/sst/v3/pkg/runtime"
)

type fakeWorker struct{ id string }

func (f *fakeWorker) Stop()               {}
func (f *fakeWorker) Logs() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }

type fakes struct {
	started []string
	stopped []string
	fail    bool
}

func newTestPool(cfg PoolConfig) (*Pool, *fakes) {
	f := &fakes{}
	p := NewPool(cfg,
		func(key poolKey, functionID string, id string, env []string) (runtime.Worker, error) {
			if f.fail {
				return nil, io.ErrUnexpectedEOF
			}
			f.started = append(f.started, id)
			return &fakeWorker{id: id}, nil
		},
		func(w *pooledWorker) { f.stopped = append(f.stopped, w.ID) },
	)
	p.quiet = 0
	return p, f
}

func ready(p *Pool, key poolKey) {
	p.Tick(key, time.Unix(1, 0), true)
}

func invocation(source, requestID string) *pendingInvocation {
	return &pendingInvocation{RequestID: requestID, Source: source, Body: []byte(`{"n":1}`)}
}

func testConfig() PoolConfig {
	return PoolConfig{Size: 2, Concurrency: 2, IdleTimeout: time.Hour, Warmup: 0}
}

func TestDispatchPacksIntoTheBusiestWorkerWithCapacity(t *testing.T) {
	p, f := newTestPool(testConfig())
	key, _ := p.Target("fnA", "nodejs22.x", "/bundle")
	p.Target("fnB", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("srcA", "fnA", []string{"HANDLER_FN=a"})
	p.Register("srcB", "fnB", []string{"HANDLER_FN=b"})

	if err := p.Dispatch(invocation("srcA", "r1")); err != nil {
		t.Fatal(err)
	}
	if err := p.Dispatch(invocation("srcB", "r2")); err != nil {
		t.Fatal(err)
	}
	if len(f.started) != 1 {
		t.Fatalf("two invocations under the concurrency limit must share one worker, started %v", f.started)
	}
	ch, _ := p.Next(f.started[0])
	first, second := <-ch, <-ch
	if first.FunctionID != "fnA" || second.FunctionID != "fnB" {
		t.Fatalf("functions were not carried through: %s %s", first.FunctionID, second.FunctionID)
	}
	// A third invocation exceeds the worker's concurrency and starts a second worker.
	p.Dispatch(invocation("srcA", "r3"))
	if len(f.started) != 2 {
		t.Fatalf("expected a second worker, started %v", f.started)
	}
}

func TestDispatchQueuesAtTheCapAndDrainsOnComplete(t *testing.T) {
	p, f := newTestPool(PoolConfig{Size: 1, Concurrency: 1, IdleTimeout: time.Hour})
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("src", "fn", nil)
	p.Dispatch(invocation("src", "r1"))
	p.Dispatch(invocation("src", "r2"))
	if len(f.started) != 1 {
		t.Fatalf("cap of one worker ignored: %v", f.started)
	}
	stats := p.Stats()
	if stats[0].Queued != 1 || stats[0].InFlight != 1 {
		t.Fatalf("expected one queued and one in flight, got %+v", stats[0])
	}
	ch, _ := p.Next(f.started[0])
	if got := <-ch; got.RequestID != "r1" {
		t.Fatalf("expected r1 first, got %s", got.RequestID)
	}
	inv, ok := p.Complete(f.started[0], "r1")
	if !ok || inv.Source != "src" {
		t.Fatalf("complete did not return the request: %v %v", inv, ok)
	}
	stats = p.Stats()
	if stats[0].Queued != 0 || stats[0].InFlight != 1 {
		t.Fatalf("queued request was not dispatched after completion: %+v", stats[0])
	}
	if got := <-ch; got.RequestID != "r2" {
		t.Fatalf("expected r2 next, got %s", got.RequestID)
	}
}

func TestDuplicateRequestIsIgnored(t *testing.T) {
	p, f := newTestPool(testConfig())
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("src", "fn", nil)
	p.Dispatch(invocation("src", "r1"))
	p.Dispatch(invocation("src", "r1"))
	if p.Stats()[0].InFlight != 1 {
		t.Fatalf("a re-sent request must not run twice: %+v", p.Stats()[0])
	}
	if len(f.started) != 1 {
		t.Fatalf("started %v", f.started)
	}
}

func TestInvalidateStopsIdleWorkersAndDrainsBusyOnes(t *testing.T) {
	p, f := newTestPool(PoolConfig{Size: 2, Concurrency: 1, IdleTimeout: time.Hour})
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("src", "fn", nil)
	p.Dispatch(invocation("src", "r1"))
	p.Dispatch(invocation("src", "r2"))
	p.Complete(f.started[1], "r2")
	p.Invalidate(key)
	if len(f.stopped) != 1 || f.stopped[0] != f.started[1] {
		t.Fatalf("the idle worker must stop now, stopped %v", f.stopped)
	}
	p.Complete(f.started[0], "r1")
	if len(f.stopped) != 2 {
		t.Fatalf("the busy worker must stop once it drains, stopped %v", f.stopped)
	}
	// A new invocation starts a fresh worker from the new bundle.
	p.Dispatch(invocation("src", "r3"))
	if len(f.started) != 3 {
		t.Fatalf("expected a fresh worker, started %v", f.started)
	}
}

func TestNewerStampInvalidatesAndQuietBundleWarms(t *testing.T) {
	p, f := newTestPool(PoolConfig{Size: 2, Concurrency: 1, IdleTimeout: time.Hour, Warmup: 1})
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	p.Tick(key, time.Unix(1, 0), false)
	if len(f.started) != 0 {
		t.Fatalf("a bundle that is not ready must not warm: %v", f.started)
	}
	p.Tick(key, time.Unix(1, 0), true)
	if len(f.started) != 1 {
		t.Fatalf("a ready, quiet bundle warms one worker: %v", f.started)
	}
	p.Tick(key, time.Unix(2, 0), true)
	if len(f.stopped) != 1 || len(f.started) != 2 {
		t.Fatalf("a newer stamp retires the warm worker and warms a fresh one: stopped %v started %v", f.stopped, f.started)
	}
}

func TestIdleWorkerIsRetiredAndReplacedOnDemand(t *testing.T) {
	p, f := newTestPool(PoolConfig{Size: 1, Concurrency: 1, IdleTimeout: 10 * time.Millisecond})
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("src", "fn", nil)
	p.Dispatch(invocation("src", "r1"))
	p.Complete(f.started[0], "r1")
	deadline := time.Now().Add(2 * time.Second)
	for len(f.stopped) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(f.stopped) != 1 {
		t.Fatalf("idle worker was not retired")
	}
	p.Dispatch(invocation("src", "r2"))
	if len(f.started) != 2 {
		t.Fatalf("a new invocation after retirement starts a fresh worker: %v", f.started)
	}
}

func TestWorkerDeathFailsHeldRequests(t *testing.T) {
	p, f := newTestPool(PoolConfig{Size: 1, Concurrency: 2, IdleTimeout: time.Hour})
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("src", "fn", nil)
	p.Dispatch(invocation("src", "r1"))
	p.Dispatch(invocation("src", "r2"))
	held := p.WorkerDied(f.started[0])
	if len(held) != 2 {
		t.Fatalf("expected both held requests, got %d", len(held))
	}
	if _, ok := p.Next(f.started[0]); ok {
		t.Fatalf("dead worker must be forgotten")
	}
	if len(f.started) != 1 {
		t.Fatalf("no replacement without waiting work: %v", f.started)
	}
	// The same request ids may be re-sent after the failure.
	p.Dispatch(invocation("src", "r1"))
	if len(f.started) != 2 {
		t.Fatalf("a re-sent request after a crash starts a worker: %v", f.started)
	}
}

func TestOrphansAreDispatchedAfterALateInit(t *testing.T) {
	p, f := newTestPool(testConfig())
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Orphan("src", invocation("src", "r1"))
	if err := p.Dispatch(invocation("src", "r2")); err == nil {
		t.Fatalf("dispatch before Init must fail")
	}
	orphans := p.Register("src", "fn", nil)
	if len(orphans) != 1 || orphans[0].FunctionID != "fn" {
		t.Fatalf("orphans were not returned with their function: %+v", orphans)
	}
	for _, inv := range orphans {
		p.Dispatch(inv)
	}
	if len(f.started) != 1 || p.Stats()[0].InFlight != 1 {
		t.Fatalf("orphan was not dispatched: %v %+v", f.started, p.Stats())
	}
}

func TestStartFailureLeavesTheRequestWaiting(t *testing.T) {
	p, f := newTestPool(testConfig())
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("src", "fn", nil)
	f.fail = true
	p.Dispatch(invocation("src", "r1"))
	if p.Stats()[0].Queued != 1 || p.Stats()[0].Workers != 0 {
		t.Fatalf("a failed start must keep the request queued with no worker: %+v", p.Stats()[0])
	}
	f.fail = false
	ready(p, key)
	if p.Stats()[0].InFlight != 1 {
		t.Fatalf("the queued request runs once a worker can start: %+v", p.Stats()[0])
	}
}

func TestRetargetInvalidatesTheOldKey(t *testing.T) {
	p, f := newTestPool(testConfig())
	key, fresh := p.Target("fn", "nodejs22.x", "/old")
	if !fresh {
		t.Fatalf("first target must report a new key")
	}
	ready(p, key)
	p.Register("src", "fn", nil)
	p.Dispatch(invocation("src", "r1"))
	p.Complete(f.started[0], "r1")
	_, fresh = p.Target("fn", "nodejs22.x", "/new")
	if !fresh || len(f.stopped) != 1 {
		t.Fatalf("moving to a new bundle retires the old idle worker: fresh=%v stopped=%v", fresh, f.stopped)
	}
}

func TestSplitTaggedAndAttribution(t *testing.T) {
	if id, text, ok := splitTagged("\x1eabc\x1ehello"); !ok || id != "abc" || text != "hello" {
		t.Fatalf("tagged line: %q %q %v", id, text, ok)
	}
	if _, text, ok := splitTagged("plain"); ok || text != "plain" {
		t.Fatalf("untagged line: %q %v", text, ok)
	}
	p, f := newTestPool(testConfig())
	key, _ := p.Target("fn", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("src", "fn", nil)
	p.Dispatch(invocation("src", "r1"))
	worker := f.started[0]
	if inv, text, _ := p.Attribute(worker, "\x1er1\x1eline"); inv == nil || inv.RequestID != "r1" || text != "line" {
		t.Fatalf("tagged attribution failed: %v %q", inv, text)
	}
	if inv, _, _ := p.Attribute(worker, "untagged"); inv == nil || inv.RequestID != "r1" {
		t.Fatalf("untagged lines go to the last dispatched request")
	}
	if inv, _, memory := p.Attribute(worker, "\x1esst.memory\x1e{\"rss\":1}"); !memory || inv != nil {
		t.Fatalf("memory report must be consumed, not attributed")
	}
	if p.Memory(worker) != `{"rss":1}` {
		t.Fatalf("memory report was not recorded: %q", p.Memory(worker))
	}
}

func TestParsePoolConfigDefaultsAndBounds(t *testing.T) {
	cfg := parsePoolConfig(poolEnv{})
	if cfg.Size != 4 || cfg.Concurrency != 10 || cfg.IdleTimeout != 5*time.Minute || cfg.Warmup != 1 || cfg.MaxHeapMB != 1024 {
		t.Fatalf("defaults: %+v", cfg)
	}
	cfg = parsePoolConfig(poolEnv{size: "2", warmup: "9", concurrency: "0", maxHeapMB: "-1", idleMs: "250"})
	if cfg.Size != 2 || cfg.Warmup != 2 || cfg.Concurrency != 1 || cfg.MaxHeapMB != 0 || cfg.IdleTimeout != 250*time.Millisecond {
		t.Fatalf("bounds: %+v", cfg)
	}
}

func TestAnnounceReturnsAHeldRequestOnce(t *testing.T) {
	p, f := newTestPool(testConfig())
	key, _ := p.Target("fnA", "nodejs22.x", "/bundle")
	ready(p, key)
	p.Register("srcA", "fnA", nil)
	p.Dispatch(invocation("srcA", "r1"))
	worker := f.started[0]

	inv, ok := p.Announce(worker, "r1")
	if !ok || inv.FunctionID != "fnA" || inv.Source != "srcA" {
		t.Fatalf("the first announce must return the held request, got %+v %v", inv, ok)
	}
	if _, ok := p.Announce(worker, "r1"); ok {
		t.Fatal("a request must be announced only once")
	}
	if _, ok := p.Announce(worker, "unknown"); ok {
		t.Fatal("an unknown request must not be announced")
	}
	p.Complete(worker, "r1")
	if _, ok := p.Announce(worker, "r1"); ok {
		t.Fatal("a completed request must not be announced")
	}
}

func TestWarmStartsAWorkerWithASourceEnvWithoutReenteringThePool(t *testing.T) {
	var gotEnv []string
	var p *Pool
	p = NewPool(PoolConfig{Size: 2, Concurrency: 2, IdleTimeout: time.Hour, Warmup: 1},
		func(key poolKey, functionID string, id string, env []string) (runtime.Worker, error) {
			gotEnv = env
			return &fakeWorker{id: id}, nil
		},
		func(w *pooledWorker) {},
	)
	p.quiet = 0
	key, _ := p.Target("fnA", "nodejs22.x", "/bundle")
	p.Register("srcA", "fnA", []string{"HANDLER_FN=a"})

	done := make(chan struct{})
	go func() {
		ready(p, key)
		// The pool must still be usable after warming.
		p.Source("srcA")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("warming a worker deadlocked the pool")
	}
	if len(gotEnv) != 1 || gotEnv[0] != "HANDLER_FN=a" {
		t.Fatalf("the warm worker must start with the source env, got %v", gotEnv)
	}
}

func TestPoolWorkerEnvListsTheSeedKeys(t *testing.T) {
	got := poolWorkerEnv([]string{"HANDLER_FN=a", "AWS_REGION=us-east-1"})
	want := []string{"HANDLER_FN=a", "AWS_REGION=us-east-1", "SST_POOL=bundle", "SST_POOL_SEED_KEYS=HANDLER_FN,AWS_REGION"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %v want %v", got, want)
	}
}
