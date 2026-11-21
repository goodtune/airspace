// Package telemetry emits OpenTelemetry metrics. Every method is safe on a
// nil *Metrics, so the rest of the code never has to check whether metrics
// are switched on.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Build identifies the running binary.
type Build struct {
	Version   string // release version, "dev" when not set at build time
	Revision  string // git commit
	GoVersion string
}

// ReadBuild fills in what the Go toolchain recorded about the binary.
func ReadBuild(version string) Build {
	b := Build{Version: version, GoVersion: runtime.Version(), Revision: "unknown"}
	if b.Version == "" {
		b.Version = "dev"
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		dirty := false
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Revision = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if dirty && b.Revision != "unknown" {
			b.Revision += "-dirty"
		}
	}
	return b
}

// Gauges is the state sampled at each collection.
type Gauges struct {
	ClientsConnected int
	ClientsActive    int
	State            string // polling, paused, or stopped
	IntervalSeconds  float64
	PollCost         int
	Recording        bool
	StorageHealthy   bool
	DBBytes          int64
	SnapshotAgeKnown bool
	SnapshotAge      float64 // seconds since the last good snapshot
}

// Metrics holds the instruments.
type Metrics struct {
	meter metric.Meter

	upstreamRequests metric.Int64Counter
	upstreamDuration metric.Float64Histogram
	creditsRemaining metric.Int64Gauge
	creditsSpent     metric.Int64Counter
	polls            metric.Int64Counter
	aircraft         metric.Int64Gauge
	httpRequests     metric.Int64Counter
	httpDuration     metric.Float64Histogram
	snapshotsStored  metric.Int64Counter
	snapshotBytes    metric.Int64Counter
	storageFailures  metric.Int64Counter
	sessionsPruned   metric.Int64Counter
	stateChanges     metric.Int64Counter
}

var states = []string{"polling", "paused", "stopped"}

// New creates the instruments on a meter and registers build_info.
func New(meter metric.Meter, build Build) (*Metrics, error) {
	m := &Metrics{meter: meter}
	var errs []error
	counter := func(name, desc, unit string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
		errs = append(errs, err)
		return c
	}
	hist := func(name, desc string, bounds ...float64) metric.Float64Histogram {
		h, err := meter.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(bounds...))
		errs = append(errs, err)
		return h
	}
	gauge := func(name, desc, unit string) metric.Int64Gauge {
		g, err := meter.Int64Gauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
		errs = append(errs, err)
		return g
	}

	m.upstreamRequests = counter("airspace.opensky.requests",
		"Requests made to OpenSky, token requests included, by endpoint and outcome.", "{request}")
	m.upstreamDuration = hist("airspace.opensky.request.duration",
		"Time taken by each request to OpenSky.", 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 45)
	m.creditsRemaining = gauge("airspace.opensky.credits.remaining",
		"OpenSky credits left, from X-Rate-Limit-Remaining on the most recent response.", "{credit}")
	m.creditsSpent = counter("airspace.opensky.credits.spent",
		"OpenSky credits spent, worked out from the area of each successful query.", "{credit}")
	m.polls = counter("airspace.polls", "Poll cycles, by outcome and coverage.", "{poll}")
	m.aircraft = gauge("airspace.snapshot.aircraft", "Aircraft in the most recent snapshot.", "{aircraft}")
	m.httpRequests = counter("airspace.http.server.requests",
		"HTTP requests served, by route, method and status.", "{request}")
	m.httpDuration = hist("airspace.http.server.request.duration",
		"Time taken to serve each HTTP request. The long-lived event stream is not included.",
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)
	m.snapshotsStored = counter("airspace.recording.snapshots", "Snapshots written to the database.", "{snapshot}")
	m.snapshotBytes = counter("airspace.recording.bytes", "Compressed snapshot bytes written to the database.", "By")
	m.storageFailures = counter("airspace.recording.failures",
		"Recording writes that failed or were dropped, by reason.", "{write}")
	m.sessionsPruned = counter("airspace.recording.sessions.pruned", "Sessions deleted by the retention rules.", "{session}")
	m.stateChanges = counter("airspace.state.changes",
		"Changes to shared state, by type: camera, interval, selected, recording, coverage, scope, and so on.", "{change}")

	// build_info follows the Prometheus convention: always 1, with the
	// build's identity in the attributes.
	info, err := meter.Int64ObservableGauge("airspace.build_info",
		metric.WithDescription("Always 1. The attributes identify the running build."))
	errs = append(errs, err)
	if err == nil {
		attrs := metric.WithAttributes(attribute.String("version", build.Version),
			attribute.String("revision", build.Revision), attribute.String("goversion", build.GoVersion))
		_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
			o.ObserveInt64(info, 1, attrs)
			return nil
		}, info)
		errs = append(errs, err)
	}
	return m, errors.Join(errs...)
}

