package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const stallQueue = "christian-local-shipping-provider-publish.fifo"
const stallArn = "arn:aws:sqs:us-east-1:123456789012:" + stallQueue

type stallHarness struct {
	t       *testing.T
	tracker *QueueStallTracker
	clock   time.Time
}

func newStallHarness(t *testing.T) *stallHarness {
	return &stallHarness{t: t, tracker: NewQueueStallTracker(), clock: time.Unix(1000, 0)}
}

func (h *stallHarness) tick() time.Time {
	h.clock = h.clock.Add(time.Second)
	return h.clock
}

func sqsRecordJSON(arn, messageID string, receiveCount int, group string) map[string]interface{} {
	attributes := map[string]interface{}{"ApproximateReceiveCount": fmt.Sprint(receiveCount)}
	if group != "" {
		attributes["MessageGroupId"] = group
	}
	return map[string]interface{}{
		"messageId":      messageID,
		"eventSource":    "aws:sqs",
		"eventSourceARN": arn,
		"awsRegion":      "us-east-1",
		"body":           "{}",
		"attributes":     attributes,
	}
}

func sqsBatch(arn string, messageIDs []string, receiveCount int, group string) []byte {
	records := []interface{}{}
	for _, id := range messageIDs {
		records = append(records, sqsRecordJSON(arn, id, receiveCount, group))
	}
	data, _ := json.Marshal(map[string]interface{}{"Records": records})
	return data
}

func fifoInput(messageID string, receiveCount int) []byte {
	return sqsBatch(stallArn, []string{messageID}, receiveCount, "org-1")
}

func (h *stallHarness) observe(requestID string, input []byte, status invocationStatus, output string) {
	h.tracker.Observe(stallObservation{
		RequestID:  requestID,
		FunctionID: "fnPublish",
		Input:      input,
		Status:     status,
		Output:     []byte(output),
		Error:      map[bool]string{true: "boom"}[status == invocationFailed],
		At:         h.tick(),
	})
}

func (h *stallHarness) fail(requestID string, receiveCount int, messageID string) {
	h.observe(requestID, fifoInput(messageID, receiveCount), invocationFailed, "")
}

func (h *stallHarness) stalls() []QueueStall {
	return h.tracker.Stalls()
}

func TestExtractSqsRecordsParsesEveryRecord(t *testing.T) {
	records := extractSqsRecords(sqsBatch(stallArn, []string{"m1", "m2", "m3"}, 4, "org-1"))
	if len(records) != 3 || records[1].MessageID != "m2" {
		t.Fatalf("every message of a batch must be parsed, got %+v", records)
	}
	first := records[0]
	if first.QueueName != stallQueue || first.QueueArn != stallArn || first.Region != "us-east-1" || !first.IsFifo || first.MessageGroupID != "org-1" || first.ReceiveCount != 4 {
		t.Fatalf("unexpected record %+v", first)
	}
}

func TestExtractSqsRecordsIgnoresOtherInputs(t *testing.T) {
	for _, input := range []string{`{"rawPath":"/x","headers":{}}`, `{"ding":true}`, `null`, `{"Records":[{"eventSource":"aws:s3"}]}`, `not json`} {
		if records := extractSqsRecords([]byte(input)); len(records) != 0 {
			t.Fatalf("%s parsed as %+v", input, records)
		}
	}
}

func TestExtractSqsRecordsDefaultsAMissingReceiveCountToOne(t *testing.T) {
	input := `{"Records":[{"messageId":"m","eventSource":"aws:sqs","eventSourceARN":"` + stallArn + `"}]}`
	if records := extractSqsRecords([]byte(input)); records[0].ReceiveCount != 1 {
		t.Fatalf("got %+v", records)
	}
}

func TestARedeliveredFailingMessageStalls(t *testing.T) {
	h := newStallHarness(t)
	h.fail("r1", 1, "msg-A")
	h.fail("r2", 2, "msg-A")
	if len(h.stalls()) != 0 {
		t.Fatal("two attempts must not stall")
	}
	h.fail("r3", 3, "msg-A")
	stalls := h.stalls()
	if len(stalls) != 1 {
		t.Fatalf("expected one stall, got %+v", stalls)
	}
	s := stalls[0]
	if s.QueueName != stallQueue || !s.IsFifo || s.MessageID != "msg-A" || s.ReceiveCount != 3 || s.FailedAttempts != 3 || s.LastError != "boom" {
		t.Fatalf("unexpected stall %+v", s)
	}
}

func TestThePoisonMessageIsFoundAnywhereInABatch(t *testing.T) {
	h := newStallHarness(t)
	h.observe("r1", sqsBatch(stallArn, []string{"a1", "stuck", "a2"}, 1, "org-1"), invocationFailed, "")
	h.observe("r2", sqsBatch(stallArn, []string{"b1", "b2", "stuck"}, 2, "org-1"), invocationFailed, "")
	if len(h.stalls()) != 0 {
		t.Fatal("two attempts must not stall")
	}
	h.observe("r3", sqsBatch(stallArn, []string{"c1", "stuck", "c2"}, 3, "org-1"), invocationFailed, "")
	stalls := h.stalls()
	if len(stalls) != 1 || stalls[0].MessageID != "stuck" || stalls[0].FailedAttempts != 3 {
		t.Fatalf("got %+v", stalls)
	}
}

