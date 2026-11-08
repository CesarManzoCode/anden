// Package metrics is the Prometheus side of Andén (contract, section 5). Each
// agent keeps the wake histograms of its node, fed by its wake events, and
// serves them on /metrics; andend serves at 10.80.0.1:9090/metrics the sum of
// what the agents report plus anden_node_up.
package metrics

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/CesarManzoCode/anden/internal/wake"
)

const (
	// Port is where andend and every agent serve /metrics, each on its address.
	Port = 9090
	// scrapeTimeout bounds the scrape of one agent, so a partitioned node
	// does not hold the aggregate.
	scrapeTimeout = time.Second
)

// Buckets are in seconds. 1 s is a bound because it is the budget of C8 and
// of AndenWakeOverBudget.
var Buckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Routes are the wake routes of D03, whose series exist before any wake.
var Routes = []string{"relaunch", "restore"}

// Wakes are the histograms of the wakes of one node.
type Wakes struct {
	reg      *prometheus.Registry
	phase    *prometheus.HistogramVec
	duration *prometheus.HistogramVec
}

func NewWakes() *Wakes {
	w := &Wakes{
		reg: prometheus.NewRegistry(),
		phase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "anden_wake_phase_seconds",
			Help:    "Time of each phase of a wake, from the wake events.",
			Buckets: Buckets,
		}, []string{"phase", "route"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "anden_wake_duration_seconds",
			Help:    "Time of a wake, from the accept of the connection to the first byte, the sum of its phases.",
			Buckets: Buckets,
		}, []string{"route"}),
	}
	w.reg.MustRegister(w.phase, w.duration)
	for _, route := range Routes {
		w.duration.WithLabelValues(route)
		for _, name := range phaseNames() {
			w.phase.WithLabelValues(name, route)
		}
	}
	return w
}

// phaseNames are the keys of phases_ms, in the order of a wake.
func phaseNames() []string {
	names := []string{}
	for p := wake.Detection; p <= wake.FirstByte; p++ {
		names = append(names, p.String())
	}
	return names
}

// Observe puts one wake event in the histograms, under the route it used. A
// nil *Wakes keeps nothing.
func (w *Wakes) Observe(ev wake.Event) {
	if w == nil {
		return
	}
	ms := ev.PhasesMS
	values := []float64{ms.Detection, ms.Placement, ms.Rootfs, ms.Network, ms.Start, ms.Readiness, ms.FirstByte}
	total := 0.0
	for i, name := range phaseNames() {
		w.phase.WithLabelValues(name, ev.Route).Observe(values[i] / 1000)
		total += values[i]
	}
	w.duration.WithLabelValues(ev.Route).Observe(total / 1000)
}

func (w *Wakes) Gatherer() prometheus.Gatherer { return w.reg }

func (w *Wakes) Handler() http.Handler {
	return promhttp.HandlerFor(w.reg, promhttp.HandlerOpts{})
}

// Node is one node as the aggregate sees it.
type Node struct {
	Name string
	// URL is the /metrics of its agent.
	URL string
	Up  bool
}

// Aggregator serves the sum of what the agents report, plus anden_node_up.
type Aggregator struct {
	// Nodes lists the registered nodes.
	Nodes  func(ctx context.Context) ([]Node, error)
	Client *http.Client
}

// Gather scrapes every agent at once and adds their families up. An agent that
// does not answer is left out of the sum and logged; anden_node_up says
// whether the control plane counts its node alive.
func (a *Aggregator) Gather(ctx context.Context) ([]*dto.MetricFamily, error) {
	nodes, err := a.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	reg := prometheus.NewRegistry()
	up := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "anden_node_up",
		Help: "1 while the control plane counts the node alive (D08), 0 once it is dead.",
	}, []string{"node"})
	reg.MustRegister(up)
	for _, n := range nodes {
		v := 0.0
		if n.Up {
			v = 1
		}
		up.WithLabelValues(n.Name).Set(v)
	}
	own, err := reg.Gather()
	if err != nil {
		return nil, err
	}

	scraped := make([][]*dto.MetricFamily, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Go(func() {
			fams, err := a.scrape(ctx, n.URL)
			if err != nil {
				log.Printf("metrics: %s: %v", n.Name, err)
				return
			}
			scraped[i] = fams
		})
	}
	wg.Wait()
	return Merge(append([][]*dto.MetricFamily{own}, scraped...)...)
}

func (a *Aggregator) scrape(ctx context.Context, url string) ([]*dto.MetricFamily, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", string(expfmt.FmtText))
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return Parse(resp.Body)
}

// Parse reads the text format.
func Parse(r io.Reader) ([]*dto.MetricFamily, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	byName, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, err
	}
	fams := make([]*dto.MetricFamily, 0, len(byName))
	for _, f := range byName {
		fams = append(fams, f)
	}
	return fams, nil
}

// Write writes families in the text format.
func Write(w io.Writer, fams []*dto.MetricFamily) error {
	for _, f := range fams {
		if _, err := expfmt.MetricFamilyToText(w, f); err != nil {
			return err
		}
	}
	return nil
}

