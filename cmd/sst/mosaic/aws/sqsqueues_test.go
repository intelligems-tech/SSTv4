package aws

import (
	"context"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/sst/sst/v3/pkg/project"
)

func TestQueuesFromCompleteReadsQueuesAndRedrive(t *testing.T) {
	evt := &project.CompleteEvent{Resources: []apitype.ResourceV3{
		{Type: "aws:sqs/queue:Queue", Outputs: map[string]interface{}{
			"name": "x-jobs.fifo", "arn": "arn:aws:sqs:us-east-1:1:x-jobs.fifo", "url": "https://sqs/x-jobs.fifo", "fifoQueue": true,
			"redrivePolicy": `{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:1:x-jobs-dlq.fifo","maxReceiveCount":5}`,
		}},
		{Type: "aws:sqs/queue:Queue", Outputs: map[string]interface{}{"name": "x-jobs-dlq.fifo", "arn": "arn:aws:sqs:us-east-1:1:x-jobs-dlq.fifo", "url": "https://sqs/x-jobs-dlq.fifo"}},
		{Type: "aws:sqs/queue:Queue", Outputs: map[string]interface{}{"name": "pending"}},
		{Type: "aws:lambda/function:Function", Outputs: map[string]interface{}{"name": "fn", "url": "x"}},
	}}
	queues := QueuesFromComplete(evt)
	if len(queues) != 2 {
		t.Fatalf("only deployed queues count, got %+v", queues)
	}
	dlq, main := queues[0], queues[1]
	if dlq.Name != "x-jobs-dlq.fifo" || dlq.DLQArn != "" {
		t.Fatalf("unexpected dlq %+v", dlq)
	}
	if !main.Fifo || main.DLQArn != "arn:aws:sqs:us-east-1:1:x-jobs-dlq.fifo" || main.MaxReceive != 5 || QueueNameFromArn(main.DLQArn) != dlq.Name {
		t.Fatalf("unexpected queue %+v", main)
	}
}

func TestReadDepthsKeepsQueueOrder(t *testing.T) {
	queues := []QueueResource{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	depths := readDepths(context.Background(), queues, func(ctx context.Context, q QueueResource) QueueDepth {
		return QueueDepth{QueueName: q.Name, Visible: len(q.Name)}
	})
	for i, depth := range depths {
		if depth.QueueName != queues[i].Name {
			t.Fatalf("depth %d is %s", i, depth.QueueName)
		}
	}
}
