package main

import (
	"context"
	"sort"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/cmd/sst/mosaic/multiplexer"
	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/flag"
)

// devStatus folds pooled worker stats and stuck queues into the sidebar
// status block, on top of the stage and the launcher's SST_DEV_STATUS.
type devStatus struct {
	base   multiplexer.Status
	pools  map[string]aws.PoolStatsEvent
	stalls []aws.QueueStall
}

func newDevStatus(stage string, tone string, segments string) *devStatus {
	return &devStatus{
		base: multiplexer.Status{
			Stage:    stage,
			Tone:     tone,
			Segments: multiplexer.ParseStatusSegments(segments),
		},
		pools: map[string]aws.PoolStatsEvent{},
	}
}

// apply folds one event in and reports whether the block changed.
func (d *devStatus) apply(evt interface{}) bool {
	switch evt := evt.(type) {
	case *aws.PoolStatsEvent:
		previous, ok := d.pools[evt.Key]
		d.pools[evt.Key] = *evt
		return !ok || previous != *evt
	case *aws.QueueStallsEvent:
		d.stalls = evt.Stalls
		return true
	}
	return false
}

func (d *devStatus) status() multiplexer.Status {
	status := d.base
	if len(d.pools) > 0 {
		pool := &multiplexer.PoolStatus{}
		for _, stat := range d.pools {
			pool.Workers += stat.Workers
			pool.InFlight += stat.InFlight
			pool.Queued += stat.Queued
		}
		status.Pool = pool
	}
	alerts := make([]string, 0, len(d.stalls))
	for _, stall := range d.stalls {
		alerts = append(alerts, "stuck "+stall.QueueName)
	}
	sort.Strings(alerts)
	status.Alerts = alerts
	return status
}

// startDevStatus keeps the multiplexer's status block current until ctx ends.
func startDevStatus(ctx context.Context, multi *multiplexer.Multiplexer, stage string) {
	state := newDevStatus(stage, flag.SST_DEV_STAGE_TONE, flag.SST_DEV_STATUS)
	evts := bus.Subscribe(&aws.PoolStatsEvent{}, &aws.QueueStallsEvent{})
	multi.SetStatus(state.status())
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt := <-evts:
				if state.apply(evt) {
					multi.SetStatus(state.status())
				}
			}
		}
	}()
}
