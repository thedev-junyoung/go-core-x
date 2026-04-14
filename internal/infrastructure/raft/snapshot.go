package raft

// Snapshot types, interfaces, and configuration for Phase 9a.
//
// Invariants:
//   - INV-S1: A snapshot at index N implies all entries [1..N] are committed and applied.
//   - INV-S2: After TakeSnapshot() returns, the captured SnapshotData is immutable
//     (COW semantics: caller holds a private copy).
//   - INV-S3: SnapshotStore.Save() must complete durably (fsync) before
//     CompactPrefix(N) is called on the LogStore.
//   - INV-S4: On restart, snapshot restoration precedes WAL log replay so that
//     duplicate entry application is avoided (entries <= snapshotIndex are filtered).

import (
	"errors"
	"time"
)

// SnapshotData is the serialisable snapshot of the KV state machine.
// The KV map is a private copy; mutations after TakeSnapshot are safe.
type SnapshotData struct {
	KV map[string]string
}

// SnapshotMeta identifies a snapshot file.
type SnapshotMeta struct {
	Index     int64     // last included Raft log index
	Term      int64     // term of the last included entry
	CreatedAt time.Time // wall-clock time at creation (informational)
	Size      int64     // on-disk file size in bytes (set by Save; 0 before Save)
	CRC32     uint32    // checksum of Header+Body (set by Save; 0 before Save)
}

// Snapshotable is the interface a state machine must implement to support
// snapshot-based recovery.
//
// TakeSnapshot must be safe to call concurrently with ongoing reads; it must
// capture a consistent (index, data) pair under the same lock acquisition.
// RestoreSnapshot replaces the entire state; callers must ensure no concurrent
// readers or writers exist during the call (startup or InstallSnapshot path).
type Snapshotable interface {
	// TakeSnapshot captures the current state machine state.
	// Returns a consistent (data, lastApplied) pair. lastTerm is resolved
	// separately by the caller (RaftNode) because it requires log access.
	TakeSnapshot() (data SnapshotData, lastApplied int64, err error)

	// RestoreSnapshot replaces the state machine state with data.
	// lastApplied is stored as the new sm.lastApplied baseline.
	RestoreSnapshot(data SnapshotData, lastApplied int64) error
}

// SnapshotStore persists and retrieves snapshot files.
//
// All Save/Load operations must be crash-safe: Save writes to a .tmp file,
// fsyncs, then renames atomically before returning success.
type SnapshotStore interface {
	// Save durably writes a snapshot. On return, meta.Size and meta.CRC32 are
	// populated with the values written to disk.
	Save(meta SnapshotMeta, data SnapshotData) error

	// Load reads and validates the snapshot at the given index.
	// Returns ErrSnapshotNotFound if no file exists for index.
	// Returns ErrCorruptedSnapshot if the CRC32 does not match.
	Load(index int64) (SnapshotMeta, SnapshotData, error)

	// Latest returns the metadata of the most recent snapshot.
	// Returns ErrSnapshotNotFound when no snapshots exist.
	Latest() (SnapshotMeta, error)

	// List returns all snapshot metadata sorted newest-first.
	List() ([]SnapshotMeta, error)

	// Prune deletes old snapshots, retaining the retainCount newest.
	// retainCount must be >= 1; values < 1 are treated as 1.
	Prune(retainCount int) error
}

// Sentinel errors for snapshot operations.
var (
	// ErrSnapshotNotFound is returned when no snapshot file exists for the
	// requested index (or when no snapshots exist at all).
	ErrSnapshotNotFound = errors.New("raft: snapshot not found")

	// ErrCorruptedSnapshot is returned when the CRC32 of a loaded snapshot file
	// does not match the stored checksum in the file footer.
	ErrCorruptedSnapshot = errors.New("raft: snapshot checksum mismatch")
)

// SnapshotConfig controls when and how snapshots are taken.
type SnapshotConfig struct {
	// Threshold is the minimum number of new entries (since the last snapshot)
	// required to trigger a new snapshot. 0 disables automatic snapshotting.
	Threshold int64

	// CheckInterval is how often the snapshot ticker fires. Meaningful only when
	// Threshold > 0. A zero value disables the ticker; maybeSnapshot can still
	// be called manually.
	CheckInterval time.Duration

	// Dir is the directory where snapshot files are written.
	Dir string

	// RetainCount is the number of snapshots to keep after pruning.
	// Minimum effective value is 1; values < 1 are clamped to 1.
	RetainCount int
}