func TestAPartialBatchFailureStallsAlthoughTheHandlerReturns(t *testing.T) {
	h := newStallHarness(t)
	partial := `{"batchItemFailures":[{"itemIdentifier":"stuck"}]}`
	h.observe("1", sqsBatch(stallArn, []string{"ok-1", "stuck"}, 1, "org-1"), invocationSucceeded, partial)
	h.observe("2", sqsBatch(stallArn, []string{"ok-1", "stuck"}, 2, "org-1"), invocationSucceeded, partial)
	if len(h.stalls()) != 0 {
		t.Fatal("two attempts must not stall")
	}
	h.observe("3", sqsBatch(stallArn, []string{"ok-1", "stuck"}, 3, "org-1"), invocationSucceeded, partial)
	stalls := h.stalls()
	if len(stalls) != 1 || stalls[0].MessageID != "stuck" || stalls[0].FailedAttempts != 3 || stalls[0].LastError != "handler returned it in batchItemFailures" {
		t.Fatalf("got %+v", stalls)
	}
}

func TestAMessageThePartialBatchHandlerDidNotReportNeverStalls(t *testing.T) {
	h := newStallHarness(t)
	for i := 1; i <= 4; i++ {
		h.observe(fmt.Sprint(i), sqsBatch(stallArn, []string{"ok-1", "stuck"}, i, "org-1"), invocationSucceeded, `{"batchItemFailures":[{"itemIdentifier":"stuck"}]}`)
	}
	stalls := h.stalls()
	if len(stalls) != 1 || stalls[0].MessageID != "stuck" {
		t.Fatalf("only the reported message may stall, got %+v", stalls)
	}
}

func TestARedeliveredMessageThatNeverFinishesStallsWithoutAnError(t *testing.T) {
	h := newStallHarness(t)
	h.observe("r1", fifoInput("msg-A", 1), invocationRunning, "")
	h.observe("r2", fifoInput("msg-A", 2), invocationRunning, "")
	if len(h.stalls()) != 0 {
		t.Fatal("two deliveries must not stall")
	}
	h.observe("r3", fifoInput("msg-A", 3), invocationRunning, "")
	stalls := h.stalls()
	if len(stalls) != 1 || stalls[0].ReceiveCount != 3 || stalls[0].FailedAttempts != 0 {
		t.Fatalf("got %+v", stalls)
	}
}

func TestAHighReceiveCountStallsOnTheFirstFailure(t *testing.T) {
	h := newStallHarness(t)
	h.fail("r1", 5, "msg-A")
	if len(h.stalls()) != 1 {
		t.Fatal("a message redelivered five times is stuck")
	}
}

func TestRepeatedObservationsOfOneInvocationCountOnce(t *testing.T) {
	h := newStallHarness(t)
	h.fail("r1", 1, "msg-A")
	h.fail("r1", 1, "msg-A")
	h.fail("r1", 1, "msg-A")
	if len(h.stalls()) != 0 {
		t.Fatal("one invocation is one failed attempt")
	}
}

func TestTheStallStaysWhileARetryRuns(t *testing.T) {
	h := newStallHarness(t)
	h.fail("r1", 3, "msg-A")
	h.observe("r2", fifoInput("msg-A", 4), invocationRunning, "")
	if len(h.stalls()) != 1 {
		t.Fatal("a running retry must not clear the stall")
	}
}

func TestASuccessfulRunClearsTheStall(t *testing.T) {
	h := newStallHarness(t)
	h.fail("r1", 3, "msg-A")
	h.observe("r2", fifoInput("msg-A", 4), invocationSucceeded, `null`)
	if len(h.stalls()) != 0 {
		t.Fatal("a consumed message is no longer stuck")
	}
}

func TestALaterBatchOfTheSameFifoGroupClearsTheMessage(t *testing.T) {
	h := newStallHarness(t)
	h.fail("r1", 3, "msg-A")
	h.fail("r2", 1, "msg-B")
	if len(h.stalls()) != 0 {
		t.Fatal("msg-A left the head of its FIFO group")
	}
}

func TestAnUnrelatedStandardQueueDeliveryKeepsTheStall(t *testing.T) {
	h := newStallHarness(t)
	arn := "arn:aws:sqs:us-east-1:123456789012:x-local-webhooks"
	h.observe("r1", sqsBatch(arn, []string{"msg-A"}, 4, ""), invocationFailed, "")
	h.observe("r2", sqsBatch(arn, []string{"msg-B"}, 1, ""), invocationFailed, "")
	stalls := h.stalls()
	if len(stalls) != 1 || stalls[0].MessageID != "msg-A" {
		t.Fatalf("got %+v", stalls)
	}
}

