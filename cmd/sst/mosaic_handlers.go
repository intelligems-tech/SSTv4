package main

import (
	"context"
	"sort"
	"sync"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/cmd/sst/mosaic/multiplexer"
	"github.com/sst/sst/v3/pkg/bus"
)

// handlerCatalog remembers the handlers pooled bundles reported, so the
// Functions filter can narrow logs to one handler of a shared pool.
type handlerCatalog struct {
	mu       sync.Mutex
	handlers map[string]string
}

func newHandlerCatalog() *handlerCatalog {
	return &handlerCatalog{handlers: map[string]string{}}
}

// startHandlerCatalog records the handler of every invocation until ctx ends.
func startHandlerCatalog(ctx context.Context) *handlerCatalog {
	catalog := newHandlerCatalog()
	invoked := bus.Subscribe(&aws.FunctionInvokedEvent{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt := <-invoked:
				if evt, ok := evt.(*aws.FunctionInvokedEvent); ok {
					catalog.Observe(evt)
				}
			}
		}
	}()
	return catalog
}

func (h *handlerCatalog) Observe(evt *aws.FunctionInvokedEvent) {
	if evt.Handler == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[evt.Handler] = evt.Label
}

// Options lists the handlers seen so far, sorted by key.
func (h *handlerCatalog) Options() []multiplexer.FilterOption {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.handlers))
	for key := range h.handlers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	options := make([]multiplexer.FilterOption, 0, len(keys))
	for _, key := range keys {
		options = append(options, multiplexer.FilterOption{
			Label:       key,
			Description: h.handlers[key],
			Value:       key,
		})
	}
	return options
}
