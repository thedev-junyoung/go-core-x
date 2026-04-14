package metrics

import "github.com/prometheus/client_golang/prometheus"

// SnapshotMetricsSource is implemented by RaftNode to expose snapshot state.
type SnapshotMetricsSource interface {
	SnapshotIndex() int64 // last snapshot index (0 = none)
}

// SnapshotRecorder is the write-side interface for snapshot event metrics.
// Implemented by PromSnapshotMetrics.
type SnapshotRecorder interface {
	// RecordSnapshotCreated records a snapshot creation event.
	//   status: "ok" | "error"
	//   sizeBytes: snapshot file size
	//   durationSeconds: time taken for COW + file write
	RecordSnapshotCreated(status string, sizeBytes float64, durationSeconds float64)

	// RecordSnapshotInstalled records an InstallSnapshot RPC received by a follower.
	RecordSnapshotInstalled()

	// RecordLogCompacted records how many WAL entries were compacted.
	RecordLogCompacted(count float64)
}

// PromSnapshotMetrics implements SnapshotRecorder using Prometheus.
type PromSnapshotMetrics struct {
	createdTotal    *prometheus.CounterVec
	sizeBytes       prometheus.Histogram
	durationSeconds prometheus.Histogram
	installedTotal  prometheus.Counter
	compactedTotal  prometheus.Counter
}

// NewPromSnapshotMetrics creates and registers all snapshot Prometheus metrics.
func NewPromSnapshotMetrics(reg *prometheus.Registry) *PromSnapshotMetrics {
	m := &PromSnapshotMetrics{
		createdTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "core_x_snapshot_created_total",
			Help: "Total number of snapshot creation attempts.",
		}, []string{"status"}), // status: "ok" | "error"

		sizeBytes: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "core_x_snapshot_size_bytes",
			Help:    "Snapshot file size in bytes.",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 10), // 1KB ~ 1GB
		}),

		durationSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "core_x_snapshot_duration_seconds",
			Help:    "Time taken to create a snapshot (COW copy + file write).",
			Buckets: prometheus.DefBuckets,
		}),

		installedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "core_x_snapshot_install_total",
			Help: "Total number of InstallSnapshot RPCs received by this follower.",
		}),

		compactedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "core_x_log_compacted_entries_total",
			Help: "Total number of Raft log entries removed by snapshot compaction.",
		}),
	}
	reg.MustRegister(
		m.createdTotal,
		m.sizeBytes,
		m.durationSeconds,
		m.installedTotal,
		m.compactedTotal,
	)
	return m
}

func (m *PromSnapshotMetrics) RecordSnapshotCreated(status string, sizeBytes, durationSeconds float64) {
	m.createdTotal.WithLabelValues(status).Inc()
	if status == "ok" {
		m.sizeBytes.Observe(sizeBytes)
		m.durationSeconds.Observe(durationSeconds)
	}
}

func (m *PromSnapshotMetrics) RecordSnapshotInstalled() {
	m.installedTotal.Inc()
}

func (m *PromSnapshotMetrics) RecordLogCompacted(count float64) {
	m.compactedTotal.Add(count)
}

// RegisterSnapshotGauge registers a GaugeFunc for core_x_snapshot_index.
// Called separately because it requires a SnapshotMetricsSource (RaftNode).
// No-op if node is nil (standalone mode).
func RegisterSnapshotGauge(reg *prometheus.Registry, node SnapshotMetricsSource) {
	if node == nil {
		return
	}
	gauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_snapshot_index",
		Help: "Last Raft index covered by the most recent snapshot (0 = none).",
	}, func() float64 {
		return float64(node.SnapshotIndex())
	})
	reg.MustRegister(gauge)
}
