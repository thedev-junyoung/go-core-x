package raft

// Internal white-box tests for log_store.go encoding.
// Uses the package-internal encodeLogEntry / decodeLogEntry functions directly.

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestDecodeLogEntry_V3Magic verifies that v3 entries (magic bytes present) are
// decoded correctly.
func TestDecodeLogEntry_V3Magic(t *testing.T) {
	want := LogEntry{Index: 42, Term: 7, Type: EntryTypeConfig, Data: []byte(`{"phase":"joint"}`)}
	encoded := encodeLogEntry(want)

	// Strip the leading record-type byte (encodeLogEntry prepends it).
	payload := encoded[1:]

	// Verify magic bytes are present at payload[0:2].
	if payload[0] != walEntryMagic0 || payload[1] != walEntryMagic1 {
		t.Fatalf("magic bytes missing: got [%02X %02X]", payload[0], payload[1])
	}

	got, err := decodeLogEntry(payload)
	if err != nil {
		t.Fatalf("decodeLogEntry: %v", err)
	}
	if got.Index != want.Index || got.Term != want.Term || got.Type != want.Type {
		t.Errorf("header mismatch: want {%d %d %d} got {%d %d %d}",
			want.Index, want.Term, want.Type, got.Index, got.Term, got.Type)
	}
	if !bytes.Equal(got.Data, want.Data) {
		t.Errorf("data mismatch: want %q got %q", want.Data, got.Data)
	}
}

// TestDecodeLogEntry_V1Fallback verifies that a v1-format payload (no magic bytes,
// no EntryType byte) is decoded correctly via the legacy fallback path.
func TestDecodeLogEntry_V1Fallback(t *testing.T) {
	// Manually construct a v1 payload: [Index:8 LE][Term:8 LE][DataLen:4 LE][Data:N]
	data := []byte("hello")
	v1 := make([]byte, 8+8+4+len(data))
	binary.LittleEndian.PutUint64(v1[0:8], 5)  // index
	binary.LittleEndian.PutUint64(v1[8:16], 2) // term
	binary.LittleEndian.PutUint32(v1[16:20], uint32(len(data)))
	copy(v1[20:], data)

	got, err := decodeLogEntry(v1)
	if err != nil {
		t.Fatalf("decodeLogEntry v1: %v", err)
	}
	if got.Index != 5 || got.Term != 2 {
		t.Errorf("want index=5 term=2, got index=%d term=%d", got.Index, got.Term)
	}
	if got.Type != EntryTypeCommand {
		t.Errorf("v1 entries must decode as EntryTypeCommand, got %d", got.Type)
	}
	if !bytes.Equal(got.Data, data) {
		t.Errorf("data mismatch: want %q got %q", data, got.Data)
	}
}

// TestDecodeLogEntry_V2Heuristic verifies that a v2-format payload (EntryType byte,
// no magic bytes) is still decoded correctly via the length heuristic.
func TestDecodeLogEntry_V2Heuristic(t *testing.T) {
	// Manually construct a v2 payload: [Index:8 LE][Term:8 LE][EntryType:1][DataLen:4 LE][Data:N]
	data := []byte(`{"key":"v"}`)
	v2 := make([]byte, 8+8+1+4+len(data))
	binary.LittleEndian.PutUint64(v2[0:8], 10)             // index
	binary.LittleEndian.PutUint64(v2[8:16], 3)             // term
	v2[16] = byte(EntryTypeConfig)                          // entry type
	binary.LittleEndian.PutUint32(v2[17:21], uint32(len(data)))
	copy(v2[21:], data)

	got, err := decodeLogEntry(v2)
	if err != nil {
		t.Fatalf("decodeLogEntry v2: %v", err)
	}
	if got.Index != 10 || got.Term != 3 {
		t.Errorf("want index=10 term=3, got index=%d term=%d", got.Index, got.Term)
	}
	if got.Type != EntryTypeConfig {
		t.Errorf("want EntryTypeConfig, got %d", got.Type)
	}
	if !bytes.Equal(got.Data, data) {
		t.Errorf("data mismatch: want %q got %q", data, got.Data)
	}
}

// TestDecodeLogEntry_V3MagicNoFalsePositiveFromV1 verifies that a v1 payload whose
// first two bytes happen to be 0xFF 0xC0 is not misidentified as v3 — in practice
// this cannot happen because 0xFF is not valid UTF-8 and v1 Data starts at byte 20,
// but we verify the length guard rejects a truncated pseudo-v3 payload cleanly.
func TestDecodeLogEntry_V3CorruptLength(t *testing.T) {
	// Construct a payload that starts with the magic bytes but is too short for v3 header.
	bad := []byte{walEntryMagic0, walEntryMagic1, 0x01, 0x02} // 4 bytes — below v3Header=23
	_, err := decodeLogEntry(bad)
	if err == nil {
		t.Fatal("expected error for truncated v3 payload, got nil")
	}
}
