package flag

import "os"

// Sidebar status block of `sst dev`: SST_DEV_STATUS is comma separated
// `key=value` segments (e.g. `resources=dev,frontend=local`), and
// SST_DEV_STAGE_TONE colours the stage: `local`, `shared` or `prod`.
var SST_DEV_STATUS = os.Getenv("SST_DEV_STATUS")
var SST_DEV_STAGE_TONE = os.Getenv("SST_DEV_STAGE_TONE")

// SST_DEV_AUTO_PURGE purges an SQS queue as soon as `sst dev` sees one of its
// messages stuck, instead of waiting for the purge key.
var SST_DEV_AUTO_PURGE = isTrue("SST_DEV_AUTO_PURGE")

// SST_DEV_QUEUE_DEPTH_OFF stops `sst dev` reading every deployed queue's
// depth for the Queues pane.
var SST_DEV_QUEUE_DEPTH_OFF = isTrue("SST_DEV_QUEUE_DEPTH_OFF")
