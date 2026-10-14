package main

import (
	"testing"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
)

func TestDevStatusSumsPoolsAndListsStuckQueues(t *testing.T) {
	d := newDevStatus("christian-local", "shared", "resources=dev,frontend=local")
	if !d.apply(&aws.PoolStatsEvent{Key: "a", Workers: 2, InFlight: 1}) {
		t.Fatal("a new pool changes the block")
	}
	d.apply(&aws.PoolStatsEvent{Key: "b", Workers: 1, Queued: 3})
	if d.apply(&aws.PoolStatsEvent{Key: "b", Workers: 1, Queued: 3}) {
		t.Fatal("identical stats must not redraw")
	}
	d.apply(&aws.QueueStallsEvent{Stalls: []aws.QueueStall{{QueueName: "orders.fifo"}}})

	status := d.status()
	if status.Stage != "christian-local" || status.Tone != "shared" || len(status.Segments) != 2 || status.Segments[0].Key != "resources" || status.Segments[0].Value != "dev" {
		t.Fatalf("unexpected base %+v", status)
	}
	if status.Pool == nil || status.Pool.Workers != 3 || status.Pool.InFlight != 1 || status.Pool.Queued != 3 {
		t.Fatalf("pools must be summed, got %+v", status.Pool)
	}
	if len(status.Alerts) != 1 || status.Alerts[0] != "stuck orders.fifo" {
		t.Fatalf("got alerts %v", status.Alerts)
	}

	d.apply(&aws.QueueStallsEvent{})
	if len(d.status().Alerts) != 0 {
		t.Fatal("recovered queues must drop their alert")
	}
}
