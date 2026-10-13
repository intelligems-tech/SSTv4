package ui

import (
	"context"
	"testing"
	"time"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
)

func TestHandlerFilterFollowsAPooledRequestUntilItEnds(t *testing.T) {
	u := New(context.Background(), WithSilent)
	u.options.Filter = "orders"
	u.Event(&aws.FunctionInvokedEvent{FunctionID: "Pool", WorkerID: "w", RequestID: "r1", Handler: "orders/get", Label: "GET /orders"})

	if !u.matchInvocation("Pool", "r1") {
		t.Fatal("a request running a matching handler must pass the filter")
	}
	if u.matchInvocation("Pool", "r2") {
		t.Fatal("another request on the same pool must not pass the filter")
	}
	u.Event(&aws.FunctionResponseEvent{FunctionID: "Pool", WorkerID: "w", RequestID: "r1"})
	if _, ok := u.handlers["r1"]; ok {
		t.Fatal("a finished request must be forgotten")
	}
}

func TestInvocationNamePrefersTheReportedLabel(t *testing.T) {
	u := New(context.Background(), WithSilent)
	if got := u.invocationName(&aws.FunctionInvokedEvent{FunctionID: "Fn", Handler: "cron/sync", Label: "cron/sync"}); got != "cron/sync" {
		t.Fatalf("got %q", got)
	}
	if got := u.invocationName(&aws.FunctionInvokedEvent{FunctionID: "Fn"}); got != "Fn" {
		t.Fatalf("an unlabelled invocation must fall back to the function, got %q", got)
	}
}

func TestDurationStyleThresholds(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Millisecond: TEXT_SUCCESS.Render("x"),
		2 * time.Second:        TEXT_WARNING.Render("x"),
		4 * time.Second:        TEXT_DANGER.Render("x"),
	}
	for duration, want := range cases {
		if got := DurationStyle(duration).Render("x"); got != want {
			t.Fatalf("%v rendered %q, want %q", duration, got, want)
		}
	}
}