func (a *Aggregator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fams, err := a.Gather(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", string(expfmt.FmtText))
	if err := Write(w, fams); err != nil {
		log.Printf("metrics: write: %v", err)
	}
}

// Merge adds up families of the same name: series with the same labels sum
// their values, and histograms their count, sum and buckets, which must have
// the same bounds. The result is sorted by name and then by labels.
func Merge(sources ...[]*dto.MetricFamily) ([]*dto.MetricFamily, error) {
	byName := map[string]*dto.MetricFamily{}
	series := map[string]map[string]*dto.Metric{}
	for _, fams := range sources {
		for _, f := range fams {
			name := f.GetName()
			out, ok := byName[name]
			if !ok {
				out = &dto.MetricFamily{Name: f.Name, Help: f.Help, Type: f.Type}
				byName[name] = out
				series[name] = map[string]*dto.Metric{}
			} else if out.GetType() != f.GetType() {
				return nil, fmt.Errorf("%s: type %s and %s", name, out.GetType(), f.GetType())
			}
			for _, m := range f.GetMetric() {
				key := labelKey(m)
				if into, ok := series[name][key]; ok {
					if err := add(into, m, f.GetType()); err != nil {
						return nil, fmt.Errorf("%s{%s}: %w", name, key, err)
					}
					continue
				}
				fresh, err := clone(m, f.GetType())
				if err != nil {
					return nil, fmt.Errorf("%s{%s}: %w", name, key, err)
				}
				series[name][key] = fresh
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*dto.MetricFamily, 0, len(names))
	for _, name := range names {
		f := byName[name]
		keys := make([]string, 0, len(series[name]))
		for k := range series[name] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f.Metric = append(f.Metric, series[name][k])
		}
		out = append(out, f)
	}
	return out, nil
}

// labelKey identifies a series by its sorted labels.
func labelKey(m *dto.Metric) string {
	pairs := make([]string, 0, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		pairs = append(pairs, fmt.Sprintf("%s=%q", l.GetName(), l.GetValue()))
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ",")
}

// clone copies the labels and the value of m, without timestamps, exemplars
// or the +Inf bucket, which the text format writes from the count.
func clone(m *dto.Metric, t dto.MetricType) (*dto.Metric, error) {
	out := &dto.Metric{}
	for _, l := range m.GetLabel() {
		out.Label = append(out.Label, &dto.LabelPair{Name: l.Name, Value: l.Value})
	}
	switch t {
	case dto.MetricType_COUNTER:
		out.Counter = &dto.Counter{Value: new(m.GetCounter().GetValue())}
	case dto.MetricType_GAUGE:
		out.Gauge = &dto.Gauge{Value: new(m.GetGauge().GetValue())}
	case dto.MetricType_UNTYPED:
		out.Untyped = &dto.Untyped{Value: new(m.GetUntyped().GetValue())}
	case dto.MetricType_HISTOGRAM:
		h := m.GetHistogram()
		out.Histogram = &dto.Histogram{SampleCount: new(h.GetSampleCount()), SampleSum: new(h.GetSampleSum())}
		for _, b := range finiteBuckets(h) {
			out.Histogram.Bucket = append(out.Histogram.Bucket,
				&dto.Bucket{UpperBound: new(b.GetUpperBound()), CumulativeCount: new(b.GetCumulativeCount())})
		}
	default:
		return nil, fmt.Errorf("type %s is not summed", t)
	}
	return out, nil
}

// add adds the value of m to into, both of type t.
func add(into, m *dto.Metric, t dto.MetricType) error {
	switch t {
	case dto.MetricType_COUNTER:
		*into.Counter.Value += m.GetCounter().GetValue()
	case dto.MetricType_GAUGE:
		*into.Gauge.Value += m.GetGauge().GetValue()
	case dto.MetricType_UNTYPED:
		*into.Untyped.Value += m.GetUntyped().GetValue()
	case dto.MetricType_HISTOGRAM:
		h := m.GetHistogram()
		buckets := finiteBuckets(h)
		if len(buckets) != len(into.Histogram.Bucket) {
			return fmt.Errorf("%d buckets and %d", len(into.Histogram.Bucket), len(buckets))
		}
		for i, b := range buckets {
			if b.GetUpperBound() != into.Histogram.Bucket[i].GetUpperBound() {
				return fmt.Errorf("bucket bound %g and %g", into.Histogram.Bucket[i].GetUpperBound(), b.GetUpperBound())
			}
		}
		for i, b := range buckets {
			*into.Histogram.Bucket[i].CumulativeCount += b.GetCumulativeCount()
		}
		*into.Histogram.SampleCount += h.GetSampleCount()
		*into.Histogram.SampleSum += h.GetSampleSum()
	default:
		return fmt.Errorf("type %s is not summed", t)
	}
	return nil
}

// finiteBuckets are the buckets of h without +Inf, sorted by bound.
func finiteBuckets(h *dto.Histogram) []*dto.Bucket {
	out := make([]*dto.Bucket, 0, len(h.GetBucket()))
	for _, b := range h.GetBucket() {
		if !math.IsInf(b.GetUpperBound(), 1) {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b *dto.Bucket) int {
		switch {
		case a.GetUpperBound() < b.GetUpperBound():
			return -1
		case a.GetUpperBound() > b.GetUpperBound():
			return 1
		}
		return 0
	})
	return out
}
