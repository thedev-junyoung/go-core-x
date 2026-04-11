package metrics

import (
	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/prometheus/client_golang/prometheus"
)

// RegisterReplicationMetrics registers replication metrics from a ReplicationLag tracker.
// lag may be nil (single-node mode); in that case this is a no-op.
func RegisterReplicationMetrics(reg *prometheus.Registry, lag *replication.ReplicationLag) {
	if lag == nil {
		return
	}

	lagBytes := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_replication_lag_bytes",
		Help: "Approximate replication lag in bytes (primary only).",
	}, func() float64 { return float64(lag.Bytes()) })

	reconnects := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_replication_reconnect_total",
		Help: "Total number of replication reconnects.",
	}, func() float64 { return float64(lag.ReconnectCount()) })

	reg.MustRegister(lagBytes, reconnects)
}
