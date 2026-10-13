package aws

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stuck SQS queues, detected from the invocations `sst dev` already sees.
//
// When a queue-triggered handler fails, SQS redelivers the message after each
// visibility timeout; on a FIFO queue that blocks the whole message group. The
// redeliveries reach the local function, and each input carries the SQS
// event, so a climbing ApproximateReceiveCount (or repeated local failures of
// one message) marks a stall without polling AWS.
//
// Detection is per message, not per batch: the poison message is rarely the
// first record, and a handler that reports batchItemFailures fails a single
// message while the invocation itself succeeds.

const (
	// A message redelivered this many times is stuck, whatever it does now.
	stallReceiveCount = 3
	// ...or one that failed locally this many times (fresh-count redrives).
	stallFailedAttempts = 3
	// Forget a message this long after its last delivery: redeliveries arrive
	// every visibility timeout, so a quiet message has left the queue.
	stallTTL = 5 * time.Minute
)

// QueueStall is a queue whose head message keeps failing and coming back.
type QueueStall struct {
	QueueName      string
	QueueArn       string
	Region         string
	IsFifo         bool
	FunctionID     string
	MessageID      string
	MessageGroupID string
	ReceiveCount   int
	FailedAttempts int
	LastError      string
	FirstSeen      time.Time
	LastSeen       time.Time
}

// QueueStallsEvent carries the current stalls, one per queue, whenever the
// set changes. An empty list means every queue is moving again.
type QueueStallsEvent struct {
	Stalls []QueueStall
}

// QueuePurgedEvent reports a purge requested from the dev UI.
type QueuePurgedEvent struct {
	QueueName string
	Error     string `json:",omitempty"`
}

type sqsRecord struct {
	QueueArn       string
	QueueName      string
	Region         string
	IsFifo         bool
	MessageID      string
	MessageGroupID string
	ReceiveCount   int
}

// extractSqsRecords parses every SQS record of a Lambda input. It is empty
// for any input that is not an SQS event.
func extractSqsRecords(input []byte) []sqsRecord {
	var event struct {
		Records []struct {
			EventSource    string `json:"eventSource"`
			EventSourceARN string `json:"eventSourceARN"`
			MessageID      string `json:"messageId"`
			AwsRegion      string `json:"awsRegion"`
			Attributes     struct {
				ApproximateReceiveCount string `json:"ApproximateReceiveCount"`
				MessageGroupID          string `json:"MessageGroupId"`
			} `json:"attributes"`
		} `json:"Records"`
	}
	if json.Unmarshal(input, &event) != nil {
		return nil
	}
	var records []sqsRecord
	for _, r := range event.Records {
		if r.EventSource != "aws:sqs" || r.EventSourceARN == "" || r.MessageID == "" {
			continue
		}
		// arn:aws:sqs:<region>:<account>:<queueName>
		parts := strings.Split(r.EventSourceARN, ":")
		name := parts[len(parts)-1]
		if name == "" {
			continue
		}
		region := r.AwsRegion
		if region == "" && len(parts) >= 4 {
			region = parts[3]
		}
		count, err := strconv.Atoi(r.Attributes.ApproximateReceiveCount)
		if err != nil || count < 1 {
			count = 1
		}
		records = append(records, sqsRecord{
			QueueArn:       r.EventSourceARN,
			QueueName:      name,
			Region:         region,
			IsFifo:         strings.HasSuffix(name, ".fifo"),
			MessageID:      r.MessageID,
			MessageGroupID: r.Attributes.MessageGroupID,
			ReceiveCount:   count,
		})
	}
	return records
}

// batchItemFailureIDs returns the message ids a partial-batch response
// reported as failed, or nil when the output is not one.
func batchItemFailureIDs(output []byte) map[string]bool {
	var response struct {
		BatchItemFailures *[]struct {
			ItemIdentifier string `json:"itemIdentifier"`
		} `json:"batchItemFailures"`
	}
	if json.Unmarshal(output, &response) != nil || response.BatchItemFailures == nil {
		return nil
	}
	ids := map[string]bool{}
	for _, failure := range *response.BatchItemFailures {
		if failure.ItemIdentifier != "" {
			ids[failure.ItemIdentifier] = true
		}
	}
	return ids
}

type invocationStatus int

const (
	invocationRunning invocationStatus = iota
	invocationSucceeded
	invocationFailed
)

// stallObservation is one state of an invocation: when it starts, and again
// when it ends.
type stallObservation struct {
	RequestID  string
	FunctionID string
	Input      []byte
	Status     invocationStatus
	Output     []byte
	Error      string
	At         time.Time
}

type messageTrack struct {
	record     sqsRecord
	functionID string
	receive    int
	failed     map[string]bool
	lastError  string
	firstSeen  time.Time
	lastSeen   time.Time
	// Sticky once tripped, so a running redelivery does not clear it.
	stalled bool
}

// QueueStallTracker turns invocations into stalls. It is not safe for
// concurrent use; the watcher owns it.
type QueueStallTracker struct {
	messages  map[string]*messageTrack
	lastEvent time.Time
}

