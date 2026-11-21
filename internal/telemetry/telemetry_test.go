package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var ctx = context.Background()

// Collected is every data point by metric name, for assertions.
type Collected map[string][]Point

type Point struct {
	Value float64
	Attrs map[string]string
}

// Collect reads all metrics from a manual reader.
func Collect(t *testing.T, r *sdkmetric.ManualReader) Collected {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	out := Collected{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], Point{float64(p.Value), attrs(p.Attributes.ToSlice())})
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], Point{float64(p.Value), attrs(p.Attributes.ToSlice())})
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], Point{p.Value, attrs(p.Attributes.ToSlice())})
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], Point{float64(p.Count), attrs(p.Attributes.ToSlice())})
				}
			}
		}
	}
	return out
}

func attrs(kvs []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

// Get returns the value of the point whose attributes include want.
func (c Collected) Get(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	for _, p := range c[name] {
		ok := true
		for k, v := range want {
			if p.Attrs[k] != v {
				ok = false
			}
		}
		if ok {
			return p.Value
		}
	}
	t.Fatalf("no point for %s %v; have %v", name, want, c[name])
	return 0
}

func newTest(t *testing.T) (*Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	r := sdkmetric.NewManualReader()
	p := sdkmetric.NewMeterProvider(sdkmetric.WithReader(r))
	t.Cleanup(func() { p.Shutdown(ctx) })
	m, err := New(p.Meter("test"), Build{Version: "1.2.3", Revision: "abc123", GoVersion: "go1.26"})
	if err != nil {
		t.Fatal(err)
	}
	return m, r
}

func TestBuildInfo(t *testing.T) {
	_, r := newTest(t)
	c := Collect(t, r)
	if v := c.Get(t, "airspace.build_info", map[string]string{"version": "1.2.3", "revision": "abc123", "goversion": "go1.26"}); v != 1 {
		t.Errorf("build_info = %v, want 1", v)
	}
	if b := ReadBuild(""); b.Version != "dev" || b.GoVersion == "" {
		t.Errorf("ReadBuild: %+v", b)
	}
}

func TestUpstreamRequestsAndCredits(t *testing.T) {
	m, r := newTest(t)
	m.UpstreamRequest(ctx, "token", 200, nil, 80*time.Millisecond, 0, false, 0)
	m.UpstreamRequest(ctx, "states", 200, nil, 400*time.Millisecond, 3998, true, 2)
	m.UpstreamRequest(ctx, "states", 200, nil, 400*time.Millisecond, 3996, true, 2)
	m.UpstreamRequest(ctx, "states", 429, nil, 50*time.Millisecond, 0, false, 2)
	m.UpstreamRequest(ctx, "states", 401, nil, 50*time.Millisecond, 0, false, 2)
	m.UpstreamRequest(ctx, "states", 503, nil, 50*time.Millisecond, 0, false, 2)
	m.UpstreamRequest(ctx, "states", 0, errors.New("connection refused"), time.Second, 0, false, 2)
	c := Collect(t, r)

	req := "airspace.opensky.requests"
	for want, attrs := range map[float64]map[string]string{
		1: {"endpoint": "token", "outcome": "ok"},
		2: {"endpoint": "states", "outcome": "ok", "http.response.status_code": "200"},
	} {
		if v := c.Get(t, req, attrs); v != want {
			t.Errorf("%v = %v, want %v", attrs, v, want)
		}
	}
	for _, o := range []map[string]string{
		{"outcome": "rate_limited", "http.response.status_code": "429"},
		{"outcome": "unauthorized", "http.response.status_code": "401"},
		{"outcome": "error", "http.response.status_code": "503"},
		{"outcome": "error", "http.response.status_code": "0"},
	} {
		if v := c.Get(t, req, o); v != 1 {
			t.Errorf("%v = %v, want 1", o, v)
		}
	}
	// The balance is the one from the latest response that carried it.
	if v := c.Get(t, "airspace.opensky.credits.remaining", nil); v != 3996 {
		t.Errorf("remaining = %v, want 3996", v)
	}
	// Only successful queries spend credits.
	if v := c.Get(t, "airspace.opensky.credits.spent", nil); v != 4 {
		t.Errorf("spent = %v, want 4", v)
	}
	if v := c.Get(t, "airspace.opensky.request.duration", map[string]string{"endpoint": "states"}); v != 6 {
		t.Errorf("duration samples = %v, want 6", v)
	}
}

func TestPollsHTTPRecordingAndGauges(t *testing.T) {
	m, r := newTest(t)
	m.Poll(ctx, "ok", false, 560)
	m.Poll(ctx, "ok", true, 12400)
	m.Poll(ctx, "rate_limited", true, 0)
	m.HTTPRequest(ctx, "/api/states", "GET", 200, 3*time.Millisecond, false)
	m.HTTPRequest(ctx, "/api/states", "GET", 304, time.Millisecond, false)
	m.HTTPRequest(ctx, "/api/stream", "GET", 200, time.Hour, true)
	m.SnapshotStored(ctx, 6000)
	m.SnapshotStored(ctx, 7000)
	m.StorageFailure(ctx, "dropped")
	m.SessionsPruned(ctx, 3)
	m.StateChange(ctx, "camera")
	if err := m.Observe(func() Gauges {
		return Gauges{ClientsConnected: 3, ClientsActive: 2, State: "polling", IntervalSeconds: 8, PollCost: 4,
			Recording: true, StorageHealthy: true, DBBytes: 4096, SnapshotAgeKnown: true, SnapshotAge: 2.5}
	}); err != nil {
		t.Fatal(err)
	}
	c := Collect(t, r)
	for name, want := range map[string]float64{
		"airspace.snapshot.aircraft": 12400, "airspace.recording.snapshots": 2, "airspace.recording.bytes": 13000,
		"airspace.recording.sessions.pruned": 3, "airspace.clients.connected": 3, "airspace.clients.active": 2,
		"airspace.poll.interval": 8, "airspace.poll.cost": 4, "airspace.recording.active": 1,
		"airspace.recording.healthy": 1, "airspace.recording.database.size": 4096, "airspace.snapshot.age": 2.5,
	} {
		if v := c.Get(t, name, nil); v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	checks := []struct {
		name  string
		attrs map[string]string
		want  float64
	}{
		{"airspace.polls", map[string]string{"outcome": "ok", "coverage": "viewport"}, 1},
		{"airspace.polls", map[string]string{"outcome": "ok", "coverage": "global"}, 1},
		{"airspace.polls", map[string]string{"outcome": "rate_limited"}, 1},
		{"airspace.http.server.requests", map[string]string{"http.route": "/api/states", "http.response.status_code": "304"}, 1},
		{"airspace.http.server.requests", map[string]string{"http.route": "/api/stream"}, 1},
		{"airspace.http.server.request.duration", map[string]string{"http.route": "/api/states"}, 2},
		{"airspace.recording.failures", map[string]string{"reason": "dropped"}, 1},
		{"airspace.state.changes", map[string]string{"type": "camera"}, 1},
		{"airspace.poller.state", map[string]string{"state": "polling"}, 1},
		{"airspace.poller.state", map[string]string{"state": "stopped"}, 0},
	}
	for _, k := range checks {
		if v := c.Get(t, k.name, k.attrs); v != k.want {
			t.Errorf("%s %v = %v, want %v", k.name, k.attrs, v, k.want)
		}
	}
	// The stream's hour-long lifetime is not in the duration histogram.
	for _, p := range c["airspace.http.server.request.duration"] {
		if p.Attrs["http.route"] == "/api/stream" {
			t.Error("the event stream must not be in the duration histogram")
		}
	}
}

func TestNilMetricsIsANoOp(t *testing.T) {
	var m *Metrics
	m.UpstreamRequest(ctx, "states", 200, nil, time.Second, 1, true, 1)
	m.Poll(ctx, "ok", false, 1)
	m.HTTPRequest(ctx, "/", "GET", 200, time.Second, false)
	m.SnapshotStored(ctx, 1)
	m.StorageFailure(ctx, "error")
	m.SessionsPruned(ctx, 1)
	m.StateChange(ctx, "camera")
	if err := m.Observe(func() Gauges { return Gauges{} }); err != nil {
		t.Fatal(err)
	}
}

func TestEnabledOnlyWithAnEndpoint(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if Enabled(env(nil)) {
		t.Error("metrics export must be off by default")
	}
	if !Enabled(env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"})) ||
		!Enabled(env(map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://collector:4318/v1/metrics"})) {
		t.Error("an endpoint should turn export on")
	}
	if Enabled(env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318", "OTEL_SDK_DISABLED": "true"})) ||
		Enabled(env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318", "OTEL_METRICS_EXPORTER": "none"})) {
		t.Error("the standard off switches must be honoured")
	}
}
