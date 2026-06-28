package tasks

import (
	"context"
	"fmt"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	"automation-agent/internal/ingest"
)

// MaxTaskBytes is the Cloud Tasks size limit for an HTTP-target task (1 MiB; verify
// against current quota docs). Enqueue refuses an envelope whose encoded body exceeds it
// rather than letting Cloud Tasks reject the CreateTask call opaquely (spec §9). Today's
// payloads are metadata well under this (PR diffs are fetched later via the API, not
// carried in the webhook body); if a future payload could exceed it, the fallback is
// store-in-Firestore + enqueue a reference — noted in the spec, not built here.
const MaxTaskBytes = 1 << 20

// submitter is the slice of the Cloud Tasks client this backend uses, isolated so the
// task-building can be unit-tested against a fake without a live gRPC connection.
type submitter interface {
	CreateTask(ctx context.Context, req *taskspb.CreateTaskRequest, opts ...gax.CallOption) (*taskspb.Task, error)
}

// CloudTasks enqueues each envelope as a Cloud Tasks HTTP-target task pointed at
// /internal/dispatch — the production backend. The queue gives durable retry with backoff
// (a task survives the instance being reclaimed mid-run and is redelivered) and rate
// limiting (the queue's max-concurrent-dispatches replaces the in-process semaphore), and
// the worker runs in-request so CPU stays allocated for the whole compute.
type CloudTasks struct {
	client      submitter
	closer      func() error
	queuePath   string
	dispatchURL string
	token       string
	now         func() time.Time
}

// NewCloudTasks opens a Cloud Tasks client and targets the queue
// projects/<project>/locations/<location>/queues/<queue>. dispatchURL is the full URL of
// the /internal/dispatch worker; token is the static INTERNAL_TOKEN the task carries as a
// Bearer header (the same auth /internal/dispatch already enforces). Close releases the
// client.
func NewCloudTasks(ctx context.Context, project, location, queue, dispatchURL, token string) (*CloudTasks, error) {
	client, err := cloudtasks.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks: cloud tasks client: %w", err)
	}
	return &CloudTasks{
		client:      client,
		closer:      client.Close,
		queuePath:   fmt.Sprintf("projects/%s/locations/%s/queues/%s", project, location, queue),
		dispatchURL: dispatchURL,
		token:       token,
		now:         time.Now,
	}, nil
}

// Enqueue builds and submits a task carrying the JSON-encoded envelope as its body and the
// INTERNAL_TOKEN as a Bearer header. A Name option sets the task name (Cloud Tasks dedup);
// a Delay option sets the schedule time.
func (c *CloudTasks) Enqueue(ctx context.Context, e ingest.Envelope, opts ...Option) error {
	o := apply(opts)
	body, err := ingest.Encode(e)
	if err != nil {
		return err
	}
	if len(body) > MaxTaskBytes {
		return fmt.Errorf("tasks: envelope is %d bytes, over the %d-byte Cloud Tasks task limit", len(body), MaxTaskBytes)
	}

	headers := map[string]string{"Content-Type": "application/json"}
	if c.token != "" {
		headers["Authorization"] = "Bearer " + c.token
	}
	task := &taskspb.Task{
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
			HttpMethod: taskspb.HttpMethod_POST,
			Url:        c.dispatchURL,
			Headers:    headers,
			Body:       body,
		}},
	}
	if o.Name != "" {
		task.Name = c.queuePath + "/tasks/" + o.Name
	}
	if o.Delay > 0 {
		task.ScheduleTime = timestamppb.New(c.now().Add(o.Delay))
	}

	if _, err := c.client.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: c.queuePath, Task: task}); err != nil {
		return fmt.Errorf("tasks: create task: %w", err)
	}
	return nil
}

// Close releases the underlying Cloud Tasks client.
func (c *CloudTasks) Close() error {
	if c.closer != nil {
		return c.closer()
	}
	return nil
}