func NewQueueStallTracker() *QueueStallTracker {
	return &QueueStallTracker{messages: map[string]*messageTrack{}}
}

func trackKey(queueName string, messageID string) string {
	return queueName + "|" + messageID
}

// Observe feeds one invocation state through the tracker.
func (t *QueueStallTracker) Observe(o stallObservation) {
	records := extractSqsRecords(o.Input)
	if len(records) == 0 {
		return
	}
	// A partial-batch response splits the verdict per message; any other
	// outcome applies to every record (SQS redelivers the whole batch).
	var failedIDs map[string]bool
	if o.Status == invocationSucceeded {
		failedIDs = batchItemFailureIDs(o.Output)
	}
	if o.At.After(t.lastEvent) {
		t.lastEvent = o.At
	}
	seen := map[string]bool{}
	groups := map[string]bool{}
	for _, record := range records {
		key := trackKey(record.QueueName, record.MessageID)
		seen[key] = true
		if record.MessageGroupID != "" {
			groups[record.QueueName+"|"+record.MessageGroupID] = true
		}
		track, ok := t.messages[key]
		if !ok {
			track = &messageTrack{
				record:     record,
				functionID: o.FunctionID,
				failed:     map[string]bool{},
				firstSeen:  o.At,
				lastSeen:   o.At,
			}
			t.messages[key] = track
		}
		if record.ReceiveCount > track.receive {
			track.receive = record.ReceiveCount
		}
		if o.At.After(track.lastSeen) {
			track.lastSeen = o.At
		}
		failed := o.Status == invocationFailed || failedIDs[record.MessageID]
		if o.Status == invocationSucceeded && !failed {
			// Consumed: nothing is blocked behind it.
			delete(t.messages, key)
			continue
		}
		if failed {
			track.failed[o.RequestID] = true
			track.lastError = o.Error
			if track.lastError == "" {
				// A partial-batch failure carries no thrown error.
				track.lastError = "handler returned it in batchItemFailures"
			}
		}
		// A redelivery count this high is a stall whatever the local run does:
		// a handler that hangs never reports an error.
		if track.receive >= stallReceiveCount || len(track.failed) >= stallFailedAttempts {
			track.stalled = true
		}
	}
	t.dropDisplacedFifoMessages(groups, seen, o.At)
}

// dropDisplacedFifoMessages forgets messages of a FIFO group that a newer
// batch of the same group no longer carries: a group is delivered in order,
// so they left the queue.
func (t *QueueStallTracker) dropDisplacedFifoMessages(groups map[string]bool, seen map[string]bool, batchAt time.Time) {
	if len(groups) == 0 {
		return
	}
	for key, track := range t.messages {
		if track.record.MessageGroupID == "" || seen[key] {
			continue
		}
		if !groups[track.record.QueueName+"|"+track.record.MessageGroupID] {
			continue
		}
		if !track.lastSeen.Before(batchAt) {
			continue
		}
		delete(t.messages, key)
	}
}

// ClearQueue forgets a queue, after a purge.
func (t *QueueStallTracker) ClearQueue(queueName string) {
	for key, track := range t.messages {
		if track.record.QueueName == queueName {
			delete(t.messages, key)
		}
	}
}

// worseStall reports whether a track should represent its queue instead of
// the stall chosen so far: the most recent wins, then the one that failed
// most, then the one stuck longest.
func worseStall(track *messageTrack, existing QueueStall) bool {
	if !track.lastSeen.Equal(existing.LastSeen) {
		return track.lastSeen.After(existing.LastSeen)
	}
	if len(track.failed) != existing.FailedAttempts {
		return len(track.failed) > existing.FailedAttempts
	}
	return track.firstSeen.Before(existing.FirstSeen)
}

// Stalls lists the current stalls, one per queue (see worseStall), most recent first. Messages quiet for longer than the TTL are
// forgotten; the clock is the latest invocation seen.
func (t *QueueStallTracker) Stalls() []QueueStall {
	byQueue := map[string]QueueStall{}
	for key, track := range t.messages {
		if t.lastEvent.Sub(track.lastSeen) > stallTTL {
			delete(t.messages, key)
			continue
		}
		if !track.stalled {
			continue
		}
		if existing, ok := byQueue[track.record.QueueName]; ok && !worseStall(track, existing) {
			continue
		}
		byQueue[track.record.QueueName] = QueueStall{
			QueueName:      track.record.QueueName,
			QueueArn:       track.record.QueueArn,
			Region:         track.record.Region,
			IsFifo:         track.record.IsFifo,
			FunctionID:     track.functionID,
			MessageID:      track.record.MessageID,
			MessageGroupID: track.record.MessageGroupID,
			ReceiveCount:   track.receive,
			FailedAttempts: len(track.failed),
			LastError:      track.lastError,
			FirstSeen:      track.firstSeen,
			LastSeen:       track.lastSeen,
		}
	}
	stalls := make([]QueueStall, 0, len(byQueue))
	for _, stall := range byQueue {
		stalls = append(stalls, stall)
	}
	sort.Slice(stalls, func(i, j int) bool { return stalls[i].LastSeen.After(stalls[j].LastSeen) })
	return stalls
}
