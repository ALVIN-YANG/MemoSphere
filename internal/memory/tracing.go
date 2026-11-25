package memory

import (
	"context"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

func (s *Service) ConfigureTracing() func() {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		return func() {}
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.User != nil || u.Hostname() == "" || u.Path == "" || u.RawQuery != "" || u.Fragment != "" || !(u.Scheme == "https" || u.Scheme == "http" && loopback(u.Hostname())) {
		log.Print("memory tracing disabled: invalid endpoint")
		return func() {}
	}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	exporter, e := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithHTTPClient(client))
	if e != nil {
		log.Print("memory tracing disabled: exporter unavailable")
		return func() {}
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "memosphere"))), sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(256), sdktrace.WithMaxExportBatchSize(32), sdktrace.WithBatchTimeout(time.Second), sdktrace.WithExportTimeout(3*time.Second)))
	s.tracer = provider.Tracer("memosphere.memory")
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = provider.Shutdown(ctx)
	}
}

type parentTrace struct{}

func (s *Service) tracedRecall(ctx context.Context, key string, d Row) Row {
	if s.tracer == nil {
		return s.recall(ctx, key, d)
	}
	parent, _ := ctx.Value(parentTrace{}).(string)
	ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": parent})
	ctx, span := s.tracer.Start(ctx, "memosphere.recall")
	defer span.End()
	defer func() {
		if p := recover(); p != nil {
			span.SetStatus(codes.Error, "")
			panic(p)
		}
	}()
	r := s.recall(ctx, key, d)
	if !span.IsRecording() {
		return r
	}
	rows := r["memories"].([]Row)
	info, _ := r["retrieval"].(Row)
	degraded, _ := info["degraded"].([]string)
	span.SetAttributes(attribute.Int("memory.candidate_count", len(rows)), attribute.String("memory.purpose", str(r["purpose"])), attribute.Bool("memory.degraded", len(degraded) > 0))
	for i, m := range rows {
		attrs := []attribute.KeyValue{attribute.Int("memory.rank", i+1), attribute.Int64("memory.revision", num(m["revision"]))}
		ranking, _ := m["ranking"].(Row)
		ranks, _ := ranking["ranks"].(map[string]int)
		for _, k := range []string{"semantic", "bm25", "entity"} {
			if n := ranks[k]; n > 0 {
				attrs = append(attrs, attribute.Int("memory.rank."+k, n))
			}
		}
		span.AddEvent("memory.candidate", trace.WithAttributes(attrs...))
	}
	return r
}
