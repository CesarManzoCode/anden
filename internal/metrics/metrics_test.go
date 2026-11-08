package metrics

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/CesarManzoCode/anden/internal/wake"
)

func event(route string, start float64) wake.Event {
	return wake.Event{Service: "s", Route: route, RouteRequested: "restore", ClientAddr: "10.80.0.100:40112",
		PhasesMS: wake.PhasesMS{Detection: 1, Placement: 2, Rootfs: 3, Network: 4, Start: start, Readiness: 6, FirstByte: 7}}
}

// find returns the series of name whose labels include all of labels.
func find(t *testing.T, fams []*dto.MetricFamily, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			for k, v := range labels {
				if got[k] != v {
					continue series
				}
			}
			return m
		}
	}
	t.Fatalf("no %s%v", name, labels)
	return nil
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestObserveFillsHistogramsWithLabels(t *testing.T) {
	w := NewWakes()
	w.Observe(event("restore", 5))
	fams, err := w.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"detection": 0.001, "placement": 0.002, "rootfs": 0.003, "network": 0.004, "start": 0.005,
		"readiness": 0.006, "first_byte": 0.007}
	for phase, seconds := range want {
		h := find(t, fams, "anden_wake_phase_seconds", map[string]string{"phase": phase, "route": "restore"}).GetHistogram()
		if h.GetSampleCount() != 1 || !near(h.GetSampleSum(), seconds) {
			t.Fatalf("%s: count %d sum %g, want 1 and %g", phase, h.GetSampleCount(), h.GetSampleSum(), seconds)
		}
		idle := find(t, fams, "anden_wake_phase_seconds", map[string]string{"phase": phase, "route": "relaunch"}).GetHistogram()
		if idle.GetSampleCount() != 0 {
			t.Fatalf("%s: relaunch has %d samples", phase, idle.GetSampleCount())
		}
	}
	d := find(t, fams, "anden_wake_duration_seconds", map[string]string{"route": "restore"}).GetHistogram()
	if d.GetSampleCount() != 1 || !near(d.GetSampleSum(), 0.028) {
		t.Fatalf("duration: count %d sum %g, want 1 and 0.028", d.GetSampleCount(), d.GetSampleSum())
	}
	for _, b := range d.GetBucket() {
		wantCount := uint64(0)
		if b.GetUpperBound() >= 0.028 {
			wantCount = 1
		}
		if b.GetCumulativeCount() != wantCount {
			t.Fatalf("bucket le=%g has %d, want %d", b.GetUpperBound(), b.GetCumulativeCount(), wantCount)
		}
	}
	if got := find(t, fams, "anden_wake_duration_seconds", map[string]string{"route": "relaunch"}).GetHistogram(); got.GetSampleCount() != 0 {
		t.Fatalf("relaunch duration has %d samples", got.GetSampleCount())
	}
}

func TestNilWakesKeepsNothing(t *testing.T) {
	var w *Wakes
	w.Observe(event("relaunch", 1))
}

func agentServer(t *testing.T, w *Wakes) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(w.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestAggregatorSumsAgents(t *testing.T) {
	a, b := NewWakes(), NewWakes()
	a.Observe(event("relaunch", 5))
	a.Observe(event("relaunch", 2000))
	b.Observe(event("relaunch", 10))
	b.Observe(event("restore", 1))
	srvA, srvB := agentServer(t, a), agentServer(t, b)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	agg := &Aggregator{Nodes: func(context.Context) ([]Node, error) {
		return []Node{
			{Name: "node1", URL: srvA.URL + "/metrics", Up: true},
			{Name: "node2", URL: srvB.URL + "/metrics", Up: true},
			{Name: "node3", URL: gone.URL + "/metrics", Up: false},
		}, nil
	}}
	rec := httptest.NewRecorder()
	agg.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	fams, err := Parse(rec.Body)
	if err != nil {
		t.Fatal(err)
	}

	for node, want := range map[string]float64{"node1": 1, "node2": 1, "node3": 0} {
		if got := find(t, fams, "anden_node_up", map[string]string{"node": node}).GetGauge().GetValue(); got != want {
			t.Fatalf("anden_node_up{node=%q} = %g, want %g", node, got, want)
		}
	}
	relaunch := find(t, fams, "anden_wake_duration_seconds", map[string]string{"route": "relaunch"}).GetHistogram()
	if relaunch.GetSampleCount() != 3 || !near(relaunch.GetSampleSum(), (28+2023+33)/1000.0) {
		t.Fatalf("relaunch: count %d sum %g", relaunch.GetSampleCount(), relaunch.GetSampleSum())
	}
	for _, bk := range relaunch.GetBucket() {
		var want uint64
		switch {
		case bk.GetUpperBound() >= 2.5:
			want = 3
		case bk.GetUpperBound() >= 0.05:
			want = 2
		}
		if bk.GetCumulativeCount() != want {
			t.Fatalf("relaunch le=%g has %d, want %d", bk.GetUpperBound(), bk.GetCumulativeCount(), want)
		}
	}
	restore := find(t, fams, "anden_wake_duration_seconds", map[string]string{"route": "restore"}).GetHistogram()
	if restore.GetSampleCount() != 1 {
		t.Fatalf("restore: count %d", restore.GetSampleCount())
	}
	start := find(t, fams, "anden_wake_phase_seconds", map[string]string{"phase": "start", "route": "relaunch"}).GetHistogram()
	if start.GetSampleCount() != 3 || !near(start.GetSampleSum(), 2.015) {
		t.Fatalf("start relaunch: count %d sum %g", start.GetSampleCount(), start.GetSampleSum())
	}
}

func TestMergeRefusesOtherBounds(t *testing.T) {
	bucket := func(le float64, n uint64) *dto.Bucket { return &dto.Bucket{UpperBound: &le, CumulativeCount: &n} }
	fam := func(les ...float64) []*dto.MetricFamily {
		name, typ := "h", dto.MetricType_HISTOGRAM
		h := &dto.Histogram{SampleCount: new(uint64(1)), SampleSum: new(0.1)}
		for _, le := range les {
			h.Bucket = append(h.Bucket, bucket(le, 1))
		}
		return []*dto.MetricFamily{{Name: &name, Type: &typ, Metric: []*dto.Metric{{Histogram: h}}}}
	}
	if _, err := Merge(fam(0.5, 1), fam(0.5, 1, math.Inf(1))); err != nil {
		t.Fatalf("+Inf should not count as a bound: %v", err)
	}
	if _, err := Merge(fam(0.5, 1), fam(0.25, 1)); err == nil {
		t.Fatal("merged buckets with other bounds")
	}
}
