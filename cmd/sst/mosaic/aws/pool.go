package aws

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sst/sst/v3/pkg/runtime"
)

// A pool serves every function that shares one prebuilt bundle with a small
// set of long-lived local workers instead of one worker per Lambda instance.
// Invocations are packed into the busiest worker that still has capacity, so
// memory stays bounded by Size workers per key; a bundle rebuild retires
// workers as they drain; idle workers are stopped after IdleTimeout.
//
// This file is bookkeeping only. Starting and stopping processes, the Runtime
// API handlers and the bridge messages live in pool_runtime.go and are
// injected through StartWorker and StopWorker, which keeps the pool testable
// with fakes.

type poolKey string

// PoolStatsEvent is published every few seconds for each pool key so a UI can
// show how many workers serve it.
type PoolStatsEvent struct {
	Key      string
	Bundle   string
	Workers  int
	InFlight int
	Queued   int
}

// A pendingInvocation is one Lambda invocation forwarded by a stub, waiting
// for or held by a pooled worker.
type pendingInvocation struct {
	RequestID  string
	Source     string
	FunctionID string
	// Headers of the stub's Runtime API `/next` response (lambda-runtime-*).
	Header http.Header
	// The event, as forwarded.
	Body      []byte
	QueuedAt  time.Time
	StartedAt time.Time
}

type pooledSource struct {
	FunctionID string
	Env        []string
}

type pooledWorker struct {
	ID       string
	Key      poolKey
	Worker   runtime.Worker
	inFlight int
	max      int
	stale    bool
	created  time.Time
	idle     *time.Timer
	requests map[string]*pendingInvocation
	// Request ID of the most recent dispatch, for untagged log lines.
	last string
	next chan *pendingInvocation
	// Latest memory report, when SST_DEBUG_MEMORY is on.
	memory string
}

type keyState struct {
	runtime   string
	bundle    string
	functions map[string]bool
	workers   []*pooledWorker
	waiting   []*pendingInvocation
	// Request IDs in flight or waiting, so a re-sent Next is not run twice.
	seen map[string]bool
	// Bundle stamp the live workers were started from.
	stamp time.Time
	// Whether the bundle is ready to run (its entry file exists).
	ready bool
	// When the stamp last changed; warmup waits until it has been quiet.
	changed time.Time
}

// StartWorker starts a process for `id` that runs the bundle behind `key`.
// `functionID` is one of the functions the pool serves, whose build output
// and environment seed the worker.
type StartWorker func(key poolKey, functionID string, id string) (runtime.Worker, error)

// StopWorker stops a worker the pool no longer wants.
type StopWorker func(w *pooledWorker)

type Pool struct {
	mu        sync.Mutex
	cfg       PoolConfig
	keys      map[poolKey]*keyState
	functions map[string]poolKey
	workers   map[string]*pooledWorker
	sources   map[string]*pooledSource
	orphans   map[string][]*pendingInvocation
	clock     func() time.Time
	start     StartWorker
	stop      StopWorker
	seq       int
	// How long the bundle stamp must be quiet before warmup starts.
	quiet time.Duration
}

func NewPool(cfg PoolConfig, start StartWorker, stop StopWorker) *Pool {
	return &Pool{
		cfg:       cfg,
		keys:      map[poolKey]*keyState{},
		functions: map[string]poolKey{},
		workers:   map[string]*pooledWorker{},
		sources:   map[string]*pooledSource{},
		orphans:   map[string][]*pendingInvocation{},
		clock:     time.Now,
		start:     start,
		stop:      stop,
		quiet:     1500 * time.Millisecond,
	}
}

func keyFor(runtime string, bundle string) poolKey {
	return poolKey(runtime + "\x00" + bundle)
}

// Target records that functionID runs from `bundle`. When the function moves
// to a different bundle, workers of its old key are retired as they drain.
// The returned key is new when no key existed for the bundle before, so the
// caller can start watching the bundle.
func (p *Pool) Target(functionID string, rt string, bundle string) (poolKey, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := keyFor(rt, bundle)
	if old, ok := p.functions[functionID]; ok && old != key {
		if ks := p.keys[old]; ks != nil {
			delete(ks.functions, functionID)
			p.invalidate(old, "retarget")
		}
	}
	p.functions[functionID] = key
	ks, existed := p.keys[key]
	if !existed {
		ks = &keyState{
			runtime:   rt,
			bundle:    bundle,
			functions: map[string]bool{},
			seen:      map[string]bool{},
		}
		p.keys[key] = ks
	}
	ks.functions[functionID] = true
	return key, !existed
}

func (p *Pool) IsPooled(functionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.functions[functionID]
	return ok
}

// Bundle is the bundle path behind a key.
func (p *Pool) Bundle(key poolKey) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ks, ok := p.keys[key]
	if !ok {
		return "", false
	}
	return ks.bundle, true
}

// Register binds a stub instance (source) to its function and environment.
// Invocations that arrived before the Init are returned so the caller can
// dispatch them.
func (p *Pool) Register(source string, functionID string, env []string) []*pendingInvocation {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources[source] = &pooledSource{FunctionID: functionID, Env: env}
	orphans := p.orphans[source]
	delete(p.orphans, source)
	for _, inv := range orphans {
		inv.FunctionID = functionID
	}
	return orphans
}

