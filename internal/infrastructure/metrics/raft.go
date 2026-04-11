package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// RaftMetricsSource is implemented by RaftNode to expose observable state.
type RaftMetricsSource interface {
	Term() int64
	RoleString() string
}

// RegisterRaftMetrics registers Raft Prometheus gauges.
// node may be nil (no-op in standalone mode).
func RegisterRaftMetrics(reg *prometheus.Registry, node RaftMetricsSource) {
	if node == nil {
		return
	}

	termGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_raft_term",
		Help: "Current Raft term of this node.",
	}, func() float64 {
		return float64(node.Term())
	})

	roleGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "core_x_raft_is_leader",
		Help: "1 if this node is the Raft leader, 0 otherwise.",
	}, func() float64 {
		if node.RoleString() == "leader" {
			return 1
		}
		return 0
	})

	reg.MustRegister(termGauge, roleGauge)
}
