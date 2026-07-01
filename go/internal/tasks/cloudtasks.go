package tasks

import (
	"context"
	"fmt"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"automation-agent/internal/ingest"
	"automation-agent/internal/obs"
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
	deadline    time.Duration // explicit per-task dispatch deadline (HTTP-target default is only 10m)
	now         func() time.Time
}

// NewCloudTasks opens a Cloud Tasks client and targets the queue
// projects/<project>/locations/<location>/queues/<queue>. dispatchURL is the full URL of
// the /internal/dispatch worker; token is the static INTERNAL_TOKEN the task carries as a
// Bearer header (the same auth /internal/dispatch already enforces). deadline is the explicit
// per-task dispatch deadline (config validated to Cloud Tasks' 15s..30m range). Close releases
// the client.
func NewCloudTasks(ctx context.Context, project, location, queue, dispatchURL, token string, deadline time.Duration) (*CloudTasks, error) {
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
		deadline:    deadline,
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
	// Carry the trace context across the queue hop as a W3C traceparent header (not inside
	// the envelope JSON), so the dispatch that runs this task continues the ingress trace.
	// A no-op when tracing is disabled or ctx has no span — Inject then returns nothing.
	for k, v := range obs.Inject(ctx) {
		headers[k] = v
	}
	task := &taskspb.Task{
		MessageType: &taskspb.Task_HttpRequest{HttpRequest: &taskspb.HttpRequest{
			HttpMethod: taskspb.HttpMethod_POST,
			Url:        c.dispatchURL,
			Headers:    headers,
			Body:       body,
		}},
	}
	// Set the dispatch deadline explicitly: the HTTP-target default is only 10m, so a longer
	// workflow would be cancelled mid-run and retried (duplicating side effects). Skipped when
	// unset (zero) so the queue default applies — production always supplies it via config.
	if c.deadline > 0 {
		task.DispatchDeadline = durationpb.New(c.deadline)
	}
	if o.Name != "" {
		task.Name = c.queuePath + "/tasks/" + o.Name
	}
	if o.Delay > 0 {
		task.ScheduleTime = timestamppb.New(c.now().Add(o.Delay))
	}

	if _, err := c.client.CreateTask(ctx, &taskspb.CreateTaskRequest{Parent: c.queuePath, Task: task}); err != nil {
		// A named task that already exists is a successful coalesce, not a failure: a burst of
		// pushes (or a re-delivered webhook) within the dedup window collapses onto the one task
		// already scheduled. Surfacing it would 500 the webhook and trigger source retries that
		// keep colliding for the ~1h the name stays reserved.
		if status.Code(err) == codes.AlreadyExists {
			return nil
		}
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
