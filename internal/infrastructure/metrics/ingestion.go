package metrics

import (
	appingestion "github.com/junyoung/core-x/internal/application/ingestion"
	"github.com/prometheus/client_golang/prometheus"
)

// PromIngestMetrics implements application/ingestion.IngestMetrics using Prometheus.
type PromIngestMetrics struct {
	requests   *prometheus.CounterVec
	latency    *prometheus.HistogramVec
	queueDepth prometheus.Gauge
}

// NewPromIngestMetrics registers metrics and returns a PromIngestMetrics.
func NewPromIngestMetrics(reg *prometheus.Registry) *PromIngestMetrics {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "core_x_ingest_requests_total",
		Help: "Total number of ingest requests by status.",
	}, []string{"status"})

	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "core_x_ingest_latency_seconds",
		Help:    "Ingest request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"status"})

	queueDepth := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "core_x_worker_queue_depth",
		Help: "Current worker pool queue depth.",
	})

	reg.MustRegister(requests, latency, queueDepth)

	return &PromIngestMetrics{
		requests:   requests,
		latency:    latency,
		queueDepth: queueDepth,
	}
}

// RecordIngest implements IngestMetrics.
func (m *PromIngestMetrics) RecordIngest(status string, latencySeconds float64) {
	m.requests.WithLabelValues(status).Inc()
	m.latency.WithLabelValues(status).Observe(latencySeconds)
}

// RecordQueueDepth implements IngestMetrics.
func (m *PromIngestMetrics) RecordQueueDepth(depth int) {
	m.queueDepth.Set(float64(depth))
}

// Compile-time interface check.
var _ appingestion.IngestMetrics = (*PromIngestMetrics)(nil)
