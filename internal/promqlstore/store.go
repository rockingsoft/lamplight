// Package promqlstore provides a bounded in-memory Prometheus-compatible
// sample store and evaluates instant PromQL queries against it.
package promqlstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/annotations"

	"lamplight/internal/model"
)

const (
	defaultRetention = time.Hour
	defaultTimeout   = 5 * time.Second
	maxQuerySamples  = 100_000
	metricNameLabel  = "__name__"
)

type Store struct {
	mu        sync.RWMutex
	series    map[string]*memorySeries
	engine    *promql.Engine
	retention time.Duration
	now       func() time.Time
}

type memorySeries struct {
	labels labels.Labels
	points []floatPoint
}

type floatPoint struct {
	timestamp int64
	value     float64
}

func New() *Store {
	return &Store{
		series:    map[string]*memorySeries{},
		retention: defaultRetention,
		now:       time.Now,
		engine: promql.NewEngine(promql.EngineOpts{
			MaxSamples:               maxQuerySamples,
			Timeout:                  defaultTimeout,
			LookbackDelta:            5 * time.Minute,
			NoStepSubqueryIntervalFn: func(int64) int64 { return 500 },
			EnableAtModifier:         true,
			EnableNegativeOffset:     true,
			EnableDelayedNameRemoval: true,
			EnableTypeAndUnitLabels:  false,
			UseStartTimestamps:       false,
		}),
	}
}

// Ingest records one scrape or OTLP export. Samples without an explicit source
// timestamp use receivedAt, which keeps scrape and push sources on one clock.
func (s *Store) Ingest(samples []model.MetricSample, receivedAt time.Time) error {
	if receivedAt.IsZero() {
		receivedAt = s.now()
	}
	timestamp := receivedAt.UnixMilli()
	oldest := receivedAt.Add(-s.retention).UnixMilli()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sample := range samples {
		if sample.Name == "" || math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
			return fmt.Errorf("cannot ingest invalid Prometheus sample %q", sample.Name)
		}
		labelMap := make(map[string]string, len(sample.Labels)+len(sample.Resource)+1)
		labelMap[metricNameLabel] = sample.Name
		for name, value := range sample.Labels {
			labelMap[name] = value
		}
		for name, value := range sample.Resource {
			labelMap["resource_"+name] = fmt.Sprint(value)
		}
		labelSet := labels.FromMap(labelMap)
		key := labelSet.String()
		item := s.series[key]
		if item == nil {
			item = &memorySeries{labels: labelSet}
			s.series[key] = item
		}
		item.points = insertPoint(item.points, floatPoint{timestamp: timestamp, value: sample.Value})
	}
	for key, item := range s.series {
		first := sort.Search(len(item.points), func(index int) bool { return item.points[index].timestamp >= oldest })
		item.points = append([]floatPoint(nil), item.points[first:]...)
		if len(item.points) == 0 {
			delete(s.series, key)
		}
	}
	return nil
}

func (s *Store) Snapshot(ctx context.Context, queryText string) (model.MetricSnapshot, error) {
	if strings.TrimSpace(queryText) == "" {
		return model.MetricSnapshot{}, errors.New("metric checks require a PromQL query")
	}
	now := s.now()
	query, err := s.engine.NewInstantQuery(ctx, s.queryable(), nil, queryText, now)
	if err != nil {
		return model.MetricSnapshot{}, fmt.Errorf("parse PromQL: %w", err)
	}
	defer query.Close()
	result := query.Exec(ctx)
	vector, err := result.Vector()
	if err != nil {
		return model.MetricSnapshot{}, fmt.Errorf("evaluate PromQL: %w", err)
	}
	samples := make([]model.MetricSample, 0, len(vector))
	for _, point := range vector {
		if point.H != nil {
			return model.MetricSnapshot{}, errors.New("native histogram PromQL results are not supported in metric assertions")
		}
		values := point.Metric.Map()
		name := values[metricNameLabel]
		delete(values, metricNameLabel)
		samples = append(samples, model.MetricSample{Name: name, Value: point.F, Labels: values})
	}
	sort.Slice(samples, func(i, j int) bool { return metricSampleKey(samples[i]) < metricSampleKey(samples[j]) })
	return model.MetricSnapshot{Samples: samples}, nil
}

func (s *Store) queryable() storage.Queryable {
	s.mu.RLock()
	series := make([]memorySeries, 0, len(s.series))
	for _, item := range s.series {
		series = append(series, memorySeries{labels: item.labels.Copy(), points: append([]floatPoint(nil), item.points...)})
	}
	s.mu.RUnlock()
	sort.Slice(series, func(i, j int) bool { return labels.Compare(series[i].labels, series[j].labels) < 0 })
	return memoryQueryable{series: series}
}

type memoryQueryable struct{ series []memorySeries }

func (q memoryQueryable) Querier(minimum, maximum int64) (storage.Querier, error) {
	return &memoryQuerier{series: q.series, minimum: minimum, maximum: maximum}, nil
}

type memoryQuerier struct {
	series           []memorySeries
	minimum, maximum int64
}

func (q *memoryQuerier) Select(_ context.Context, _ bool, _ *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	selected := []storage.Series{}
	for _, item := range q.series {
		if !matches(item.labels, matchers) {
			continue
		}
		points := make([]chunks.Sample, 0, len(item.points))
		for _, point := range item.points {
			if point.timestamp >= q.minimum && point.timestamp <= q.maximum {
				points = append(points, point)
			}
		}
		if len(points) > 0 {
			selected = append(selected, storage.NewListSeries(item.labels, points))
		}
	}
	return &seriesSet{series: selected, index: -1}
}

func (*memoryQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*memoryQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*memoryQuerier) Close() error { return nil }

type seriesSet struct {
	series []storage.Series
	index  int
}

func (s *seriesSet) Next() bool {
	s.index++
	return s.index < len(s.series)
}

func (s *seriesSet) At() storage.Series              { return s.series[s.index] }
func (*seriesSet) Err() error                        { return nil }
func (*seriesSet) Warnings() annotations.Annotations { return nil }

func matches(labelSet labels.Labels, matchers []*labels.Matcher) bool {
	for _, matcher := range matchers {
		if !matcher.Matches(labelSet.Get(matcher.Name)) {
			return false
		}
	}
	return true
}

func insertPoint(points []floatPoint, point floatPoint) []floatPoint {
	index := sort.Search(len(points), func(index int) bool { return points[index].timestamp >= point.timestamp })
	if index < len(points) && points[index].timestamp == point.timestamp {
		points[index] = point
		return points
	}
	points = append(points, floatPoint{})
	copy(points[index+1:], points[index:])
	points[index] = point
	return points
}

func metricSampleKey(sample model.MetricSample) string {
	names := make([]string, 0, len(sample.Labels))
	for name := range sample.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var key strings.Builder
	key.WriteString(sample.Name)
	for _, name := range names {
		fmt.Fprintf(&key, "\x00%s=%s", name, sample.Labels[name])
	}
	return key.String()
}

func (p floatPoint) T() int64                    { return p.timestamp }
func (p floatPoint) ST() int64                   { return 0 }
func (p floatPoint) F() float64                  { return p.value }
func (floatPoint) H() *histogram.Histogram       { return nil }
func (floatPoint) FH() *histogram.FloatHistogram { return nil }
func (floatPoint) Type() chunkenc.ValueType      { return chunkenc.ValFloat }
func (p floatPoint) Copy() chunks.Sample         { return p }

var _ model.MetricStore = (*Store)(nil)
