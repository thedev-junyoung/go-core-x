package ingestion_test

import (
	"testing"

	"github.com/junyoung/core-x/internal/application/ingestion"
	"github.com/junyoung/core-x/internal/domain"
)

type capturingMetrics struct {
	calls []string
}

func (m *capturingMetrics) RecordIngest(status string, _ float64) {
	m.calls = append(m.calls, status)
}
func (m *capturingMetrics) RecordQueueDepth(_ int) {}

type alwaysSubmitter struct{}

func (a *alwaysSubmitter) Submit(_ *domain.Event) bool { return true }

type neverSubmitter struct{}

func (n *neverSubmitter) Submit(_ *domain.Event) bool { return false }

type stubPool struct{}

func (s *stubPool) Acquire() *domain.Event  { return &domain.Event{} }
func (s *stubPool) Release(_ *domain.Event) {}

func TestIngestionService_Ingest_RecordsOkMetric(t *testing.T) {
	m := &capturingMetrics{}
	svc := ingestion.NewIngestionService(&alwaysSubmitter{}, &stubPool{}, nil)
	svc.SetMetrics(m)
	_ = svc.Ingest("src", "payload")
	if len(m.calls) != 1 || m.calls[0] != "ok" {
		t.Fatalf("expected [ok], got %v", m.calls)
	}
}

func TestIngestionService_Ingest_RecordsOverloadedMetric(t *testing.T) {
	m := &capturingMetrics{}
	svc := ingestion.NewIngestionService(&neverSubmitter{}, &stubPool{}, nil)
	svc.SetMetrics(m)
	_ = svc.Ingest("src", "payload")
	if len(m.calls) != 1 || m.calls[0] != "overloaded" {
		t.Fatalf("expected [overloaded], got %v", m.calls)
	}
}
