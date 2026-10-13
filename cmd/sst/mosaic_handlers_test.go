package main

import (
	"testing"

	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
)

func TestHandlerCatalogListsObservedHandlersOnce(t *testing.T) {
	catalog := newHandlerCatalog()
	catalog.Observe(&aws.FunctionInvokedEvent{Handler: "orders/get", Label: "GET /orders"})
	catalog.Observe(&aws.FunctionInvokedEvent{Handler: "billing/sync", Label: "sqs billing"})
	catalog.Observe(&aws.FunctionInvokedEvent{Handler: "orders/get", Label: "GET /orders"})
	catalog.Observe(&aws.FunctionInvokedEvent{FunctionID: "plain"})

	options := catalog.Options()
	if len(options) != 2 {
		t.Fatalf("expected two handlers, got %+v", options)
	}
	if options[0].Value != "billing/sync" || options[0].Description != "sqs billing" || options[1].Value != "orders/get" {
		t.Fatalf("unexpected options %+v", options)
	}
}