func (p *Pool) Source(source string) (*pooledSource, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, ok := p.sources[source]
	return info, ok
}

// Orphan parks an invocation from a source that has not sent Init yet.
func (p *Pool) Orphan(source string, inv *pendingInvocation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.orphans[source] = append(p.orphans[source], inv)
}

// Dispatch hands an invocation to a worker: the busiest live worker with
// spare capacity, else a new worker while the key is under its cap, else the
// queue. A request ID already in flight or waiting is ignored, because a
// rebooted stub re-sends its pending request.
func (p *Pool) Dispatch(inv *pendingInvocation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, ok := p.sources[inv.Source]
	if !ok {
		return fmt.Errorf("unknown pooled source %q", inv.Source)
	}
	inv.FunctionID = info.FunctionID
	key, ok := p.functions[inv.FunctionID]
	if !ok {
		return fmt.Errorf("function %q is not pooled", inv.FunctionID)
	}
	ks := p.keys[key]
	if ks.seen[inv.RequestID] {
		return nil
	}
	ks.seen[inv.RequestID] = true
	if inv.QueuedAt.IsZero() {
		inv.QueuedAt = p.clock()
	}
	ks.waiting = append(ks.waiting, inv)
	p.drain(key)
	return nil
}

// Next is the channel a pooled worker reads its invocations from.
func (p *Pool) Next(workerID string) (chan *pendingInvocation, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.workers[workerID]
	if !ok {
		return nil, false
	}
	return w.next, true
}

// Complete records that a worker finished a request. It returns the request
// so the caller can answer its source, then frees the worker's slot: a stale
// worker that drained is stopped, an idle worker starts its idle timer, and
// waiting invocations are dispatched.
func (p *Pool) Complete(workerID string, requestID string) (*pendingInvocation, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.workers[workerID]
	if !ok {
		return nil, false
	}
	inv, ok := w.requests[requestID]
	if !ok {
		return nil, false
	}
	delete(w.requests, requestID)
	if ks := p.keys[w.Key]; ks != nil {
		delete(ks.seen, requestID)
	}
	if w.inFlight > 0 {
		w.inFlight--
	}
	p.release(w)
	return inv, true
}

// Held reports the requests a worker holds, for a caller that must fail them.
func (p *Pool) Held(workerID string) []*pendingInvocation {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.workers[workerID]
	if !ok {
		return nil
	}
	held := make([]*pendingInvocation, 0, len(w.requests))
	for _, inv := range w.requests {
		held = append(held, inv)
	}
	return held
}

// WorkerDied forgets a worker that exited on its own and returns the requests
// it was holding so the caller can fail them. A replacement starts only when
// something is waiting.
func (p *Pool) WorkerDied(workerID string) []*pendingInvocation {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.workers[workerID]
	if !ok {
		return nil
	}
	held := make([]*pendingInvocation, 0, len(w.requests))
	ks := p.keys[w.Key]
	for id, inv := range w.requests {
		held = append(held, inv)
		if ks != nil {
			delete(ks.seen, id)
		}
	}
	p.remove(w)
	p.drain(w.Key)
	return held
}

// Invalidate retires every worker of a key: idle ones now, busy ones once
// they drain. The next dispatch starts fresh workers from the new bundle.
func (p *Pool) Invalidate(key poolKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invalidate(key, "invalidate")
}

// Tick feeds the bundle's stamp and readiness, as observed by the watcher:
// a newer stamp invalidates the key, and a ready bundle that has been quiet
// for a while gets its warm workers.
func (p *Pool) Tick(key poolKey, stamp time.Time, ready bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ks, ok := p.keys[key]
	if !ok {
		return
	}
	now := p.clock()
	if ks.stamp.IsZero() {
		ks.stamp = stamp
		ks.changed = now
	} else if stamp.After(ks.stamp) {
		ks.stamp = stamp
		ks.changed = now
		p.invalidate(key, "stale")
	}
	ks.ready = ready
	if ready && now.Sub(ks.changed) >= p.quiet {
		p.warm(key)
		p.drain(key)
	}
}

// Stats snapshots every key for the PoolStatsEvent publisher.
func (p *Pool) Stats() []PoolStatsEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]PoolStatsEvent, 0, len(p.keys))
	for key, ks := range p.keys {
		stat := PoolStatsEvent{Key: string(key), Bundle: ks.bundle, Queued: len(ks.waiting)}
		for _, w := range ks.workers {
			stat.Workers++
			stat.InFlight += w.inFlight
		}
		result = append(result, stat)
	}
	return result
}

// Attribute resolves a worker's log line to the invocation it belongs to.
// Tagged lines name their request; untagged lines go to the most recently
// dispatched request. Memory reports are recorded and not returned as logs.
func (p *Pool) Attribute(workerID string, line string) (inv *pendingInvocation, text string, memory bool) {
	requestID, text, tagged := splitTagged(line)
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.workers[workerID]
	if !ok {
		return nil, text, false
	}
	if tagged && requestID == memoryTag {
		w.memory = text
		return nil, text, true
	}
	if !tagged {
		requestID = w.last
	}
	return w.requests[requestID], text, false
}

