package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	gax "github.com/googleapis/gax-go/v2"

	"automation-agent/internal/ingest"
)

// fakeSubmitter records the last CreateTask request and returns a configurable error.
type fakeSubmitter struct {
	req *taskspb.CreateTaskRequest
	err error
}

func (f *fakeSubmitter) CreateTask(_ context.Context, req *taskspb.CreateTaskRequest, _ ...gax.CallOption) (*taskspb.Task, error) {
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	return req.Task, nil
}

// newTestCloudTasks builds a CloudTasks over a fake submitter with a fixed clock, so task
// building is exercised without a live gRPC client.
func newTestCloudTasks(f *fakeSubmitter, token string) *CloudTasks {
	return &CloudTasks{
		client:      f,
		queuePath:   "projects/p/locations/l/queues/q",
		dispatchURL: "https://svc.run.app/internal/dispatch",
		token:       token,
		now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
}

func httpReq(t *testing.T, task *taskspb.Task) *taskspb.HttpRequest {
	t.Helper()
	hr, ok := task.MessageType.(*taskspb.Task_HttpRequest)
	if !ok {
		t.Fatalf("task message type = %T, want HttpRequest", task.MessageType)
	}
	return hr.HttpRequest
}

// A plain enqueue builds a POST task targeting /internal/dispatch, carrying the encoded
// envelope as the body and the INTERNAL_TOKEN as a Bearer header, with no name/schedule.
func TestCloudTasksBuildsTask(t *testing.T) {
	f := &fakeSubmitter{}
	ct := newTestCloudTasks(f, "sekret")
	env := ingest.New(ingest.KindCI, "webhook:/github", []byte(`{"action":"completed"}`), time.Unix(0, 0).UTC())

	if err := ct.Enqueue(context.Background(), env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if f.req.Parent != "projects/p/locations/l/queues/q" {
		t.Errorf("parent = %q", f.req.Parent)
	}
	hr := httpReq(t, f.req.Task)
	if hr.HttpMethod != taskspb.HttpMethod_POST {
		t.Errorf("method = %v, want POST", hr.HttpMethod)
	}
	if hr.Url != "https://svc.run.app/internal/dispatch" {
		t.Errorf("url = %q", hr.Url)
	}
	if hr.Headers["Authorization"] != "Bearer sekret" {
		t.Errorf("auth header = %q", hr.Headers["Authorization"])
	}
	if hr.Headers["Content-Type"] != "application/json" {
		t.Errorf("content-type = %q", hr.Headers["Content-Type"])
	}
	// The body is the exact wire codec output, and decodes back to the envelope.
	want, _ := ingest.Encode(env)
	if string(hr.Body) != string(want) {
		t.Errorf("body = %s, want %s", hr.Body, want)
	}
	out, err := ingest.Decode(hr.Body)
	if err != nil || out.Kind != ingest.KindCI {
		t.Errorf("body did not round-trip: %v / %q", err, out.Kind)
	}
	if f.req.Task.Name != "" {
		t.Errorf("unexpected task name %q (no dedup requested)", f.req.Task.Name)
	}
	if f.req.Task.ScheduleTime != nil {
		t.Errorf("unexpected schedule time (no delay requested)")
	}
}

// The optional dedup name and schedule delay are carried onto the built task.
func TestCloudTasksHonorsNameAndDelay(t *testing.T) {
	f := &fakeSubmitter{}
	ct := newTestCloudTasks(f, "")
	env := ingest.New(ingest.KindCoverage, "webhook:/coverage", []byte("{}"), time.Unix(0, 0))

	if err := ct.Enqueue(context.Background(), env, WithName("pr-42"), WithDelay(30*time.Second)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if got, want := f.req.Task.Name, "projects/p/locations/l/queues/q/tasks/pr-42"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if f.req.Task.ScheduleTime == nil {
		t.Fatal("schedule time not set")
	}
	if got := f.req.Task.ScheduleTime.AsTime(); !got.Equal(time.Unix(1_700_000_030, 0).UTC()) {
		t.Errorf("schedule time = %v, want now+30s", got)
	}
	// With no token configured, no Authorization header is attached.
	if _, ok := httpReq(t, f.req.Task).Headers["Authorization"]; ok {
		t.Error("Authorization header set despite empty token")
	}
}

// An envelope whose encoded body exceeds the Cloud Tasks task-size limit is refused
// up front rather than failing opaquely at CreateTask (spec §9).
func TestCloudTasksRejectsOversizeEnvelope(t *testing.T) {
	f := &fakeSubmitter{}
	ct := newTestCloudTasks(f, "")
	big := ingest.New(ingest.KindLint, "s", make([]byte, MaxTaskBytes+1), time.Unix(0, 0))
	err := ct.Enqueue(context.Background(), big)
	if err == nil || !strings.Contains(err.Error(), "task limit") {
		t.Fatalf("err = %v, want task-limit error", err)
	}
	if f.req != nil {
		t.Error("oversize envelope should not reach CreateTask")
	}
}

// A CreateTask failure surfaces to the caller (which becomes a 500 → the webhook source
// retries, and the queue itself retries an /internal/dispatch failure).
func TestCloudTasksSurfacesSubmitError(t *testing.T) {
	f := &fakeSubmitter{err: errors.New("unavailable")}
	ct := newTestCloudTasks(f, "")
	err := ct.Enqueue(context.Background(), ingest.New(ingest.KindCI, "s", nil, time.Unix(0, 0)))
	if err == nil || !strings.Contains(err.Error(), "create task") {
		t.Fatalf("err = %v, want create-task error", err)
	}
}

// Close releases the underlying client, and is a no-op when none is set.
func TestCloudTasksClose(t *testing.T) {
	closed := false
	ct := &CloudTasks{closer: func() error { closed = true; return nil }}
	if err := ct.Close(); err != nil || !closed {
		t.Fatalf("Close: err=%v closed=%v", err, closed)
	}
	if err := (&CloudTasks{}).Close(); err != nil {
		t.Errorf("Close with no client = %v, want nil", err)
	}
}
