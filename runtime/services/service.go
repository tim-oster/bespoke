package services

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/go-chi/httplog/v3"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tim-oster/bespoke/runtime/slogctx"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.19.0"
)

type otelConfig struct {
	name           string
	metricProvider *metric.MeterProvider
}

func makeOTelConfig(name string) (otelConfig, error) {
	appResource, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			"",
			semconv.ServiceNameKey.String(name),
		),
	)
	if err != nil {
		return otelConfig{}, fmt.Errorf("failed to create otel resource: %w", err)
	}

	metricExporter, err := prometheus.New()
	if err != nil {
		return otelConfig{}, fmt.Errorf("failed to create otel prometheus meter provider: %w", err)
	}
	meterProvider := metric.NewMeterProvider(metric.WithReader(metricExporter), metric.WithResource(appResource))

	return otelConfig{
		name:           name,
		metricProvider: meterProvider,
	}, nil
}

func Run(name string, fn func(b *Bootstrapper) error) {
	logFormat := httplog.SchemaGCP.Concise(false)
	logger := newLogger(getLogLevel(), logFormat.ReplaceAttr).With(slog.String("service", name))
	slog.SetDefault(logger)

	slog.Info("starting service...")

	otelConfig, err := makeOTelConfig(name)
	if err != nil {
		slogFatal("failed to create otel config", err)
	}

	b := &Bootstrapper{
		logger:     logger,
		otelConfig: otelConfig,
	}

	err = fn(b)
	if err != nil {
		slogFatal("failed to start service", err)
	}

	b.addDebugServer()

	for _, job := range b.startupJobs {
		ctx := slogctx.With(context.Background(), slog.String("job", job.name))
		err := job.fn(ctx)
		if err != nil {
			slogFatalContext(ctx, "failed to run startup job", err)
		}
	}

	backgroundCtx, backgroundCancel := context.WithCancel(context.Background())
	defer backgroundCancel()

	var wg sync.WaitGroup

	for _, port := range slices.Sorted(maps.Keys(b.servers)) {
		wg.Add(1)
		go func(server *http.Server) {
			defer wg.Done()

			slog.Info("starting server", "addr", server.Addr)
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slogFatal("failed to start server", err)
			}
		}(b.servers[port])
	}

	for name, j := range b.jobs {
		wg.Add(1)
		go func(name string, job job) {
			defer wg.Done()
			runJob(backgroundCtx, name, job.interval, job.fn)
		}(name, j)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("received shutdown signal")

	slog.Info("stopping jobs")
	backgroundCancel()

	slog.Info("shutting down server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for port, server := range b.servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Warn("graceful shutdown failed - shutting down forcefully", "error", err, "port", port)
			if err := server.Close(); err != nil {
				slog.Warn("forceful shutdown failed", "error", err, "port", port)
			}
		}
	}

	slog.Info("waiting for jobs to finish")
	wg.Wait()

	for _, fn := range b.deferFns {
		fn()
	}

	slog.Info("bye!")
}

func slogFatal(msg string, err error) {
	slogFatalContext(context.Background(), msg, err)
}

func slogFatalContext(ctx context.Context, msg string, err error) {
	if err != nil {
		slog.ErrorContext(ctx, msg, "error", err)
	} else {
		slog.ErrorContext(ctx, msg)
	}
	os.Exit(1)
}

func runJob(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	ctx = slogctx.With(ctx, slog.String("job", name))

	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "failed to run job", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type Bootstrapper struct {
	logger      *slog.Logger
	otelConfig  otelConfig
	servers     map[int]*http.Server
	jobs        map[string]job
	startupJobs []job
	deferFns    []func()
}

type job struct {
	name     string
	interval time.Duration
	fn       func(context.Context) error
}

func (b *Bootstrapper) Logger() *slog.Logger {
	return b.logger
}

func (b *Bootstrapper) MeterProvider() *metric.MeterProvider {
	return b.otelConfig.metricProvider
}

func NewRouter(logger *slog.Logger, corsOptions cors.Options) *chi.Mux {
	isDebugHeaderSet := func(r *http.Request) bool {
		return r.Header.Get("Debug") == "body"
	}

	r := chi.NewRouter()
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := slogctx.PrepareContext(r.Context())
			h.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	r.Use(httplog.RequestLogger(logger, &httplog.Options{
		Level:             slog.LevelInfo,
		Schema:            httplog.SchemaGCP.Concise(false),
		RecoverPanics:     true,
		LogRequestHeaders: []string{"Origin"},
		LogRequestBody:    isDebugHeaderSet,
		LogResponseBody:   isDebugHeaderSet,
	}))
	r.Use(middleware.RequestSize(100 << 10)) // 100 KB
	r.Use(cors.New(corsOptions).Handler)
	return r
}

func (b *Bootstrapper) AddServer(srv *http.Server) {
	_, port, err := net.SplitHostPort(srv.Addr)
	if err != nil {
		slogFatal("Failed to split host and port", err)
	}
	portInt, err := strconv.Atoi(port)
	if err != nil {
		slogFatal("Failed to convert port to int", err)
	}

	if b.servers == nil {
		b.servers = make(map[int]*http.Server)
	}
	if _, ok := b.servers[portInt]; ok {
		slogFatal("server already added", errors.New("server already added"))
	}
	b.servers[portInt] = srv
}

func (b *Bootstrapper) addDebugServer() {
	debugPort := cmp.Or(os.Getenv("DEBUG_PORT"), "6060")
	debugMux := http.NewServeMux()

	debugMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	debugMux.Handle("GET /metrics", promhttp.Handler())

	debugMux.HandleFunc("GET /debug/pprof/", pprof.Index)
	debugMux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	debugMux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	debugMux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	debugMux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	b.AddServer(&http.Server{Addr: ":" + debugPort, Handler: debugMux})
}

func (b *Bootstrapper) AddJob(name string, interval time.Duration, fn func(context.Context) error) {
	if b.jobs == nil {
		b.jobs = make(map[string]job)
	}
	if _, ok := b.jobs[name]; ok {
		slogFatal("job already added", errors.New("job already added"))
	}
	b.jobs[name] = job{
		name:     name,
		interval: interval,
		fn:       fn,
	}
}

func (b *Bootstrapper) AddJobAndOnStartup(name string, interval time.Duration, fn func(context.Context) error) {
	b.AddJob(name, interval, fn)
	b.startupJobs = append(b.startupJobs, b.jobs[name])
}

func (b *Bootstrapper) Defer(fn func()) {
	b.deferFns = append(b.deferFns, fn)
}
