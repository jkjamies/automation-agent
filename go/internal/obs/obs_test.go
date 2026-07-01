package obs

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// restoreGlobals snapshots the OTel global tracer provider and propagator and restores them
// when the test ends. Init/install mutate process-global state; this keeps tests isolated.
func restoreGlobals(t *testing.T) {
	t.Helper()
	tp := otel.GetTracerProvider()
	prop := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(prop)
	})
}

// installRecording registers a provider exporting to a fresh in-memory exporter (via the
// same install path Init uses) and returns the exporter. The provider uses a
// BatchSpanProcessor, so ended spans are buffered until Flush — exactly the production
// shape the flush-on-return guard depends on.
func installRecording(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	restoreGlobals(t)
	exp := tracetest.NewInMemoryExporter()
	shutdown := install(exp, Config{ServiceName: "automation-agent-test"})
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return exp
}

// emitFakeAgentTree creates the agent framework's native span shape — invoke_agent ->
// call_llm -> execute_tool — with representative GenAI-semconv attribute keys, without any
// model call. It stands in for the framework's auto-emitted tree so tests assert on span
// structure and attribute keys (never on LLM output text).
func emitFakeAgentTree(ctx context.Context) {
	tr := otel.GetTracerProvider().Tracer("obs-test")
	ctx, invoke := tr.Start(ctx, "invoke_agent automation_agent")
	llmCtx, llm := tr.Start(ctx, "call_llm gemma")
	llm.SetAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", "gemma4:12b"),
		attribute.Int("gen_ai.usage.input_tokens", 12),
	)
	_, tool := tr.Start(llmCtx, "execute_tool apply_fix")
	tool.SetAttributes(attribute.String("gen_ai.tool.name", "apply_fix"))
	tool.End()
	llm.End()
	invoke.End()
}

func TestInitNoneIsNoOp(t *testing.T) {
	restoreGlobals(t)
	before := otel.GetTracerProvider()

	shutdown, err := Init(context.Background(), Config{Exporter: ExporterNone})
	if err != nil {
		t.Fatalf("Init(none): %v", err)
	}
	if otel.GetTracerProvider() != before {
		t.Error("Init(none) replaced the global tracer provider; it must install nothing")
	}
	// The no-op Shutdown must succeed and do nothing observable.
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op Shutdown returned error: %v", err)
	}
}

func TestInitEmptyExporterIsNoOp(t *testing.T) {
	restoreGlobals(t)
	before := otel.GetTracerProvider()
	if _, err := Init(context.Background(), Config{}); err != nil {
		t.Fatalf("Init(empty): %v", err)
	}
	if otel.GetTracerProvider() != before {
		t.Error("Init with empty exporter replaced the global provider; empty must mean none")
	}
}

func TestInitUnknownExporterRejected(t *testing.T) {
	restoreGlobals(t)
	if _, err := Init(context.Background(), Config{Exporter: "jaeger"}); err == nil {
		t.Error("Init with an unknown exporter must return an error")
	}
}

