package obs

import (
	"context"
	"fmt"
	"strings"

	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// newExporter builds the span exporter for cfg.Exporter. The caller has already rejected
// ExporterNone (no exporter) and any unknown value, so this handles only the three real
// sinks. The application names no vendor: every OTLP backend is reached through
// ExporterOTLP + endpoint, and ExporterGCP is the one convenience path (Cloud Trace via
// Application Default Credentials).
func newExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch cfg.Exporter {
	case ExporterConsole:
		exp, err := stdouttrace.New()
		if err != nil {
			return nil, fmt.Errorf("obs: build console exporter: %w", err)
		}
		return exp, nil
	case ExporterOTLP:
		if strings.TrimSpace(cfg.OTLPEndpoint) == "" {
			// config validates this, but guard so a direct caller fails loudly rather than
			// silently exporting nowhere.
			return nil, fmt.Errorf("obs: exporter %q requires an OTLP endpoint", ExporterOTLP)
		}
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint)}
		if headers := parseOTLPHeaders(cfg.OTLPHeaders); len(headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(headers))
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("obs: build otlp exporter: %w", err)
		}
		return exp, nil
	case ExporterGCP:
		// No project id: the Cloud Trace exporter detects it from Application Default
		// Credentials / the metadata server, matching how the rest of the GCP path authenticates.
		exp, err := texporter.New()
		if err != nil {
			return nil, fmt.Errorf("obs: build gcp (cloud trace) exporter: %w", err)
		}
		return exp, nil
	default:
		return nil, fmt.Errorf("obs: unknown OTEL_TRACES_EXPORTER %q", cfg.Exporter)
	}
}

// parseOTLPHeaders parses the standard OTEL_EXPORTER_OTLP_HEADERS form — comma-separated
// key=value pairs (e.g. "api-key=secret,env=prod") — into a header map. Blank entries and
// entries without a key are skipped; only the first '=' splits, so a value may contain '='.
func parseOTLPHeaders(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}
