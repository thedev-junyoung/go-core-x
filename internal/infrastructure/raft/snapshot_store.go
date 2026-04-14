package raft

// FileSnapshotStore implements SnapshotStore using local filesystem files.
//
// File naming convention: snapshot-{index}-{term}.snap
// File format (ADR-017 §3):
//
//	[Header]
//	  Magic:   4 bytes  (0x52 0x41 0x46 0x54 = "RAFT")
//	  Version: 2 bytes  (0x00 0x01)
//	  Index:   8 bytes  (big-endian int64)
//	  Term:    8 bytes  (big-endian int64)
//	  KVCount: 8 bytes  (big-endian int64)
//
//	[Body: KVCount records]
//	  KeyLen:  2 bytes  (big-endian uint16)
//	  Key:     KeyLen bytes
//	  ValLen:  4 bytes  (big-endian uint32)
//	  Value:   ValLen bytes
//
//	[Footer]
//	  CRC32:   4 bytes  (IEEE of Header+Body)
//
// Crash safety: Save() writes to a .tmp file, fsyncs, then renames atomically.
// Startup: NewFileSnapshotStore removes any leftover .tmp files.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	snapshotMagic   = uint32(0x52414654) // "RAFT"
	snapshotVersion = uint16(0x0001)

	snapshotHeaderSize = 4 + 2 + 8 + 8 + 8 // magic + version + index + term + kvcount = 30
	snapshotFooterSize = 4                   // CRC32
)

// FileSnapshotStore stores snapshots as binary files on disk.
type FileSnapshotStore struct {
	dir string
}

// NewFileSnapshotStore creates a FileSnapshotStore rooted at dir.
// dir is created (with parents) if it does not exist.
// Any leftover .tmp files from a previous crashed Save() are removed.
func NewFileSnapshotStore(dir string) (*FileSnapshotStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("raft: snapshot store: create dir %s: %w", dir, err)
	}

	// Clean up leftover tmp files from previous crashed saves.
	tmps, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		return nil, fmt.Errorf("raft: snapshot store: glob tmp files: %w", err)
	}
	for _, tmp := range tmps {
		if removeErr := os.Remove(tmp); removeErr != nil {
			slog.Warn("raft: snapshot store: failed to remove leftover tmp file",
				"path", tmp, "err", removeErr)
		}
	}

	return &FileSnapshotStore{dir: dir}, nil
}

// Save durably writes the snapshot to disk.
// Procedure: encode → write to .tmp → fsync → rename to final path.
// On success, meta.Size and meta.CRC32 are populated.
func (s *FileSnapshotStore) Save(meta SnapshotMeta, data SnapshotData) error {
	finalPath := s.snapshotPath(meta.Index, meta.Term)
	tmpPath := finalPath + ".tmp"

	// Remove leftover tmp from a previous failed attempt.
	_ = os.Remove(tmpPath)

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("raft: snapshot store: create tmp %s: %w", tmpPath, err)
	}

	body, csum, err := encodeSnapshot(meta.Index, meta.Term, data)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("raft: snapshot store: encode: %w", err)
	}

	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("raft: snapshot store: write tmp: %w", err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("raft: snapshot store: fsync tmp: %w", err)
	}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("raft: snapshot store: stat tmp: %w", err)
	}
	fileSize := fi.Size()

	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("raft: snapshot store: close tmp: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("raft: snapshot store: rename to %s: %w", finalPath, err)
	}

	// Populate meta fields for the caller's reference.
	meta.Size = fileSize
	meta.CRC32 = csum

	slog.Debug("raft: snapshot saved",
		"index", meta.Index, "term", meta.Term,
		"size", meta.Size, "path", finalPath)

	return nil
}

// Load reads and validates the snapshot at index.
// Returns ErrSnapshotNotFound if the file does not exist.
// Returns ErrCorruptedSnapshot on CRC32 mismatch.
func (s *FileSnapshotStore) Load(index int64) (SnapshotMeta, SnapshotData, error) {
	// Discover matching file (term is part of filename, we only know index).
	metas, err := s.List()
	if err != nil {
		return SnapshotMeta{}, SnapshotData{}, err
	}
	var target *SnapshotMeta
	for i := range metas {
		if metas[i].Index == index {
			target = &metas[i]
			break
		}
	}
	if target == nil {
		return SnapshotMeta{}, SnapshotData{}, ErrSnapshotNotFound
	}

	path := s.snapshotPath(target.Index, target.Term)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SnapshotMeta{}, SnapshotData{}, ErrSnapshotNotFound
		}
		return SnapshotMeta{}, SnapshotData{}, fmt.Errorf("raft: snapshot store: read %s: %w", path, err)
	}

	meta, data, err := decodeSnapshot(raw)
	if err != nil {
		return SnapshotMeta{}, SnapshotData{}, err
	}

	fi, err := os.Stat(path)
	if err == nil {
		meta.Size = fi.Size()
	}
	meta.CreatedAt = time.Time{} // not stored in file; caller may use file mtime if needed

	return meta, data, nil
}

