package aws

import (
	"strconv"
	"time"

	"github.com/sst/sst/v3/pkg/flag"
)

// PoolConfig sizes the local worker pool that serves functions sharing one
// prebuilt bundle. Every value can be overridden through the environment; the
// defaults keep a laptop running `sst dev` responsive.
type PoolConfig struct {
	// Live workers per pool key.
	Size int
	// Invocations one worker runs at the same time.
	Concurrency int
	// How long an idle worker is kept before it is stopped.
	IdleTimeout time.Duration
	// Workers started as soon as the bundle is ready. 0 disables warmup.
	Warmup int
	// V8 old-space cap per worker, in MB. 0 leaves it unbounded.
	MaxHeapMB int
	// Run workers with --enable-source-maps (costs memory per worker).
	SourceMaps bool
	// Workers report their memory use after loading and after responses.
	DebugMemory bool
}

// DefaultPoolConfig reads the SST_WORKER_* knobs.
func DefaultPoolConfig() PoolConfig {
	return parsePoolConfig(poolEnv{
		size:        flag.SST_WORKER_POOL_SIZE,
		concurrency: flag.SST_WORKER_CONCURRENCY,
		idleMs:      flag.SST_WORKER_IDLE_TIMEOUT,
		warmup:      flag.SST_WARMUP_COUNT,
		maxHeapMB:   flag.SST_WORKER_MAX_HEAP_MB,
		sourceMaps:  flag.SST_SOURCE_MAPS,
		debugMemory: flag.SST_DEBUG_MEMORY,
	})
}

type poolEnv struct {
	size, concurrency, idleMs, warmup, maxHeapMB string
	sourceMaps, debugMemory                      bool
}

func parsePoolConfig(env poolEnv) PoolConfig {
	cfg := PoolConfig{
		Size:        intOr(env.size, 4),
		Concurrency: intOr(env.concurrency, 10),
		IdleTimeout: time.Duration(intOr(env.idleMs, 5*60*1000)) * time.Millisecond,
		Warmup:      intOr(env.warmup, 1),
		MaxHeapMB:   intOr(env.maxHeapMB, 1024),
		SourceMaps:  env.sourceMaps,
		DebugMemory: env.debugMemory,
	}
	if cfg.Size < 1 {
		cfg.Size = 1
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Warmup < 0 {
		cfg.Warmup = 0
	}
	if cfg.Warmup > cfg.Size {
		cfg.Warmup = cfg.Size
	}
	if cfg.MaxHeapMB < 0 {
		cfg.MaxHeapMB = 0
	}
	return cfg
}

func intOr(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
