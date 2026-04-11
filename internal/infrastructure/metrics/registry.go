// Package metrics provides Prometheus-backed implementations of application-layer metric ports.
//
// Design: Each application port (IngestMetrics) has one Prometheus struct implementing it.
// All metric structs register with a shared prometheus.Registry passed at construction.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry creates a new Prometheus registry with Go runtime and process collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// NewHTTPHandler returns an http.Handler for the /metrics endpoint.
func NewHTTPHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}
