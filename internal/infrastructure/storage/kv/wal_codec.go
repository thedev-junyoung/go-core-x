// Package kv — WAL record encoding/decoding for Raft KV data.
//
// Record format (payload written into wal.Writer):
//
//	KV set:  [type:1=0x01][raftIndex:8 LE][keyLen:2 LE][key:N][valueLen:2 LE][value:M]
//	KV del:  [type:1=0x02][raftIndex:8 LE][keyLen:2 LE][key:N]
//	meta:    [type:1=0x03][raftIndex:8 LE][keyLen:2 LE][key:N][valueLen:2 LE][value:M]
//
// The meta record uses the reserved key "__raft_last_applied__" to checkpoint
// the last Raft log index applied to this KV store.
package kv

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Record type bytes for the KV WAL payload.
const (
	kvRecordTypeSet  = byte(0x01) // set key=value
	kvRecordTypeDel  = byte(0x02) // delete key
	kvRecordTypeMeta = byte(0x03) // metadata (e.g. __raft_last_applied__)
)

// MetaKeyLastApplied is the reserved WAL key used to checkpoint lastAppliedIndex.
// User-supplied keys must not equal this value; WriteKV/DeleteKV enforce this.
const MetaKeyLastApplied = "__raft_last_applied__"

// ErrReservedKey is returned when a caller passes MetaKeyLastApplied as a user key.
var ErrReservedKey = errors.New("kv: key is reserved for internal use")

// encodeKVSet encodes a set record into dst (appends and returns).
// Format: [0x01][raftIndex:8 LE][keyLen:2 LE][key:N][valueLen:2 LE][value:M]
func encodeKVSet(dst []byte, key, value string, raftIndex int64) []byte {
	dst = append(dst, kvRecordTypeSet)
	dst = appendInt64LE(dst, raftIndex)
	dst = appendString2(dst, key)
	dst = appendString2(dst, value)
	return dst
}

// encodeKVDel encodes a delete record into dst (appends and returns).
// Format: [0x02][raftIndex:8 LE][keyLen:2 LE][key:N]
func encodeKVDel(dst []byte, key string, raftIndex int64) []byte {
	dst = append(dst, kvRecordTypeDel)
	dst = appendInt64LE(dst, raftIndex)
	dst = appendString2(dst, key)
	return dst
}

// encodeKVMeta encodes a meta record into dst (appends and returns).
// Format: [0x03][raftIndex:8 LE][keyLen:2 LE][key:N][valueLen:2 LE][value:M]
func encodeKVMeta(dst []byte, key string, raftIndex int64) []byte {
	// value for the meta record is empty; raftIndex carries the checkpoint value.
	dst = append(dst, kvRecordTypeMeta)
	dst = appendInt64LE(dst, raftIndex)
	dst = appendString2(dst, key)
	dst = appendString2(dst, "") // value field unused for meta; raftIndex is the payload
	return dst
}

// KVRecord is the decoded result of a single KV WAL payload.
type KVRecord struct {
	Type      byte
	RaftIndex int64
	Key       string
	Value     string // empty for del and meta records
}

// decodeKVRecord parses a raw WAL payload into a KVRecord.
// Returns ErrCorrupted if the payload is malformed.
func decodeKVRecord(data []byte) (KVRecord, error) {
	if len(data) < 1 {
		return KVRecord{}, fmt.Errorf("kv: empty record payload: %w", ErrCorrupted)
	}

	recType := data[0]
	data = data[1:]

	// All record types carry raftIndex (8 bytes LE).
	if len(data) < 8 {
		return KVRecord{}, fmt.Errorf("kv: record too short for raftIndex: %w", ErrCorrupted)
	}
	raftIndex := int64(binary.LittleEndian.Uint64(data[:8]))
	data = data[8:]

	// Read key (2-byte length prefix).
	key, rest, err := readString2(data)
	if err != nil {
		return KVRecord{}, fmt.Errorf("kv: failed to read key: %w", err)
	}
	data = rest

	var value string
	if recType == kvRecordTypeSet || recType == kvRecordTypeMeta {
		// Read value (2-byte length prefix).
		value, _, err = readString2(data)
		if err != nil {
			return KVRecord{}, fmt.Errorf("kv: failed to read value: %w", err)
		}
	}

	return KVRecord{
		Type:      recType,
		RaftIndex: raftIndex,
		Key:       key,
		Value:     value,
	}, nil
}

// ErrCorrupted is returned when a KV WAL record cannot be decoded.
var ErrCorrupted = errors.New("kv: corrupted record")

// --- encoding helpers --------------------------------------------------------

func appendInt64LE(dst []byte, v int64) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(v))
	return append(dst, buf[:]...)
}

// appendString2 appends a 2-byte LE length prefix followed by the string bytes.
// Supports strings up to 65535 bytes.
func appendString2(dst []byte, s string) []byte {
	n := len(s)
	dst = append(dst, byte(n), byte(n>>8)) // little-endian 2 bytes
	return append(dst, s...)
}

// readString2 reads a 2-byte LE length-prefixed string from data.
// Returns the string, the remaining bytes, and any error.
func readString2(data []byte) (string, []byte, error) {
	if len(data) < 2 {
		return "", nil, fmt.Errorf("kv: insufficient bytes for string length: %w", ErrCorrupted)
	}
	n := int(data[0]) | int(data[1])<<8 // little-endian
	data = data[2:]
	if len(data) < n {
		return "", nil, fmt.Errorf("kv: insufficient bytes for string data (need %d, have %d): %w", n, len(data), ErrCorrupted)
	}
	return string(data[:n]), data[n:], nil
}
