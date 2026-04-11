package raft

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"sync"
)

// metaRecordSize is the fixed byte length of a serialised RaftMeta record on disk.
//
// Layout (269 bytes total):
//
//	[0]       Version  : uint8  (always metaVersion)
//	[1:9]     Term     : int64  (little-endian)
//	[9]       VoteLen  : uint8  (length of VotedFor string; 0 = no vote)
//	[10:265]  VotedFor : [255]byte (zero-padded node ID)
//	[265:269] CRC32    : uint32 (little-endian, over bytes [0:265])
const (
	metaVersion      = 1
	metaVotedForSize = 255
	metaHeaderSize   = 1 + 8 + 1 + metaVotedForSize // 265 — covered by CRC
	metaChecksumSize = 4
	metaRecordSize   = metaHeaderSize + metaChecksumSize // 269
)

// RaftMeta holds the Raft persistent state fields defined in §5.
type RaftMeta struct {
	Term     int64
	VotedFor string // "" means no vote cast this term
}

// MetaStore persists and recovers RaftMeta with CRC integrity guarantees.
//
// Implementations must ensure that Save is durable (fsync'd) before returning,
// because Raft's safety properties depend on persisted state surviving crashes.
type MetaStore interface {
	// Load reads the last saved RaftMeta. Returns the zero value if no record
	// exists yet (first startup). Returns an error on corruption or I/O failure.
	Load() (RaftMeta, error)

	// Save writes m to stable storage. The call must not return until the data
	// is durable. On failure the caller must treat the RPC as rejected.
	Save(m RaftMeta) error

	// Close releases the underlying resource.
	Close() error
}

// FileMetaStore is a MetaStore backed by a fixed-size binary file.
// A single pwrite(2) at offset 0 overwrites the entire 269-byte record;
// because the record fits within one 512-byte disk sector the write is
// effectively atomic against torn-write failures. CRC32 provides an
// additional integrity check.
type FileMetaStore struct {
	f *os.File
}

// NewFileMetaStore opens or creates the metadata file at path.
func NewFileMetaStore(path string) (*FileMetaStore, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("raft meta: open %s: %w", path, err)
	}
	return &FileMetaStore{f: f}, nil
}

// Load reads the persistent state from the file.
// An empty file (first startup) returns the zero RaftMeta without error.
func (s *FileMetaStore) Load() (RaftMeta, error) {
	var buf [metaRecordSize]byte
	n, err := s.f.ReadAt(buf[:], 0)
	if n == 0 {
		// Empty file: return initial state.
		return RaftMeta{}, nil
	}
	if n != metaRecordSize {
		return RaftMeta{}, fmt.Errorf("raft meta: short read (%d of %d bytes)", n, metaRecordSize)
	}
	if err != nil {
		return RaftMeta{}, fmt.Errorf("raft meta: read: %w", err)
	}

	stored := binary.LittleEndian.Uint32(buf[metaHeaderSize:])
	computed := crc32.ChecksumIEEE(buf[:metaHeaderSize])
	if stored != computed {
		return RaftMeta{}, fmt.Errorf("raft meta: CRC mismatch (stored=0x%x computed=0x%x) — file may be corrupt", stored, computed)
	}

	if buf[0] != metaVersion {
		return RaftMeta{}, fmt.Errorf("raft meta: unsupported version %d", buf[0])
	}

	term := int64(binary.LittleEndian.Uint64(buf[1:9]))
	voteLen := int(buf[9])
	var votedFor string
	if voteLen > 0 {
		votedFor = string(buf[10 : 10+voteLen])
	}
	return RaftMeta{Term: term, VotedFor: votedFor}, nil
}

// Save serialises m and writes it to the file, then fsyncs.
func (s *FileMetaStore) Save(m RaftMeta) error {
	var buf [metaRecordSize]byte
	buf[0] = metaVersion
	binary.LittleEndian.PutUint64(buf[1:9], uint64(m.Term))

	voteBytes := []byte(m.VotedFor)
	if len(voteBytes) > metaVotedForSize {
		return fmt.Errorf("raft meta: votedFor too long (%d bytes, max %d)", len(voteBytes), metaVotedForSize)
	}
	buf[9] = byte(len(voteBytes))
	copy(buf[10:10+metaVotedForSize], voteBytes)

	checksum := crc32.ChecksumIEEE(buf[:metaHeaderSize])
	binary.LittleEndian.PutUint32(buf[metaHeaderSize:], checksum)

	if _, err := s.f.WriteAt(buf[:], 0); err != nil {
		return fmt.Errorf("raft meta: write: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("raft meta: sync: %w", err)
	}
	return nil
}

// Close closes the underlying file.
func (s *FileMetaStore) Close() error {
	return s.f.Close()
}

// MemMetaStore is an in-memory MetaStore used in tests that do not require
// disk persistence. It is safe for concurrent use.
type MemMetaStore struct {
	mu   sync.Mutex
	data RaftMeta
	// saveErr, if non-nil, is returned by every Save call (fault injection).
	saveErr error
}

// NewMemMetaStore creates an empty MemMetaStore.
func NewMemMetaStore() *MemMetaStore { return &MemMetaStore{} }

// InjectSaveError makes every subsequent Save return err.
// Pass nil to clear the injected error.
func (m *MemMetaStore) InjectSaveError(err error) {
	m.mu.Lock()
	m.saveErr = err
	m.mu.Unlock()
}

func (m *MemMetaStore) Load() (RaftMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data, nil
}

func (m *MemMetaStore) Save(meta RaftMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.data = meta
	return nil
}

func (m *MemMetaStore) Close() error { return nil }
