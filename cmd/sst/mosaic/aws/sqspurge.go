package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go"
	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/flag"
	"github.com/sst/sst/v3/pkg/server"
)

// SQS allows one PurgeQueue per queue every 60 seconds.
const purgeCooldown = 60 * time.Second

type purgeFunc func(ctx context.Context, queueName string, region string) error

// queuePurger purges stuck queues for the dev UI and enforces the AWS
// cooldown locally, so a second press says when to retry instead of failing.
type queuePurger struct {
	mu    sync.Mutex
	purge purgeFunc
	now   func() time.Time
	last  map[string]time.Time
}

func newQueuePurger(purge purgeFunc) *queuePurger {
	return &queuePurger{purge: purge, now: time.Now, last: map[string]time.Time{}}
}

// Purge purges a queue. A successful purge, or AWS reporting one already in
// progress, starts the cooldown; any other failure does not, so a retry
// reaches AWS again.
func (p *queuePurger) Purge(ctx context.Context, queueName string, region string) error {
	p.mu.Lock()
	if last, ok := p.last[queueName]; ok {
		if remaining := purgeCooldown - p.now().Sub(last); remaining > 0 {
			p.mu.Unlock()
			return fmt.Errorf("SQS allows one purge per 60s, retry in %ds", int((remaining+time.Second-1)/time.Second))
		}
	}
	p.mu.Unlock()

	err := p.purge(ctx, queueName, region)
	if err == nil || isPurgeInProgress(err) {
		p.mu.Lock()
		p.last[queueName] = p.now()
		p.mu.Unlock()
	}
	return err
}

func isPurgeInProgress(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return strings.Contains(apiErr.ErrorCode(), "PurgeQueueInProgress")
	}
	return strings.Contains(err.Error(), "PurgeQueueInProgress")
}

// sqsPurge purges a queue by name with the dev session's credentials.
func sqsPurge(config aws.Config) purgeFunc {
	return func(ctx context.Context, queueName string, region string) error {
		client := sqs.NewFromConfig(config, func(o *sqs.Options) {
			if region != "" {
				o.Region = region
			}
		})
		url, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(queueName)})
		if err != nil {
			return err
		}
		_, err = client.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: url.QueueUrl})
		return err
	}
}

type purgeRequest struct {
	Queue  string `json:"queue"`
	Region string `json:"region"`
}

// handlePurge serves `POST /api/sqs/purge {queue, region}` for the dev UI.
// The outcome is answered and published as a QueuePurgedEvent.
func handlePurge(ctx context.Context, purger *queuePurger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req purgeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Queue == "" {
			http.Error(w, "expected {\"queue\": \"<name>\"}", http.StatusBadRequest)
			return
		}
		evt := purgeQueue(ctx, purger, req.Queue, req.Region)
		w.Header().Set("Content-Type", "application/json")
		if evt.Error != "" {
			w.WriteHeader(http.StatusConflict)
		}
		json.NewEncoder(w).Encode(evt)
	}
}

func purgeQueue(ctx context.Context, purger *queuePurger, queue string, region string) *QueuePurgedEvent {
	evt := &QueuePurgedEvent{QueueName: queue}
	if err := purger.Purge(ctx, queue, region); err != nil {
		evt.Error = err.Error()
	}
	slog.Info("queue purge", "queue", queue, "error", evt.Error)
	bus.Publish(evt)
	return evt
}

// watchQueueStalls feeds every invocation through a QueueStallTracker and
// publishes the stalls whenever they change. A purged queue is forgotten.
// With SST_DEV_AUTO_PURGE a newly stuck queue is purged right away.
func watchQueueStalls(ctx context.Context, purger *queuePurger) {
	evts := bus.Subscribe(&FunctionInvokedEvent{}, &FunctionResponseEvent{}, &FunctionErrorEvent{}, &QueuePurgedEvent{})
	watcher := newStallWatcher(time.Now)
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-evts:
			changed, fresh := watcher.handle(evt)
			if !changed {
				continue
			}
			bus.Publish(&QueueStallsEvent{Stalls: watcher.tracker.Stalls()})
			if flag.SST_DEV_AUTO_PURGE {
				for _, stall := range fresh {
					go purgeQueue(ctx, purger, stall.QueueName, stall.Region)
				}
			}
		}
	}
}

// stallWatcher assembles invocations from bus events for the tracker.
type stallWatcher struct {
	tracker  *QueueStallTracker
	now      func() time.Time
	inputs   map[string]stallObservation
	previous map[string]string
}

func newStallWatcher(now func() time.Time) *stallWatcher {
	return &stallWatcher{
		tracker:  NewQueueStallTracker(),
		now:      now,
		inputs:   map[string]stallObservation{},
		previous: map[string]string{},
	}
}

// handle applies one bus event. It reports whether the stalls changed and
// which queues newly stalled.
func (s *stallWatcher) handle(evt interface{}) (bool, []QueueStall) {
	switch evt := evt.(type) {
	case *FunctionInvokedEvent:
		if len(ExtractSqsRecords(evt.Input)) == 0 {
			return false, nil
		}
		obs := stallObservation{RequestID: evt.RequestID, FunctionID: evt.FunctionID, Input: evt.Input, Status: invocationRunning, At: s.now()}
		s.inputs[evt.RequestID] = obs
		s.tracker.Observe(obs)
	case *FunctionResponseEvent:
		obs, ok := s.inputs[evt.RequestID]
		if !ok {
			return false, nil
		}
		delete(s.inputs, evt.RequestID)
		obs.Status, obs.Output, obs.At = invocationSucceeded, evt.Output, s.now()
		s.tracker.Observe(obs)
	case *FunctionErrorEvent:
		obs, ok := s.inputs[evt.RequestID]
		if !ok {
			return false, nil
		}
		delete(s.inputs, evt.RequestID)
		obs.Status, obs.Error, obs.At = invocationFailed, evt.ErrorMessage, s.now()
		if obs.Error == "" {
			obs.Error = evt.ErrorType
		}
		s.tracker.Observe(obs)
	case *QueuePurgedEvent:
		if evt.Error != "" {
			return false, nil
		}
		s.tracker.ClearQueue(evt.QueueName)
	default:
		return false, nil
	}
	return s.diff()
}

func (s *stallWatcher) diff() (bool, []QueueStall) {
	current := map[string]string{}
	var fresh []QueueStall
	for _, stall := range s.tracker.Stalls() {
		current[stall.QueueName] = fmt.Sprintf("%s|%d|%d|%s", stall.MessageID, stall.ReceiveCount, stall.FailedAttempts, stall.LastError)
		if _, ok := s.previous[stall.QueueName]; !ok {
			fresh = append(fresh, stall)
		}
	}
	changed := len(current) != len(s.previous)
	for queue, signature := range current {
		if s.previous[queue] != signature {
			changed = true
		}
	}
	s.previous = current
	return changed, fresh
}

// startQueues wires stall detection, the purge endpoint and queue depth
// polling into the dev server.
func startQueues(ctx context.Context, config aws.Config, s *server.Server) {
	purger := newQueuePurger(sqsPurge(config))
	s.Mux.HandleFunc("/api/sqs/purge", handlePurge(ctx, purger))
	go watchQueueStalls(ctx, purger)
	go watchQueueDepths(ctx, sqsDepth(config))
}