// Observe registers the gauges that are sampled at each collection.
func (m *Metrics) Observe(read func() Gauges) error {
	if m == nil {
		return nil
	}
	var errs []error
	og := func(name, desc, unit string) metric.Int64ObservableGauge {
		g, err := m.meter.Int64ObservableGauge(name, metric.WithDescription(desc), metric.WithUnit(unit))
		errs = append(errs, err)
		return g
	}
	connected := og("airspace.clients.connected", "Browsers holding the event stream.", "{client}")
	active := og("airspace.clients.active", "Browsers counted as watching live. Polling runs only while this is above zero.", "{client}")
	state := og("airspace.poller.state", "1 for the poller's current state, 0 for the others.", "")
	cost := og("airspace.poll.cost", "Credit cost of one poll at the current scope.", "{credit}")
	recording := og("airspace.recording.active", "1 while a session is being recorded.", "")
	healthy := og("airspace.recording.healthy", "1 while storage works, 0 when the app has fallen back to live-only.", "")
	dbBytes := og("airspace.recording.database.size", "Size of the database.", "By")
	interval, err := m.meter.Float64ObservableGauge("airspace.poll.interval",
		metric.WithDescription("Effective poll interval."), metric.WithUnit("s"))
	errs = append(errs, err)
	age, err := m.meter.Float64ObservableGauge("airspace.snapshot.age",
		metric.WithDescription("Time since the last good snapshot. Not reported before the first one."), metric.WithUnit("s"))
	errs = append(errs, err)
	if err := errors.Join(errs...); err != nil {
		return err
	}
	b := func(v bool) int64 {
		if v {
			return 1
		}
		return 0
	}
	_, err = m.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		g := read()
		o.ObserveInt64(connected, int64(g.ClientsConnected))
		o.ObserveInt64(active, int64(g.ClientsActive))
		for _, s := range states {
			o.ObserveInt64(state, b(s == g.State), metric.WithAttributes(attribute.String("state", s)))
		}
		o.ObserveInt64(cost, int64(g.PollCost))
		o.ObserveInt64(recording, b(g.Recording))
		o.ObserveInt64(healthy, b(g.StorageHealthy))
		o.ObserveInt64(dbBytes, g.DBBytes)
		o.ObserveFloat64(interval, g.IntervalSeconds)
		if g.SnapshotAgeKnown {
			o.ObserveFloat64(age, g.SnapshotAge)
		}
		return nil
	}, connected, active, state, cost, recording, healthy, dbBytes, interval, age)
	return err
}

// UpstreamRequest counts one request to OpenSky. When the response carried
// the credit balance, it is recorded in the same call, so the counter and
// the balance are always sampled together.
func (m *Metrics) UpstreamRequest(ctx context.Context, endpoint string, status int, err error, d time.Duration, remaining int, hasRemaining bool, cost int) {
	if m == nil {
		return
	}
	outcome := "ok"
	switch {
	case err != nil:
		outcome = "error" // no response at all
	case status == 429:
		outcome = "rate_limited"
	case status == 401:
		outcome = "unauthorized"
	case status < 200 || status > 299:
		outcome = "error"
	}
	attrs := metric.WithAttributes(attribute.String("endpoint", endpoint), attribute.String("outcome", outcome),
		attribute.Int("http.response.status_code", status))
	m.upstreamRequests.Add(ctx, 1, attrs)
	m.upstreamDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("endpoint", endpoint)))
	if hasRemaining {
		m.creditsRemaining.Record(ctx, int64(remaining))
	}
	if outcome == "ok" && cost > 0 {
		m.creditsSpent.Add(ctx, int64(cost))
	}
}

