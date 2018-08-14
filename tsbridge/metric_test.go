// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tsbridge

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/ts-bridge/mocks"

	"github.com/golang/mock/gomock"
	"go.opencensus.io/stats/view"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
)

// checks that `value` is close to `target` within `margin`.
func durationWithin(value, target, margin time.Duration) bool {
	return math.Abs(value.Seconds()-target.Seconds()) <= margin.Seconds()
}

type fakeExporter struct {
	values map[string]view.AggregationData
}

func (e *fakeExporter) ExportView(d *view.Data) {
	for _, r := range d.Rows {
		var metricWithTags bytes.Buffer
		metricWithTags.WriteString(d.View.Name)
		for _, t := range r.Tags {
			metricWithTags.WriteString(fmt.Sprintf(":%v", t.Value))
		}
		e.values[metricWithTags.String()] = r.Data
	}
}
func (e *fakeExporter) Flush() {}
func fakeStats(t *testing.T) (*StatsCollector, *fakeExporter) {
	e := &fakeExporter{values: make(map[string]view.AggregationData)}
	c := &StatsCollector{Exporter: e}
	if err := c.registerAndCreateMetrics(); err != nil {
		t.Fatalf("Cannot initialize collector: %v", err)
	}
	return c, e
}

var metricUpdateTests = []struct {
	name       string
	setup      func(*mocks.MockSourceMetric, *mocks.MockStackdriverAdapter)
	wantStatus string
}{
	{"error getting timestamp", func(src *mocks.MockSourceMetric, sd *mocks.MockStackdriverAdapter) {
		// Update fails if we can't get latest timestamp from Stackdriver.
		sd.EXPECT().LatestTimestamp(gomock.Any(), "sd-project", "sd-metricname").Return(
			time.Time{}, fmt.Errorf("some-error"))
	}, "failed to get latest timestamp: some-error"},

	{"error getting new data", func(src *mocks.MockSourceMetric, sd *mocks.MockStackdriverAdapter) {
		// Update fails when we can't get fresh data from the source (e.g. Datadog).
		// This also verifies that `latest` is propagated correctly.
		latest := time.Now().Add(-5 * time.Minute)
		sd.EXPECT().LatestTimestamp(gomock.Any(), "sd-project", "sd-metricname").Return(latest, nil)
		src.EXPECT().StackdriverData(gomock.Any(), latest).Return(nil, nil, fmt.Errorf("another-error"))
	}, "failed to get data: another-error"},

	{"no new points", func(src *mocks.MockSourceMetric, sd *mocks.MockStackdriverAdapter) {
		// If `StackdriverData` returns no new points, this should be logged. It's not an error.
		latest := time.Now().Add(-5 * time.Minute)
		sd.EXPECT().LatestTimestamp(gomock.Any(), "sd-project", "sd-metricname").Return(latest, nil)
		src.EXPECT().StackdriverData(gomock.Any(), latest).Return(nil, nil, nil)
	}, "0 new points found"},

	{"error writing to stackdriver", func(src *mocks.MockSourceMetric, sd *mocks.MockStackdriverAdapter) {
		// In this case everything happens successfully up until we try to write data to Stackdriver.
		latest := time.Now().Add(-5 * time.Minute)
		sd.EXPECT().LatestTimestamp(gomock.Any(), "sd-project", "sd-metricname").Return(latest, nil)

		descr := &metricpb.MetricDescriptor{Description: "foobar"}
		ts := []*monitoringpb.TimeSeries{&monitoringpb.TimeSeries{ValueType: metricpb.MetricDescriptor_DOUBLE}}
		src.EXPECT().StackdriverData(gomock.Any(), latest).Return(descr, ts, nil)
		sd.EXPECT().CreateTimeseries(gomock.Any(), "sd-project", "sd-metricname", descr, ts).Return(
			fmt.Errorf("some-error"))
	}, "failed to write to Stackdriver: some-error"},

	{"success", func(src *mocks.MockSourceMetric, sd *mocks.MockStackdriverAdapter) {
		latest := time.Now().Add(-5 * time.Minute)
		sd.EXPECT().LatestTimestamp(gomock.Any(), "sd-project", "sd-metricname").Return(latest, nil)

		descr := &metricpb.MetricDescriptor{Description: "foobar"}
		ts := []*monitoringpb.TimeSeries{&monitoringpb.TimeSeries{ValueType: metricpb.MetricDescriptor_DOUBLE}}
		src.EXPECT().StackdriverData(gomock.Any(), latest).Return(descr, ts, nil)
		sd.EXPECT().CreateTimeseries(gomock.Any(), "sd-project", "sd-metricname", descr, ts).Return(nil)
	}, "1 new points found"},
}

