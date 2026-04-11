package replication_test

import (
	"testing"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
)

func TestReplicationLag(t *testing.T) {
	lag := replication.NewReplicationLag()

	if got := lag.Bytes(); got != 0 {
		t.Fatalf("initial lag = %d, want 0", got)
	}

	lag.UpdatePrimary(1000)
	lag.UpdateReplica(600)

	if got := lag.Bytes(); got != 400 {
		t.Errorf("lag = %d, want 400", got)
	}

	lag.UpdateReplica(1200)
	if got := lag.Bytes(); got < 0 {
		t.Errorf("lag should not be negative, got %d", got)
	}
}