// Latest returns metadata of the most recent snapshot (highest index).
// Returns ErrSnapshotNotFound when no snapshots exist.
func (s *FileSnapshotStore) Latest() (SnapshotMeta, error) {
	metas, err := s.List()
	if err != nil {
		return SnapshotMeta{}, err
	}
	if len(metas) == 0 {
		return SnapshotMeta{}, ErrSnapshotNotFound
	}
	// List() returns newest-first.
	return metas[0], nil
}

// List returns all snapshot metadata sorted newest-first (highest index first).
func (s *FileSnapshotStore) List() ([]SnapshotMeta, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "snapshot-*.snap"))
	if err != nil {
		return nil, fmt.Errorf("raft: snapshot store: glob: %w", err)
	}

	metas := make([]SnapshotMeta, 0, len(matches))
	for _, path := range matches {
		index, term, err := parseSnapshotFilename(filepath.Base(path))
		if err != nil {
			slog.Warn("raft: snapshot store: skip unparseable file", "path", path, "err", err)
			continue
		}
		fi, err := os.Stat(path)
		size := int64(0)
		if err == nil {
			size = fi.Size()
		}
		metas = append(metas, SnapshotMeta{
			Index: index,
			Term:  term,
			Size:  size,
		})
	}

	// Sort newest-first (descending by index).
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].Index > metas[j].Index
	})

	return metas, nil
}

// Prune deletes old snapshots, keeping the retainCount newest.
// retainCount < 1 is treated as 1.
func (s *FileSnapshotStore) Prune(retainCount int) error {
	if retainCount < 1 {
		retainCount = 1
	}

	metas, err := s.List()
	if err != nil {
		return err
	}

	if len(metas) <= retainCount {
		return nil // nothing to prune
	}

	toDelete := metas[retainCount:] // oldest snapshots (List is newest-first)
	for _, m := range toDelete {
		path := s.snapshotPath(m.Index, m.Term)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("raft: snapshot store: failed to prune old snapshot",
				"path", path, "err", err)
		} else {
			slog.Debug("raft: snapshot pruned", "index", m.Index, "term", m.Term)
		}
	}

	return nil
}

// snapshotPath returns the canonical path for a snapshot with given index and term.
func (s *FileSnapshotStore) snapshotPath(index, term int64) string {
	return filepath.Join(s.dir, fmt.Sprintf("snapshot-%d-%d.snap", index, term))
}

// parseSnapshotFilename extracts index and term from "snapshot-{index}-{term}.snap".
func parseSnapshotFilename(name string) (index, term int64, err error) {
	// Strip extension.
	name = strings.TrimSuffix(name, ".snap")
	// Strip "snapshot-" prefix.
	name = strings.TrimPrefix(name, "snapshot-")

	parts := strings.SplitN(name, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("raft: snapshot: bad filename format: %q", name)
	}

	idx, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("raft: snapshot: parse index %q: %w", parts[0], err)
	}
	t, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("raft: snapshot: parse term %q: %w", parts[1], err)
	}

	return idx, t, nil
}

// marshalSnapshotData serialises a SnapshotData into the ADR-017 §3 on-disk
// format (header + KV body + CRC32 footer). The index and term are embedded in
// the header; the caller must supply a consistent (index, term) pair.
//
// This is the canonical serialisation path shared by Save (disk) and
// InstallSnapshot (network transfer).
func marshalSnapshotData(index, term int64, data SnapshotData) ([]byte, error) {
	b, _, err := encodeSnapshot(index, term, data)
	return b, err
}

// unmarshalSnapshotData parses bytes produced by marshalSnapshotData back into
// SnapshotMeta and SnapshotData. Returns ErrCorruptedSnapshot on CRC mismatch.
func unmarshalSnapshotData(raw []byte) (SnapshotMeta, SnapshotData, error) {
	return decodeSnapshot(raw)
}

