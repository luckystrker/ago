package otelx

import (
	"context"
	"io"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/luckystrker/ago/middleware"
)

// DefaultOption selects the exporter InstallDefault wires into the SDK
// tracer provider. Without options spans export to stdout.
type DefaultOption interface {
	apply(*defaultConfig)
}

type defaultConfig struct {
	stdout    bool
	stdoutW   io.Writer
	collector string
}

type funcOption func(*defaultConfig)

func (f funcOption) apply(c *defaultConfig) { f(c) }

// WithStdout exports spans as JSON lines to os.Stdout.
func WithStdout() DefaultOption {
	return funcOption(func(c *defaultConfig) { c.stdout = true })
}

// WithStdoutWriter exports spans as JSON lines through w — for tests and for
// hosts that want the export in a file or buffer of their own.
func WithStdoutWriter(w io.Writer) DefaultOption {
	return funcOption(func(c *defaultConfig) { c.stdout = true; c.stdoutW = w })
}

// WithCollector exports spans over OTLP/HTTP to a collector. The endpoint is
// "host:port", "http://host:port" (plaintext) or "https://host" (TLS); ""
// means plaintext localhost:4318, the standard local collector socket.
//
// AGO-DEVIATION (D-064): only the OTLP/HTTP exporter is wired — Python leaves
// the whole provider to the caller; a gRPC exporter user builds their own
// provider and passes it to Middleware.
func WithCollector(endpoint string) DefaultOption {
	return funcOption(func(c *defaultConfig) { c.collector = endpoint })
}

// InstallDefault builds an SDK tracer provider exporting to stdout (the
// default) or to an OTLP collector, hands it to Middleware, registers it as
// the process-global provider and returns the middleware factory together
// with a shutdown function flushing the batcher and stopping the provider.
// Call it once, export with the returned factory, defer the shutdown.
//
// The global registration happens at most once per process with effect:
// otel.SetTracerProvider is consulted by every instrumentation on first use,
// so later InstallDefault calls still return a working factory, but the
// global delegate keeps serving the first provider it saw.
func InstallDefault(ctx context.Context, opts ...DefaultOption) (middleware.Factory, func(context.Context) error, error) {
	cfg := &defaultConfig{stdout: true}
	for _, o := range opts {
		if o != nil {
			o.apply(cfg)
		}
	}

	var exp sdktrace.SpanExporter
	var err error
	if cfg.collector != "" {
		exp, err = collectorExporter(ctx, cfg.collector)
	} else {
		w := cfg.stdoutW
		if w == nil {
			w = os.Stdout
		}
		exp, err = stdouttrace.New(stdouttrace.WithWriter(w))
	}
	if err != nil {
		return nil, nil, err
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(tp)
	return Middleware(tp), tp.Shutdown, nil
}

// collectorExporter builds the OTLP/HTTP exporter for the endpoint form
// WithCollector documents.
func collectorExporter(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error) {
	var opts []otlptracehttp.Option
	switch ep := endpoint; {
	case strings.HasPrefix(ep, "https://"):
		opts = append(opts, otlptracehttp.WithEndpoint(strings.TrimSuffix(strings.TrimPrefix(ep, "https://"), "/")))
	case strings.HasPrefix(ep, "http://"):
		opts = append(opts,
			otlptracehttp.WithEndpoint(strings.TrimSuffix(strings.TrimPrefix(ep, "http://"), "/")),
			otlptracehttp.WithInsecure())
	case ep == "":
		opts = append(opts, otlptracehttp.WithEndpoint("localhost:4318"), otlptracehttp.WithInsecure())
	default:
		opts = append(opts, otlptracehttp.WithEndpoint(ep), otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, opts...)
}
