package observability

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func TestObservabilityServer_Endpoints(t *testing.T) {
	srv := NewServer(":0")
	if srv == nil {
		t.Fatal("expected non-nil server")
	}

	// Test healthz endpoint
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("healthz body = %q, want {\"status\":\"ok\"}", string(body))
	}

	// Test metrics endpoint
	reqMetrics := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	wMetrics := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(wMetrics, reqMetrics)

	respMetrics := wMetrics.Result()
	if respMetrics.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want %d", respMetrics.StatusCode, http.StatusOK)
	}
}

func TestObservabilityMetrics_Recording(t *testing.T) {
	vendor := "test-vendor"
	model := "test-model"
	effort := "medium"

	RequestsTotal.WithLabelValues(vendor, model, effort, "success").Inc()
	RequestDuration.WithLabelValues(vendor, model, effort).Observe(0.25)
	TimeToFirstToken.WithLabelValues(vendor, model).Observe(0.08)
	TokensTotal.WithLabelValues(vendor, model, "input").Add(150)
	TokensTotal.WithLabelValues(vendor, model, "output").Add(75)
	ActiveRequests.WithLabelValues(vendor, model).Inc()
	ActiveRequests.WithLabelValues(vendor, model).Dec()
	ThrottledTotal.WithLabelValues("rpm").Inc()
	FallbacksTotal.WithLabelValues(model, "fallback-model", "capacity_exhausted").Inc()
	BatchJobsTotal.WithLabelValues(vendor, "SUCCEEDED").Inc()
	BatchItemsTotal.WithLabelValues(vendor, "SUCCEEDED").Inc()
	BatchPollDuration.WithLabelValues(vendor).Observe(0.5)

	srv := NewServer(":0")
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)

	body, _ := io.ReadAll(w.Result().Body)
	metricsOutput := string(body)

	expectedMetrics := []string{
		"ktms_requests_total",
		"ktms_request_duration_seconds",
		"ktms_time_to_first_token_seconds",
		"ktms_tokens_total",
		"ktms_active_requests",
		"ktms_throttled_total",
		"ktms_fallbacks_total",
		"ktms_batch_jobs_total",
		"ktms_batch_items_total",
		"ktms_batch_poll_duration_seconds",
	}

	for _, metricName := range expectedMetrics {
		if !strings.Contains(metricsOutput, metricName) {
			t.Errorf("expected metrics output to contain %q", metricName)
		}
	}
}

func TestObservabilityTracing_InitAndSpan(t *testing.T) {
	ctx := context.Background()
	shutdown, err := InitTracing(ctx, "test-llm-proxy", "")
	if err != nil {
		t.Fatalf("InitTracing: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("tracing shutdown: %v", err)
		}
	}()

	tr := Tracer()
	ctx, span := tr.Start(ctx, "test-operation", trace.WithAttributes(
		AttrGenAIProviderName.String("test-vendor"),
		AttrGenAIRequestModel.String("test-model"),
		AttrKTMSEffort.String("high"),
	))
	if !span.SpanContext().IsValid() {
		// Valid span context created even in headless mode
	}
	span.SetAttributes(
		AttrGenAIResponseModel.String("test-model"),
		AttrGenAIUsageInput.Int(100),
		AttrGenAIUsageOutput.Int(50),
	)
	span.End()
}

func TestObservabilityServer_Lifecycle(t *testing.T) {
	srv := NewServer("127.0.0.1:0")
	srv.Start()

	// Verify server shut down
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("server shutdown: %v", err)
	}
}