func TestInitConsoleRegistersProvider(t *testing.T) {
	restoreGlobals(t)
	before := otel.GetTracerProvider()
	shutdown, err := Init(context.Background(), Config{Exporter: ExporterConsole, ServiceName: "automation-agent"})
	if err != nil {
		t.Fatalf("Init(console): %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	if otel.GetTracerProvider() == before {
		t.Error("Init(console) did not register a new global provider")
	}
	// The propagator must be the W3C TraceContext so cross-process propagation round-trips.
	if _, ok := otel.GetTextMapPropagator().(propagation.TraceContext); !ok {
		t.Errorf("Init(console) did not set the W3C TraceContext propagator, got %T", otel.GetTextMapPropagator())
	}
}

func TestInitOTLPRequiresEndpoint(t *testing.T) {
	restoreGlobals(t)
	if _, err := Init(context.Background(), Config{Exporter: ExporterOTLP}); err == nil {
		t.Error("Init(otlp) with no endpoint must return an error")
	}
}

func TestInitOTLPWithEndpointBuilds(t *testing.T) {
	restoreGlobals(t)
	before := otel.GetTracerProvider()
	// otlptracehttp does not dial at construction, so a well-formed endpoint builds a
	// provider without a live collector.
	shutdown, err := Init(context.Background(), Config{
		Exporter:     ExporterOTLP,
		ServiceName:  "automation-agent",
		OTLPEndpoint: "http://localhost:4318",
		OTLPHeaders:  "api-key=secret",
	})
	if err != nil {
		t.Fatalf("Init(otlp): %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	if otel.GetTracerProvider() == before {
		t.Error("Init(otlp) did not register a new global provider")
	}
}

func TestInitGCPWithoutCredentials(t *testing.T) {
	restoreGlobals(t)
	// The Cloud Trace exporter needs Application Default Credentials. In a unit environment
	// there are none, so Init surfaces a build error (rather than silently exporting nowhere).
	// If ADC happens to be present, the provider builds — accept both, exercising the branch.
	shutdown, err := Init(context.Background(), Config{Exporter: ExporterGCP, ServiceName: "automation-agent"})
	if err != nil {
		if !strings.Contains(err.Error(), "gcp") {
			t.Errorf("gcp build error should name the exporter, got: %v", err)
		}
		return
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
}

func TestRecordedSpanTreeAndGenAIAttributes(t *testing.T) {
	exp := installRecording(t)

	emitFakeAgentTree(context.Background())
	if err := Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans (invoke_agent -> call_llm -> execute_tool), got %d", len(spans))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	invoke, ok := byName["invoke_agent automation_agent"]
	if !ok {
		t.Fatal("missing invoke_agent span")
	}
	llm, ok := byName["call_llm gemma"]
	if !ok {
		t.Fatal("missing call_llm span")
	}
	tool, ok := byName["execute_tool apply_fix"]
	if !ok {
		t.Fatal("missing execute_tool span")
	}

	// Tree shape: call_llm is a child of invoke_agent; execute_tool is a child of call_llm;
	// all three share one trace.
	if llm.Parent.SpanID() != invoke.SpanContext.SpanID() {
		t.Error("call_llm is not a child of invoke_agent")
	}
	if tool.Parent.SpanID() != llm.SpanContext.SpanID() {
		t.Error("execute_tool is not a child of call_llm")
	}
	if invoke.SpanContext.TraceID() != llm.SpanContext.TraceID() || llm.SpanContext.TraceID() != tool.SpanContext.TraceID() {
		t.Error("spans are not in the same trace")
	}

	// GenAI-semconv attribute keys are preserved (assert on keys/structure, not LLM text).
	assertHasAttrKey(t, llm, "gen_ai.request.model")
	assertHasAttrKey(t, llm, "gen_ai.usage.input_tokens")
	assertHasAttrKey(t, tool, "gen_ai.tool.name")
}

func TestFlushExportsBeforeReturn(t *testing.T) {
	exp := installRecording(t)

	emitFakeAgentTree(context.Background())
	// BatchSpanProcessor buffers ended spans and exports on a background timer (~5s). Without
	// an explicit flush nothing has shipped yet — this is the scale-to-zero loss window.
	if got := len(exp.GetSpans()); got != 0 {
		t.Fatalf("spans exported before Flush (%d); the batch processor should still be buffering", got)
	}
	if err := Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(exp.GetSpans()); got == 0 {
		t.Fatal("Flush did not export buffered spans")
	}
}

func TestFlushWithoutProviderIsNoOp(t *testing.T) {
	restoreGlobals(t)
	// With tracing disabled the global is the framework's no-op provider (no ForceFlush);
	// Flush must be a safe no-op rather than panic.
	otel.SetTracerProvider(noop.NewTracerProvider())
	if err := Flush(context.Background()); err != nil {
		t.Errorf("Flush with no SDK provider returned error: %v", err)
	}
}

func TestParseSamplerDefaults(t *testing.T) {
	// Unknown/empty fall back to the always-on default rather than failing.
	for _, name := range []string{"", "parentbased_always_on", "nonsense"} {
		if got := parseSampler(name).Description(); got == "" {
			t.Errorf("parseSampler(%q) returned a sampler with no description", name)
		}
	}
	if parseSampler("always_off").Description() != sdktrace.NeverSample().Description() {
		t.Error("always_off did not map to NeverSample")
	}
	if parseSampler("always_on").Description() != sdktrace.AlwaysSample().Description() {
		t.Error("always_on did not map to AlwaysSample")
	}
	if parseSampler("parentbased_always_off").Description() != sdktrace.ParentBased(sdktrace.NeverSample()).Description() {
		t.Error("parentbased_always_off did not map to ParentBased(NeverSample)")
	}
}

func TestParseOTLPHeaders(t *testing.T) {
	got := parseOTLPHeaders("api-key=secret , env=prod,bad,=novalue,k=a=b")
	want := map[string]string{"api-key": "secret", "env": "prod", "k": "a=b"}
	if len(got) != len(want) {
		t.Fatalf("parseOTLPHeaders parsed %d entries, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %q = %q, want %q", k, got[k], v)
		}
	}
}

func assertHasAttrKey(t *testing.T, s tracetest.SpanStub, key string) {
	t.Helper()
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return
		}
	}
	t.Errorf("span %q missing attribute key %q", s.Name, key)
}