// Poll counts one poll cycle. outcome is ok, error, or rate_limited.
func (m *Metrics) Poll(ctx context.Context, outcome string, global bool, aircraft int) {
	if m == nil {
		return
	}
	coverage := "viewport"
	if global {
		coverage = "global"
	}
	m.polls.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome), attribute.String("coverage", coverage)))
	if outcome == "ok" {
		m.aircraft.Record(ctx, int64(aircraft))
	}
}

// HTTPRequest counts one request served. stream marks the long-lived event
// stream, whose duration is a connection lifetime and is left out.
func (m *Metrics) HTTPRequest(ctx context.Context, route, method string, status int, d time.Duration, stream bool) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("http.route", route), attribute.String("http.request.method", method),
		attribute.Int("http.response.status_code", status))
	m.httpRequests.Add(ctx, 1, attrs)
	if !stream {
		m.httpDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("http.route", route)))
	}
}

// SnapshotStored counts a snapshot written to the database.
func (m *Metrics) SnapshotStored(ctx context.Context, bytes int) {
	if m == nil {
		return
	}
	m.snapshotsStored.Add(ctx, 1)
	m.snapshotBytes.Add(ctx, int64(bytes))
}

// StorageFailure counts a write that failed ("error") or was dropped
// because the queue was full ("dropped").
func (m *Metrics) StorageFailure(ctx context.Context, reason string) {
	if m == nil {
		return
	}
	m.storageFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// SessionsPruned counts sessions deleted by retention.
func (m *Metrics) SessionsPruned(ctx context.Context, n int) {
	if m == nil {
		return
	}
	m.sessionsPruned.Add(ctx, int64(n))
}

// StateChange counts a change to shared state.
func (m *Metrics) StateChange(ctx context.Context, kind string) {
	if m == nil {
		return
	}
	m.stateChanges.Add(ctx, 1, metric.WithAttributes(attribute.String("type", kind)))
}

// Enabled reports whether the environment asks for metrics to be exported.
// Export is off unless an OTLP endpoint is set.
func Enabled(getenv func(string) string) bool {
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") || strings.EqualFold(getenv("OTEL_METRICS_EXPORTER"), "none") {
		return false
	}
	return getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

// Setup starts exporting metrics over OTLP when the standard OTEL_*
// environment variables point at a collector. It returns nil Metrics, which
// is a valid no-op, when they do not. Call shutdown before exit to send the
// last batch.
func Setup(ctx context.Context, build Build, log *slog.Logger) (m *Metrics, shutdown func(context.Context) error, err error) {
	shutdown = func(context.Context) error { return nil }
	if !Enabled(os.Getenv) {
		return nil, shutdown, nil
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	var exp sdkmetric.Exporter
	if protocol == "grpc" {
		exp, err = otlpmetricgrpc.New(ctx)
	} else {
		protocol = "http/protobuf"
		exp, err = otlpmetrichttp.New(ctx)
	}
	if err != nil {
		return nil, shutdown, err
	}
	// service.name defaults to airspace; OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES override and add to it.
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "airspace"), attribute.String("service.version", build.Version)),
		resource.WithFromEnv(), resource.WithTelemetrySDK(), resource.WithHost())
	if err != nil && res == nil {
		return nil, shutdown, err
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	m, err = New(provider.Meter("github.com/goodtune/airspace"), build)
	if err != nil {
		provider.Shutdown(ctx)
		return nil, shutdown, err
	}
	// Report export problems through the app's own log, in its format.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Warn("metrics export", "error", err) }))
	log.Info("exporting metrics", "protocol", protocol, "version", build.Version, "revision", build.Revision)
	return m, provider.Shutdown, nil
}