func TestMetricUpdate(t *testing.T) {
	for _, tt := range metricUpdateTests {
		t.Run(tt.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			mockSource := mocks.NewMockSourceMetric(mockCtrl)
			mockSource.EXPECT().Query()
			mockSource.EXPECT().StackdriverName().MaxTimes(100).Return("sd-metricname")

			m, err := NewMetric(testCtx, "metricname", mockSource, "sd-project")
			if err != nil {
				t.Fatalf("error while creating metric: %v", err)
			}
			m.Record.LastStatus = "OK: all good"
			m.Record.LastAttempt = time.Now().Add(-time.Hour)

			mockSD := mocks.NewMockStackdriverAdapter(mockCtrl)
			tt.setup(mockSource, mockSD)

			collector, exporter := fakeStats(t)

			// Any errors during the update are recorded in MetricRecord, so the function itself
			// should succeed in all these cases.
			if err := m.Update(testCtx, mockSD, collector); err != nil {
				t.Errorf("Metric.Update() returned error %v", err)
			}
			if time.Now().Sub(m.Record.LastAttempt) > time.Minute {
				t.Errorf("expected to see LastAttempt updated")
			}
			if !strings.Contains(m.Record.LastStatus, tt.wantStatus) {
				t.Errorf("expected to see LastStatus contain '%s'; got %s", tt.wantStatus, m.Record.LastStatus)
			}
			collector.Close()
			if got, ok := exporter.values["ts_bridge/metric_import_latencies:metricname"]; !ok {
				t.Errorf("expected to see import latency recorded; got %v", got)
			}
		})
	}
}

func TestMetricImportLatencyMetric(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	mockSource := mocks.NewMockSourceMetric(mockCtrl)
	mockSource.EXPECT().Query()
	mockSource.EXPECT().StackdriverName().MaxTimes(100).Return("sd-metricname")

	m, err := NewMetric(testCtx, "metricname", mockSource, "sd-project")
	if err != nil {
		t.Fatalf("error while creating metric: %v", err)
	}
	mockSD := mocks.NewMockStackdriverAdapter(mockCtrl)
	mockSD.EXPECT().LatestTimestamp(gomock.Any(), "sd-project", "sd-metricname").DoAndReturn(
		func(ctx context.Context, project, name string) (time.Time, error) {
			time.Sleep(100 * time.Millisecond)
			return time.Now(), fmt.Errorf("some error")
		})

	collector, exporter := fakeStats(t)

	if err := m.Update(testCtx, mockSD, collector); err != nil {
		t.Errorf("Metric.Update() returned error %v", err)
	}
	collector.Close()

	val, ok := exporter.values["ts_bridge/metric_import_latencies:metricname"]
	got := time.Duration(val.(*view.DistributionData).Mean) * time.Millisecond
	if !ok || !durationWithin(got, 100*time.Millisecond, 40*time.Millisecond) {
		t.Errorf("expected to see import latency around 100ms; got %v", got)
	}
}

var updateAllMetricsTests = []struct {
	name             string
	numMetrics       int
	numPoints        int
	wantTotalLatency time.Duration
	wantOldestAge    time.Duration
}{
	{"1 metric, no points", 1, 0, 100 * time.Millisecond, time.Hour + 100*time.Millisecond},
	{"2 metric, no points", 1, 0, 200 * time.Millisecond, time.Hour + 100*time.Millisecond},
	{"1 metric, 1 points", 1, 1, 100 * time.Millisecond, 100 * time.Millisecond},
	{"2 metric, 1 points", 2, 1, 200 * time.Millisecond, 200 * time.Millisecond},
}

func TestUpdateAllMetrics(t *testing.T) {
	for _, tt := range updateAllMetricsTests {
		t.Run(tt.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			config := &Config{}
			for i := 0; i < tt.numMetrics; i++ {
				name := fmt.Sprintf("metric-%d", i)
				src := mocks.NewMockSourceMetric(mockCtrl)
				var ts []*monitoringpb.TimeSeries
				for j := 0; j < tt.numPoints; j++ {
					ts = append(ts, &monitoringpb.TimeSeries{ValueType: metricpb.MetricDescriptor_DOUBLE})
				}
				src.EXPECT().StackdriverData(gomock.Any(), gomock.Any()).Return(
					&metricpb.MetricDescriptor{}, ts, nil)
				src.EXPECT().StackdriverName().MaxTimes(100).Return(name)
				metric := &Metric{
					Name:   name,
					Record: &MetricRecord{LastUpdate: time.Now().Add(-time.Hour)},
					Source: src,
				}
				config.metrics = append(config.metrics, metric)
			}

			mockSD := mocks.NewMockStackdriverAdapter(mockCtrl)
			// Running LatestTimestamp for each metric takes 100ms. This is where most of latency comes from.
			mockSD.EXPECT().LatestTimestamp(gomock.Any(), gomock.Any(), gomock.Any()).Times(tt.numMetrics).DoAndReturn(
				func(ctx context.Context, project, name string) (time.Time, error) {
					time.Sleep(100 * time.Millisecond)
					return time.Now(), nil
				})
			mockSD.EXPECT().CreateTimeseries(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(
				tt.numMetrics * tt.numPoints).Return(nil)

			collector, exporter := fakeStats(t)

			if errs := UpdateAllMetrics(testCtx, config, mockSD, collector); len(errs) > 0 {
				t.Errorf("UpdateAllMetrics() returned errors: %v", errs)
			}
			collector.Close()

			val, ok := exporter.values["ts_bridge/import_latencies"]
			latency := time.Duration(val.(*view.DistributionData).Mean) * time.Millisecond
			want := time.Duration(tt.numMetrics*100) * time.Millisecond
			if !ok || !durationWithin(latency, want, 50*time.Millisecond) {
				t.Errorf("expected to see import latency around %v; got %v", want, latency)
			}

			val, ok = exporter.values["ts_bridge/oldest_metric_age"]
			age := time.Duration(val.(*view.LastValueData).Value) * time.Millisecond
			if !ok || !durationWithin(age, tt.wantOldestAge, 50*time.Millisecond) {
				t.Errorf("expected oldest metric age around %v; got %v", tt.wantOldestAge, age)
			}
		})
	}
}