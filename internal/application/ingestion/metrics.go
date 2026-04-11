package ingestion

// IngestMetrics is the port for recording ingestion telemetry.
//
// Why a port here?
// IngestionService must not import Prometheus (infrastructure concern).
// Injecting this interface keeps the Application layer testable and infrastructure-agnostic.
type IngestMetrics interface {
	// RecordIngest records one ingest attempt.
	//   status: "ok" | "overloaded" | "wal_error"
	//   latencySeconds: duration of the Ingest() call in seconds
	RecordIngest(status string, latencySeconds float64)

	// RecordQueueDepth records the current worker queue depth.
	RecordQueueDepth(depth int)
}
