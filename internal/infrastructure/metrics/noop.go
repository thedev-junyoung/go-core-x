package metrics

import appingestion "github.com/junyoung/core-x/internal/application/ingestion"

// NoopIngestMetrics is a zero-overhead IngestMetrics for tests.
// Discards all observations.
type NoopIngestMetrics struct{}

func (NoopIngestMetrics) RecordIngest(_ string, _ float64) {}
func (NoopIngestMetrics) RecordQueueDepth(_ int)           {}

// Compile-time interface check.
var _ appingestion.IngestMetrics = NoopIngestMetrics{}
