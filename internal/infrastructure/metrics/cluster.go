package metrics

import (
	"github.com/junyoung/core-x/internal/infrastructure/cluster"
	"github.com/prometheus/client_golang/prometheus"
)

// RegisterClusterMetrics registers ring health metrics.
// ring may be nil (single-node mode); in that case this is a no-op.
func RegisterClusterMetrics(reg *prometheus.Registry, ring *cluster.Ring) {
	if ring == nil {
		return
	}

	ringNodes := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_ring_nodes_total",
		Help: "Total number of nodes in the consistent hash ring.",
	}, func() float64 { return float64(ring.Len()) })

	reg.MustRegister(ringNodes)
}