func TestAStallExpiresOnceRedeliveriesStop(t *testing.T) {
	h := newStallHarness(t)
	arn := "arn:aws:sqs:us-east-1:123456789012:x-local-webhooks"
	h.observe("r1", sqsBatch(arn, []string{"msg-A"}, 4, ""), invocationFailed, "")
	h.clock = h.clock.Add(time.Minute)
	h.observe("r2", sqsBatch(arn, []string{"msg-B"}, 1, ""), invocationFailed, "")
	if len(h.stalls()) != 1 {
		t.Fatal("still inside the redelivery window")
	}
	h.clock = h.clock.Add(5 * time.Minute)
	h.observe("r3", sqsBatch(arn, []string{"msg-C"}, 1, ""), invocationFailed, "")
	if stalls := h.stalls(); len(stalls) != 0 {
		t.Fatalf("a message quiet for longer than the TTL left the queue, got %+v", stalls)
	}
}

func TestClearQueueForgetsTheQueue(t *testing.T) {
	h := newStallHarness(t)
	h.fail("r1", 3, "msg-A")
	h.tracker.ClearQueue(stallQueue)
	if len(h.stalls()) != 0 {
		t.Fatal("a purged queue must be forgotten")
	}
	h.fail("r2", 1, "msg-B")
	h.fail("r3", 1, "msg-B")
	if len(h.stalls()) != 0 {
		t.Fatal("attempts before the purge must not carry over")
	}
	h.fail("r4", 1, "msg-B")
	if len(h.stalls()) != 1 {
		t.Fatal("three fresh failures stall again")
	}
}

func TestStallWatcherAssemblesInvocationsFromEvents(t *testing.T) {
	clock := time.Unix(0, 0)
	w := newStallWatcher(func() time.Time { clock = clock.Add(time.Second); return clock })
	var changed bool
	var fresh []QueueStall
	for i := 1; i <= 3; i++ {
		id := fmt.Sprint("r", i)
		w.handle(&FunctionInvokedEvent{FunctionID: "fn", RequestID: id, Input: fifoInput("msg-A", 1)})
		changed, fresh = w.handle(&FunctionErrorEvent{FunctionID: "fn", RequestID: id, ErrorType: "Error", ErrorMessage: "boom"})
	}
	if !changed || len(fresh) != 1 || fresh[0].QueueName != stallQueue {
		t.Fatalf("the third failure must report a fresh stall, got %v %+v", changed, fresh)
	}
	if changed, _ := w.handle(&FunctionLogEvent{RequestID: "r3"}); changed {
		t.Fatal("unrelated events change nothing")
	}
	changed, fresh = w.handle(&QueuePurgedEvent{QueueName: stallQueue})
	if !changed || len(fresh) != 0 || len(w.tracker.Stalls()) != 0 {
		t.Fatal("a purge must clear the stall")
	}
	if changed, _ := w.handle(&QueuePurgedEvent{QueueName: stallQueue, Error: "AccessDenied"}); changed {
		t.Fatal("a failed purge changes nothing")
	}
}

func TestPurgeEnforcesTheCooldownAfterASuccess(t *testing.T) {
	calls := 0
	purger := newQueuePurger(func(ctx context.Context, queue string, region string) error {
		calls++
		if queue != stallQueue || region != "us-east-1" {
			t.Fatalf("purged %s@%s", queue, region)
		}
		return nil
	})
	now := time.Unix(100, 0)
	purger.now = func() time.Time { return now }

	if err := purger.Purge(context.Background(), stallQueue, "us-east-1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	err := purger.Purge(context.Background(), stallQueue, "us-east-1")
	if err == nil || !strings.Contains(err.Error(), "retry in 50s") || calls != 1 {
		t.Fatalf("a purge inside the cooldown must not reach AWS, got %v after %d calls", err, calls)
	}
	now = now.Add(60 * time.Second)
	if err := purger.Purge(context.Background(), stallQueue, "us-east-1"); err != nil || calls != 2 {
		t.Fatalf("got %v after %d calls", err, calls)
	}
}

func TestAFailedPurgeDoesNotStartTheCooldown(t *testing.T) {
	calls := 0
	purger := newQueuePurger(func(ctx context.Context, queue string, region string) error {
		calls++
		return errors.New("ExpiredToken")
	})
	purger.Purge(context.Background(), stallQueue, "")
	if err := purger.Purge(context.Background(), stallQueue, ""); err == nil || err.Error() != "ExpiredToken" || calls != 2 {
		t.Fatalf("an immediate retry must reach AWS, got %v after %d calls", err, calls)
	}
}

func TestAPurgeInProgressStartsTheCooldown(t *testing.T) {
	calls := 0
	purger := newQueuePurger(func(ctx context.Context, queue string, region string) error {
		calls++
		return errors.New("AWS.SimpleQueueService.PurgeQueueInProgress: Only one PurgeQueue operation is allowed every 60 seconds")
	})
	now := time.Unix(100, 0)
	purger.now = func() time.Time { return now }
	purger.Purge(context.Background(), stallQueue, "")
	now = now.Add(10 * time.Second)
	if err := purger.Purge(context.Background(), stallQueue, ""); err == nil || !strings.Contains(err.Error(), "retry in 50s") || calls != 1 {
		t.Fatalf("got %v after %d calls", err, calls)
	}
}
