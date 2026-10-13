package invocations

import (
	"testing"
	"time"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestStore() (*Store, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return NewStore(clock.now), clock
}

func TestStoreAssemblesAnInvocationFromEvents(t *testing.T) {
	s, clock := newTestStore()
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "Pool", WorkerID: "w1", RequestID: "r1", Input: []byte(`{"rawPath":"/orders","requestContext":{"http":{"method":"GET"}}}`), Handler: "orders/get", Label: "GET /orders"})
	s.Apply(&aws.FunctionLogEvent{RequestID: "r1", Line: "loading"})
	clock.advance(250 * time.Millisecond)
	s.Apply(&aws.FunctionResponseEvent{RequestID: "r1", Output: []byte(`{"ok":true}`)})

	groups := s.Groups("")
	if len(groups) != 1 || groups[0].Title != "orders/get" || groups[0].Subtitle != "GET /orders" || groups[0].Total != 1 || groups[0].Running != 0 {
		t.Fatalf("unexpected groups %+v", groups)
	}
	inv := groups[0].Invocations[0]
	if inv.Status != Succeeded || inv.Duration(clock.now()) != 250*time.Millisecond || len(inv.Logs) != 1 || !inv.Cold || string(inv.Output) != `{"ok":true}` {
		t.Fatalf("unexpected invocation %+v", inv)
	}
	if Preview(inv) != "GET /orders" {
		t.Fatalf("preview %q", Preview(inv))
	}
}

func TestStoreGroupsUnlabelledInvocationsByFunctionAndCountsErrors(t *testing.T) {
	s, _ := newTestStore()
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "Cron", WorkerID: "w1", RequestID: "r1"})
	s.Apply(&aws.FunctionErrorEvent{RequestID: "r1", ErrorType: "Error", ErrorMessage: "boom"})
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "Cron", WorkerID: "w1", RequestID: "r2"})

	group := s.Group("Cron")
	if group == nil || group.Errors != 1 || group.Running != 1 || s.Errors != 1 || s.Total != 2 {
		t.Fatalf("unexpected group %+v", group)
	}
	if group.Invocations[1].Cold {
		t.Fatal("the second request on a worker is warm")
	}
	if list := group.List(""); list[0].RequestID != "r2" {
		t.Fatal("invocations list newest first")
	}
}

func TestStoreKeepsABoundedHistoryPerGroup(t *testing.T) {
	s, _ := newTestStore()
	for i := 0; i < perGroup+5; i++ {
		id := string(rune('a'+i%26)) + string(rune('0'+i/26))
		s.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", WorkerID: "w", RequestID: id})
	}
	group := s.Group("Fn")
	if len(group.Invocations) != perGroup || group.Total != perGroup+5 || group.Running != perGroup {
		t.Fatalf("kept %d, total %d, running %d", len(group.Invocations), group.Total, group.Running)
	}
	if len(s.requests) != perGroup {
		t.Fatalf("evicted running requests must be forgotten, tracking %d", len(s.requests))
	}
}

func TestStoreIgnoresEventsForUnknownRequests(t *testing.T) {
	s, _ := newTestStore()
	if s.Apply(&aws.FunctionLogEvent{RequestID: "nope", Line: "x"}) || s.Apply(&aws.FunctionResponseEvent{RequestID: "nope"}) {
		t.Fatal("events for a request the pane never saw change nothing")
	}
}

func TestGroupsSortByActivityWindowThenTitle(t *testing.T) {
	s, clock := newTestStore()
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "b", RequestID: "1"})
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "a", RequestID: "2"})
	clock.advance(time.Minute)
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "c", RequestID: "3"})

	groups := s.Groups("")
	if groups[0].Key != "c" || groups[1].Key != "a" || groups[2].Key != "b" {
		t.Fatalf("got %s %s %s", groups[0].Key, groups[1].Key, groups[2].Key)
	}
	if filtered := s.Groups("bb"); len(filtered) != 0 {
		t.Fatal("the filter must match in order")
	}
}

func TestSearchFindsLogsAndErrors(t *testing.T) {
	s, _ := newTestStore()
	s.Apply(&aws.FunctionInvokedEvent{FunctionID: "Fn", RequestID: "r1"})
	s.Apply(&aws.FunctionLogEvent{RequestID: "r1", Line: "Connecting to Postgres"})
	s.Apply(&aws.FunctionErrorEvent{RequestID: "r1", ErrorType: "Error", ErrorMessage: "postgres timeout"})

	matches := s.Search("POSTGRES")
	if len(matches) != 2 || matches[0].Line != 0 || matches[1].Line != -1 {
		t.Fatalf("got %+v", matches)
	}
	if s.Search("  ") != nil {
		t.Fatal("a blank query matches nothing")
	}
}

func TestPreviewSummarisesEventShapes(t *testing.T) {
	cases := map[string]string{
		`{"httpMethod":"POST","path":"/v1/x"}`: "POST /v1/x",
		`{"Records":[{"eventSource":"aws:sqs","eventSourceARN":"arn:aws:sqs:us-east-1:1:jobs.fifo"},{"eventSource":"aws:sqs"}]}`: "sqs jobs.fifo ×2",
		`{"source":"aws.events","detail-type":"Scheduled Event"}`:                                                                "aws.events Scheduled Event",
		`{"ding":true}`: `{"ding":true}`,
	}
	for input, want := range cases {
		if got := Preview(&Invocation{Input: []byte(input)}); got != want {
			t.Fatalf("%s: got %q, want %q", input, got, want)
		}
	}
}
