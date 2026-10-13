// Package queues is the Queues pane of `sst dev`: every SQS queue the app
// deployed, its depth in AWS, the messages the local consumers received and
// how they ended, stuck queues and their dead-letter queues.
package queues

import (
	"sort"
	"strings"
	"time"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
	"github.com/sst/sst/v3/pkg/project"
)

// perQueue bounds how many received messages a queue keeps.
const perQueue = 100

// activityWindow coarsens the activity sort so rows do not reshuffle on
// every message.
const activityWindow = 30 * time.Second

type Status int

const (
	Running Status = iota
	Done
	Failed
)

// Message is one SQS message a local consumer received.
type Message struct {
	Queue     string
	ID        string
	GroupID   string
	RequestID string
	Consumer  string
	Body      string
	Receive   int
	Status    Status
	Error     string
	At        time.Time
}

type Queue struct {
	Name       string
	Arn        string
	URL        string
	Fifo       bool
	Deployed   bool
	DLQ        string
	MaxReceive int
	// Sources names the queues that dead-letter into this one.
	Sources   []string
	Depth     *aws.QueueDepth
	Stall     *aws.QueueStall
	Received  int
	Failed    int
	Running   int
	Last      time.Time
	Consumers []string
	Messages  []*Message
}

func (q *Queue) IsDLQ() bool {
	return len(q.Sources) > 0
}

type Store struct {
	now       func() time.Time
	queues    map[string]*Queue
	batches   map[string][]*Message
	DepthAt   time.Time
	LastPurge *aws.QueuePurgedEvent
}

func NewStore(now func() time.Time) *Store {
	return &Store{now: now, queues: map[string]*Queue{}, batches: map[string][]*Message{}}
}

func (s *Store) queue(name string) *Queue {
	q, ok := s.queues[name]
	if !ok {
		q = &Queue{Name: name, Fifo: strings.HasSuffix(name, ".fifo")}
		s.queues[name] = q
	}
	return q
}

// Queue returns a queue by name.
func (s *Store) Queue(name string) *Queue {
	return s.queues[name]
}

// Apply folds one dev event in and reports whether the pane changed.
func (s *Store) Apply(evt interface{}) bool {
	switch evt := evt.(type) {
	case *project.CompleteEvent:
		if evt.Old || !evt.Finished {
			return false
		}
		s.deployed(aws.QueuesFromComplete(evt))
	case *aws.FunctionInvokedEvent:
		return s.invoked(evt)
	case *aws.FunctionResponseEvent:
		messages, ok := s.batches[evt.RequestID]
		if !ok {
			return false
		}
		delete(s.batches, evt.RequestID)
		failed := aws.BatchItemFailureIDs(evt.Output)
		for _, message := range messages {
			if failed[message.ID] {
				s.finish(message, Failed, "returned in batchItemFailures")
			} else {
				s.finish(message, Done, "")
			}
		}
	case *aws.FunctionErrorEvent:
		messages, ok := s.batches[evt.RequestID]
		if !ok {
			return false
		}
		delete(s.batches, evt.RequestID)
		reason := evt.ErrorMessage
		if reason == "" {
			reason = evt.ErrorType
		}
		for _, message := range messages {
			s.finish(message, Failed, reason)
		}
	case *aws.QueueStallsEvent:
		for _, q := range s.queues {
			q.Stall = nil
		}
		for i := range evt.Stalls {
			stall := evt.Stalls[i]
			s.queue(stall.QueueName).Stall = &stall
		}
	case *aws.QueueDepthsEvent:
		for i := range evt.Depths {
			depth := evt.Depths[i]
			if q, ok := s.queues[depth.QueueName]; ok {
				q.Depth = &depth
			}
		}
		s.DepthAt = evt.At
	case *aws.QueuePurgedEvent:
		s.LastPurge = evt
	default:
		return false
	}
	return true
}

func (s *Store) deployed(resources []aws.QueueResource) {
	for _, q := range s.queues {
		q.Sources = nil
	}
	for _, resource := range resources {
		q := s.queue(resource.Name)
		q.Arn, q.URL, q.Fifo, q.Deployed = resource.Arn, resource.URL, resource.Fifo, true
		q.MaxReceive = resource.MaxReceive
		q.DLQ = ""
		if resource.DLQArn != "" {
			q.DLQ = aws.QueueNameFromArn(resource.DLQArn)
		}
	}
	for _, q := range s.queues {
		if q.DLQ != "" {
			dlq := s.queue(q.DLQ)
			dlq.Sources = append(dlq.Sources, q.Name)
		}
	}
}

func (s *Store) invoked(evt *aws.FunctionInvokedEvent) bool {
	records := aws.ExtractSqsRecords(evt.Input)
	if len(records) == 0 {
		return false
	}
	consumer := evt.Label
	if consumer == "" {
		consumer = evt.Handler
	}
	if consumer == "" {
		consumer = evt.FunctionID
	}
	now := s.now()
	messages := make([]*Message, 0, len(records))
	for _, record := range records {
		q := s.queue(record.QueueName)
		if q.Arn == "" {
			q.Arn = record.QueueArn
		}
		message := &Message{
			Queue:     record.QueueName,
			ID:        record.MessageID,
			GroupID:   record.MessageGroupID,
			RequestID: evt.RequestID,
			Consumer:  consumer,
			Body:      record.Body,
			Receive:   record.ReceiveCount,
			Status:    Running,
			At:        now,
		}
		q.Messages = append(q.Messages, message)
		if len(q.Messages) > perQueue {
			q.Messages = q.Messages[1:]
		}
		q.Received++
		q.Running++
		q.Last = now
		if !contains(q.Consumers, consumer) {
			q.Consumers = append(q.Consumers, consumer)
		}
		messages = append(messages, message)
	}
	s.batches[evt.RequestID] = messages
	return true
}

func (s *Store) finish(message *Message, status Status, reason string) {
	message.Status = status
	message.Error = reason
	q := s.queues[message.Queue]
	q.Running = max(0, q.Running-1)
	q.Last = s.now()
	if status == Failed {
		q.Failed++
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// List lists queues whose name contains filter: stuck queues first, then
// recently active ones in coarse windows, then the rest by name.
func (s *Store) List(filter string) []*Queue {
	filter = strings.ToLower(filter)
	result := []*Queue{}
	for _, q := range s.queues {
		if filter == "" || strings.Contains(strings.ToLower(q.Name), filter) {
			result = append(result, q)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if (a.Stall != nil) != (b.Stall != nil) {
			return a.Stall != nil
		}
		at, bt := a.Last.Truncate(activityWindow), b.Last.Truncate(activityWindow)
		if !at.Equal(bt) {
			return at.After(bt)
		}
		return a.Name < b.Name
	})
	return result
}

// Waiting is the queue's visible message count in AWS, or -1 when unknown.
func (q *Queue) Waiting() int {
	if q.Depth == nil || q.Depth.Error != "" {
		return -1
	}
	return q.Depth.Visible + q.Depth.Delayed
}

// Recent lists the queue's messages newest first.
func (q *Queue) Recent() []*Message {
	result := make([]*Message, 0, len(q.Messages))
	for i := len(q.Messages) - 1; i >= 0; i-- {
		result = append(result, q.Messages[i])
	}
	return result
}