// Memory is the latest memory report of a worker, when any.
func (p *Pool) Memory(workerID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w, ok := p.workers[workerID]; ok {
		return w.memory
	}
	return ""
}

// ---- internals (caller holds p.mu) ----

func (p *Pool) liveCount(ks *keyState) int {
	count := 0
	for _, w := range ks.workers {
		if !w.stale {
			count++
		}
	}
	return count
}

// pick is the busiest live worker that still has spare capacity. Packing
// invocations into as few workers as possible keeps memory down: every worker
// that serves traffic grows to hold the handlers it has loaded.
func (p *Pool) pick(ks *keyState) *pooledWorker {
	var best *pooledWorker
	for _, w := range ks.workers {
		if w.stale || w.inFlight >= w.max {
			continue
		}
		if best == nil || w.inFlight > best.inFlight {
			best = w
		}
	}
	return best
}

func (p *Pool) drain(key poolKey) {
	ks, ok := p.keys[key]
	if !ok {
		return
	}
	for len(ks.waiting) > 0 {
		w := p.pick(ks)
		if w == nil && p.liveCount(ks) < p.cfg.Size {
			w = p.startWorker(key, ks)
		}
		if w == nil {
			return
		}
		inv := ks.waiting[0]
		ks.waiting = ks.waiting[1:]
		p.checkout(w, inv)
	}
}

func (p *Pool) checkout(w *pooledWorker, inv *pendingInvocation) {
	if w.idle != nil {
		w.idle.Stop()
		w.idle = nil
	}
	inv.StartedAt = p.clock()
	w.inFlight++
	w.requests[inv.RequestID] = inv
	w.last = inv.RequestID
	// Never blocks: inFlight <= max and the channel holds max entries.
	w.next <- inv
}

func (p *Pool) release(w *pooledWorker) {
	if w.inFlight > 0 {
		p.drain(w.Key)
		return
	}
	if w.stale {
		p.remove(w)
		p.stop(w)
		p.drain(w.Key)
		return
	}
	if w.idle != nil {
		w.idle.Stop()
	}
	id := w.ID
	w.idle = time.AfterFunc(p.cfg.IdleTimeout, func() { p.retireIdle(id) })
	p.drain(w.Key)
}

func (p *Pool) retireIdle(workerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.workers[workerID]
	if !ok || w.inFlight > 0 {
		return
	}
	p.remove(w)
	p.stop(w)
}

func (p *Pool) invalidate(key poolKey, reason string) {
	ks, ok := p.keys[key]
	if !ok {
		return
	}
	for _, w := range append([]*pooledWorker{}, ks.workers...) {
		if w.inFlight == 0 {
			p.remove(w)
			p.stop(w)
			continue
		}
		w.stale = true
	}
	_ = reason
}

func (p *Pool) warm(key poolKey) {
	ks := p.keys[key]
	for p.liveCount(ks) < p.cfg.Warmup {
		if p.startWorker(key, ks) == nil {
			return
		}
	}
}

func (p *Pool) startWorker(key poolKey, ks *keyState) *pooledWorker {
	if !ks.ready {
		return nil
	}
	var functionID string
	for id := range ks.functions {
		functionID = id
		break
	}
	if functionID == "" {
		return nil
	}
	p.seq++
	w := &pooledWorker{
		ID:       fmt.Sprintf("pool-%d", p.seq),
		Key:      key,
		max:      p.cfg.Concurrency,
		created:  p.clock(),
		requests: map[string]*pendingInvocation{},
		next:     make(chan *pendingInvocation, p.cfg.Concurrency),
	}
	// The worker must be visible before start returns, because start wires
	// up its log scanner, which looks the worker up by id.
	p.workers[w.ID] = w
	ks.workers = append(ks.workers, w)
	worker, err := p.start(key, functionID, w.ID)
	if err != nil || worker == nil {
		p.remove(w)
		return nil
	}
	w.Worker = worker
	return w
}

func (p *Pool) remove(w *pooledWorker) {
	if w.idle != nil {
		w.idle.Stop()
		w.idle = nil
	}
	delete(p.workers, w.ID)
	ks, ok := p.keys[w.Key]
	if !ok {
		return
	}
	for i, existing := range ks.workers {
		if existing == w {
			ks.workers = append(ks.workers[:i], ks.workers[i+1:]...)
			break
		}
	}
}

// ---- log line tagging ----

// Pooled workers prefix every console line with the request it belongs to,
// using the ASCII record separator, which never appears in normal logs.
const tagMark = "\x1e"

// A tagged line whose request id is memoryTag carries a worker memory report.
const memoryTag = "sst.memory"

func splitTagged(line string) (requestID string, text string, tagged bool) {
	if !strings.HasPrefix(line, tagMark) {
		return "", line, false
	}
	end := strings.Index(line[1:], tagMark)
	if end < 0 {
		return "", line, false
	}
	return line[1 : end+1], line[end+2:], true
}