// encodeSnapshot serialises a snapshot into the wire format and returns
// the complete byte slice and the CRC32 of Header+Body.
func encodeSnapshot(index, term int64, data SnapshotData) ([]byte, uint32, error) {
	kvCount := int64(len(data.KV))

	// Pre-calculate body size to avoid re-allocation.
	bodySize := 0
	for k, v := range data.KV {
		bodySize += 2 + len(k) + 4 + len(v)
	}

	totalSize := snapshotHeaderSize + bodySize + snapshotFooterSize
	buf := make([]byte, 0, totalSize)

	// --- Header ---
	var hdr [snapshotHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], snapshotMagic)
	binary.BigEndian.PutUint16(hdr[4:6], snapshotVersion)
	binary.BigEndian.PutUint64(hdr[6:14], uint64(index))
	binary.BigEndian.PutUint64(hdr[14:22], uint64(term))
	binary.BigEndian.PutUint64(hdr[22:30], uint64(kvCount))
	buf = append(buf, hdr[:]...)

	// --- Body ---
	for k, v := range data.KV {
		if len(k) > 0xFFFF {
			return nil, 0, fmt.Errorf("raft: snapshot encode: key too long (%d bytes)", len(k))
		}
		if len(v) > 0xFFFFFFFF {
			return nil, 0, fmt.Errorf("raft: snapshot encode: value too long (%d bytes)", len(v))
		}

		var klenBuf [2]byte
		binary.BigEndian.PutUint16(klenBuf[:], uint16(len(k)))
		buf = append(buf, klenBuf[:]...)
		buf = append(buf, k...)

		var vlenBuf [4]byte
		binary.BigEndian.PutUint32(vlenBuf[:], uint32(len(v)))
		buf = append(buf, vlenBuf[:]...)
		buf = append(buf, v...)
	}

	// --- Footer: CRC32 over Header+Body ---
	csum := crc32.ChecksumIEEE(buf)
	var footer [snapshotFooterSize]byte
	binary.BigEndian.PutUint32(footer[:], csum)
	buf = append(buf, footer[:]...)

	return buf, csum, nil
}

// decodeSnapshot parses raw bytes into SnapshotMeta and SnapshotData.
// Returns ErrCorruptedSnapshot on CRC32 mismatch or malformed data.
func decodeSnapshot(raw []byte) (SnapshotMeta, SnapshotData, error) {
	if len(raw) < snapshotHeaderSize+snapshotFooterSize {
		return SnapshotMeta{}, SnapshotData{},
			fmt.Errorf("%w: file too short (%d bytes)", ErrCorruptedSnapshot, len(raw))
	}

	// Validate CRC32: computed over everything except the 4-byte footer.
	csumOffset := len(raw) - snapshotFooterSize
	storedCsum := binary.BigEndian.Uint32(raw[csumOffset:])
	computedCsum := crc32.ChecksumIEEE(raw[:csumOffset])
	if storedCsum != computedCsum {
		return SnapshotMeta{}, SnapshotData{}, ErrCorruptedSnapshot
	}

	// Parse header.
	magic := binary.BigEndian.Uint32(raw[0:4])
	if magic != snapshotMagic {
		return SnapshotMeta{}, SnapshotData{},
			fmt.Errorf("%w: bad magic 0x%08X", ErrCorruptedSnapshot, magic)
	}
	// version := binary.BigEndian.Uint16(raw[4:6]) — reserved for future migration
	index := int64(binary.BigEndian.Uint64(raw[6:14]))
	term := int64(binary.BigEndian.Uint64(raw[14:22]))
	kvCount := int64(binary.BigEndian.Uint64(raw[22:30]))

	meta := SnapshotMeta{
		Index: index,
		Term:  term,
		CRC32: storedCsum,
	}

	// Parse body.
	kv := make(map[string]string, kvCount)
	r := raw[snapshotHeaderSize:csumOffset]
	pos := 0
	for i := int64(0); i < kvCount; i++ {
		if pos+2 > len(r) {
			return SnapshotMeta{}, SnapshotData{},
				fmt.Errorf("%w: truncated key length at record %d", ErrCorruptedSnapshot, i)
		}
		kLen := int(binary.BigEndian.Uint16(r[pos : pos+2]))
		pos += 2

		if pos+kLen > len(r) {
			return SnapshotMeta{}, SnapshotData{},
				fmt.Errorf("%w: truncated key at record %d", ErrCorruptedSnapshot, i)
		}
		key := string(r[pos : pos+kLen])
		pos += kLen

		if pos+4 > len(r) {
			return SnapshotMeta{}, SnapshotData{},
				fmt.Errorf("%w: truncated value length at record %d", ErrCorruptedSnapshot, i)
		}
		vLen := int(binary.BigEndian.Uint32(r[pos : pos+4]))
		pos += 4

		if pos+vLen > len(r) {
			return SnapshotMeta{}, SnapshotData{},
				fmt.Errorf("%w: truncated value at record %d", ErrCorruptedSnapshot, i)
		}
		val := string(r[pos : pos+vLen])
		pos += vLen

		kv[key] = val
	}

	return meta, SnapshotData{KV: kv}, nil
}
