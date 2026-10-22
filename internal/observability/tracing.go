package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	TracerName = "github.com/ai-process/ktms"

	// GenAI Semantic Conventions
	AttrGenAIProviderName  = attribute.Key("gen_ai.provider.name")
	AttrGenAIRequestModel  = attribute.Key("gen_ai.request.model")
	AttrGenAIResponseModel = attribute.Key("gen_ai.response.model")
	AttrGenAIUsageInput    = attribute.Key("gen_ai.usage.input_tokens")
	AttrGenAIUsageOutput   = attribute.Key("gen_ai.usage.output_tokens")

	// LLM Proxy routing attributes
	AttrKTMSRule      = attribute.Key("ktms.rule")
	AttrKTMSEffort    = attribute.Key("ktms.effort")
	AttrKTMSClientKey = attribute.Key("ktms.client_key")
)

// Tracer returns the named tracer for KTMS.
func Tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer(TracerName)
}

// InitTracing initializes the OpenTelemetry tracer provider with OTLP gRPC export.
func InitTracing(ctx context.Context, serviceName, endpoint string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel resource: %w", err)
	}

	var opts []sdktrace.TracerProviderOption
	opts = append(opts, sdktrace.WithResource(res))

	if endpoint != "" {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
		)
		if err != nil {
			return nil, fmt.Errorf("create otlp trace exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}
