package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/goodtune/airspace/internal/telemetry"
)

type attrSet struct{ kvs []attribute.KeyValue }

func (a attrSet) value(k string) string {
	for _, kv := range a.kvs {
		if string(kv.Key) == k {
			return kv.Value.Emit()
		}
	}
	return ""
}

// point finds a metric value by name and a set of attributes it must carry.
func point(t *testing.T, r *sdkmetric.ManualReader, name string, want map[string]string) (float64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	match := func(set attrSet) bool {
		for k, v := range want {
			if set.value(k) != v {
				return false
			}
		}
		return true
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					if match(attrSet{p.Attributes.ToSlice()}) {
						return float64(p.Value), true
					}
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					if match(attrSet{p.Attributes.ToSlice()}) {
						return float64(p.Value), true
					}
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					if match(attrSet{p.Attributes.ToSlice()}) {
						return p.Value, true
					}
				}
			}
		}
	}
	return 0, false
}

func TestMetricsFromARunningApp(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer provider.Shutdown(context.Background())
	m, err := telemetry.New(provider.Meter("test"), telemetry.Build{Version: "9.9.9", Revision: "cafe", GoVersion: "go"})
	if err != nil {
		t.Fatal(err)
	}
	r := startWith(t, nil, func(a *App) {
		if err := a.Instrument(m); err != nil {
			t.Fatal(err)
		}
	})
	get := func(name string, want map[string]string) float64 {
		t.Helper()
		v, ok := point(t, reader, name, want)
		if !ok {
			t.Fatalf("metric %s %v was not reported", name, want)
		}
		return v
	}

	// Before anyone connects: build_info is there, and nothing has gone upstream.
	if get("airspace.build_info", map[string]string{"version": "9.9.9", "revision": "cafe"}) != 1 {
		t.Error("build_info should be 1")
	}
	if _, ok := point(t, reader, "airspace.opensky.requests", nil); ok {
		t.Error("an upstream request was counted with nobody watching")
	}
	if get("airspace.poller.state", map[string]string{"state": "stopped"}) != 1 || get("airspace.clients.active", nil) != 0 {
		t.Error("idle app should report stopped with no active clients")
	}

	c := r.connect()
	r.act(c, "select", map[string]any{"icao24": "a00001"})
	r.getJSON("/api/status", nil)
	r.waitFor("four polls", func() bool { return r.fake.StateRequests() >= 4 })
	c.close()
	r.waitFor("polling to stop", func() bool { return r.app.Hub.State().Status.Active == 0 })
	time.Sleep(100 * time.Millisecond) // an in-flight poll may finish
	r.app.Store.Flush()

	// Every upstream request is counted, token requests included.
	if got := get("airspace.opensky.requests", map[string]string{"endpoint": "token", "outcome": "ok"}); got != float64(r.fake.TokenRequests()) {
		t.Errorf("token requests counted %v, fake saw %d", got, r.fake.TokenRequests())
	}
	states := get("airspace.opensky.requests", map[string]string{"endpoint": "states", "outcome": "ok", "http.response.status_code": "200"})
	if states != float64(r.fake.StateRequests()) {
		t.Errorf("state requests counted %v, fake saw %d", states, r.fake.StateRequests())
	}
	// The balance is the header's, and what was spent accounts for the rest.
	if got := get("airspace.opensky.credits.remaining", nil); got != float64(r.fake.Remaining()) {
		t.Errorf("credits remaining %v, fake has %d", got, r.fake.Remaining())
	}
	if got := get("airspace.opensky.credits.spent", nil); got != float64(4000-r.fake.Remaining()) {
		t.Errorf("credits spent %v, want %d", got, 4000-r.fake.Remaining())
	}
	if got := get("airspace.polls", map[string]string{"outcome": "ok", "coverage": "viewport"}); got != states {
		t.Errorf("polls %v, want %v", got, states)
	}
	// The gauge matches the snapshot, which may be empty for this view.
	if got, want := get("airspace.snapshot.aircraft", nil), float64(r.app.Hub.State().Snapshot.Count); got != want {
		t.Errorf("aircraft gauge %v, snapshot holds %v", got, want)
	}
	if got := get("airspace.recording.snapshots", nil); got != states {
		t.Errorf("snapshots stored %v, want %v", got, states)
	}
	if get("airspace.recording.bytes", nil) <= 0 || get("airspace.recording.healthy", nil) != 1 {
		t.Error("recording metrics missing")
	}
	// HTTP requests are counted by route pattern, not by raw path.
	for route, method := range map[string]string{"/api/status": "GET", "/api/stream": "GET", "/api/action/{name}": "POST"} {
		if get("airspace.http.server.requests", map[string]string{"http.route": route, "http.request.method": method}) < 1 {
			t.Errorf("no request counted for %s", route)
		}
	}
	if get("airspace.state.changes", map[string]string{"type": "selected"}) != 1 {
		t.Error("selection change not counted")
	}
	if get("airspace.poller.state", map[string]string{"state": "stopped"}) != 1 {
		t.Error("poller should be stopped again")
	}

	// A failing upstream is counted as such.
	r.fake.SetFailing(true)
	c2 := r.connect()
	r.waitFor("an upstream error", func() bool { return c2.get().Status.Error != "" })
	if get("airspace.opensky.requests", map[string]string{"endpoint": "states", "outcome": "error", "http.response.status_code": "503"}) < 1 {
		t.Error("failed request not counted")
	}
	if get("airspace.polls", map[string]string{"outcome": "error"}) < 1 {
		t.Error("failed poll not counted")
	}
	// A static file counts under one route label.
	resp, err := http.Get(r.url + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if get("airspace.http.server.requests", map[string]string{"http.route": "static"}) < 1 {
		t.Error("static request not counted")
	}
}
