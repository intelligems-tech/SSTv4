package aws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/sst/sst/v3/pkg/bus"
	"github.com/sst/sst/v3/pkg/flag"
	"github.com/sst/sst/v3/pkg/project"
)

// queueResourceType is the Pulumi type of an SQS queue; sst.aws.Queue wraps
// one, so this finds queues made either way.
const queueResourceType = "aws:sqs/queue:Queue"

// queueDepthInterval is how often `sst dev` reads every queue's depth.
const queueDepthInterval = 15 * time.Second

// QueueResource is an SQS queue the app deployed.
type QueueResource struct {
	Name string
	Arn  string
	URL  string
	Fifo bool
	// DLQArn is the dead-letter queue messages move to after MaxReceive
	// failed receives, when the queue has a redrive policy.
	DLQArn     string
	MaxReceive int
}

// QueuesFromComplete lists the app's queues from a deploy, sorted by name.
func QueuesFromComplete(evt *project.CompleteEvent) []QueueResource {
	queues := []QueueResource{}
	for _, resource := range evt.Resources {
		if string(resource.Type) != queueResourceType {
			continue
		}
		outputs := resource.Outputs
		queue := QueueResource{
			Name: stringOutput(outputs, "name"),
			Arn:  stringOutput(outputs, "arn"),
			URL:  stringOutput(outputs, "url"),
		}
		if queue.Name == "" || queue.URL == "" {
			continue
		}
		queue.Fifo, _ = outputs["fifoQueue"].(bool)
		var redrive struct {
			DeadLetterTargetArn string      `json:"deadLetterTargetArn"`
			MaxReceiveCount     json.Number `json:"maxReceiveCount"`
		}
		if raw := stringOutput(outputs, "redrivePolicy"); raw != "" && json.Unmarshal([]byte(raw), &redrive) == nil {
			queue.DLQArn = redrive.DeadLetterTargetArn
			if count, err := redrive.MaxReceiveCount.Int64(); err == nil {
				queue.MaxReceive = int(count)
			}
		}
		queues = append(queues, queue)
	}
	sort.Slice(queues, func(i, j int) bool { return queues[i].Name < queues[j].Name })
	return queues
}

func stringOutput(outputs map[string]interface{}, key string) string {
	value, _ := outputs[key].(string)
	return value
}

// QueueDepth is a queue's approximate message counts, read from SQS.
type QueueDepth struct {
	QueueName string
	Visible   int
	InFlight  int
	Delayed   int
	Error     string `json:",omitempty"`
}

// QueueDepthsEvent carries every queue's depth after each poll.
type QueueDepthsEvent struct {
	Depths []QueueDepth
	At     time.Time
}

type depthReader func(ctx context.Context, queue QueueResource) QueueDepth

// sqsDepth reads a queue's approximate counts with the dev credentials.
func sqsDepth(config aws.Config) depthReader {
	client := sqs.NewFromConfig(config)
	return func(ctx context.Context, queue QueueResource) QueueDepth {
		depth := QueueDepth{QueueName: queue.Name}
		out, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(queue.URL),
			AttributeNames: []types.QueueAttributeName{
				types.QueueAttributeNameApproximateNumberOfMessages,
				types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
				types.QueueAttributeNameApproximateNumberOfMessagesDelayed,
			},
		})
		if err != nil {
			depth.Error = err.Error()
			return depth
		}
		attribute := func(name types.QueueAttributeName) int {
			value, _ := strconv.Atoi(out.Attributes[string(name)])
			return value
		}
		depth.Visible = attribute(types.QueueAttributeNameApproximateNumberOfMessages)
		depth.InFlight = attribute(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)
		depth.Delayed = attribute(types.QueueAttributeNameApproximateNumberOfMessagesDelayed)
		return depth
	}
}

// readDepths reads every queue, a few at a time, in queue order.
func readDepths(ctx context.Context, queues []QueueResource, read depthReader) []QueueDepth {
	depths := make([]QueueDepth, len(queues))
	limit := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, queue := range queues {
		wg.Add(1)
		limit <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-limit }()
			depths[i] = read(ctx, queue)
		}()
	}
	wg.Wait()
	return depths
}

// watchQueueDepths polls the depth of every deployed queue and publishes a
// QueueDepthsEvent, right after each deploy and then on an interval. A purge
// triggers an early poll so the pane shows the empty queue.
func watchQueueDepths(ctx context.Context, read depthReader) {
	if flag.SST_DEV_QUEUE_DEPTH_OFF {
		return
	}
	evts := bus.Subscribe(&project.CompleteEvent{}, &QueuePurgedEvent{})
	ticker := time.NewTicker(queueDepthInterval)
	defer ticker.Stop()
	var queues []QueueResource
	poll := func(queues []QueueResource) {
		if len(queues) == 0 {
			return
		}
		depths := readDepths(ctx, queues, read)
		for _, depth := range depths {
			if depth.Error != "" {
				slog.Warn("queue depth", "queue", depth.QueueName, "error", depth.Error)
			}
		}
		bus.Publish(&QueueDepthsEvent{Depths: depths, At: time.Now()})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-evts:
			switch evt := evt.(type) {
			case *project.CompleteEvent:
				if evt.Old || !evt.Finished {
					continue
				}
				queues = QueuesFromComplete(evt)
				go poll(queues)
			case *QueuePurgedEvent:
				if evt.Error == "" {
					go poll(queues)
				}
			}
		case <-ticker.C:
			go poll(queues)
		}
	}
}

// QueueNameFromArn is the last segment of an SQS ARN.
func QueueNameFromArn(arn string) string {
	parts := strings.Split(arn, ":")
	return parts[len(parts)-1]
}
