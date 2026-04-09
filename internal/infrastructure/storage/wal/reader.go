// Package wal provides Reader for sequential WAL file access.
package wal

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"time"

	"github.com/junyoung/core-x/internal/domain"
)

// Record represents a single WAL record read from the file.
type Record struct {
	// Timestamp is the Unix nanosecond timestamp from the WAL header.
	// Used to reconstruct Event.ReceivedAt during recovery.
	Timestamp time.Time

	// Data is the raw payload bytes (before Event decoding).
	// Caller is responsible for calling DecodeEvent() to extract Event.
	Data []byte
}

// Reader provides sequential read access to a WAL file using iterator pattern.
// Similar to bufio.Scanner: Scan() to advance, Record() to get current, Err() to check error.
//
// Concurrency: not goroutine-safe. Single-threaded reading only (recovery path).
type Reader struct {
	// file is the opened WAL file handle.
	file *os.File

	// bufferedReader provides buffered I/O for sequential reading.
	// 4KB buffer reduces syscall count from ~250k to ~25k for 100MB files.
	bufferedReader *bufio.Reader

	// currentRecord holds the most recently read record.
	// Valid only after Scan() returns true.
	currentRecord Record

	// lastError is the sticky error state.
	// Once set, all subsequent Scan() calls return false.
	lastError error
}

// NewReader creates a new Reader for the WAL file at path.
// The file must exist and be readable.
//
// Returns an error if the file cannot be opened.
func NewReader(path string) (*Reader, error) {
	file, error := os.Open(path)
	if error != nil {
		return nil, error
	}

	return &Reader{
		file:           file,
		bufferedReader: bufio.NewReaderSize(file, 4*1024),
	}, nil
}

// Scan advances to the next record and returns true if a valid record was read.
// Returns false at EOF or on error. Call Err() after Scan() returns false to check for errors.
//
// Iterator pattern (like bufio.Scanner):
// - true: Record() is valid
// - false: Err() will return nil (EOF) or an error
//
// Once Scan() returns false, all subsequent calls return false (sticky state).
func (reader *Reader) Scan() bool {
	// Sticky error state: once an error occurs, stop scanning
	if reader.lastError != nil {
		return false
	}

	record, error := reader.readRecord()
	if error != nil {
		// Check if it's normal EOF or an error
		if error == io.EOF {
			// Normal end of file, not an error state
			return false
		}
		// Record error and stop
		reader.lastError = error
		return false
	}

	reader.currentRecord = record
	return true
}

// Record returns the current record after a successful Scan().
// Only valid immediately after Scan() returns true.
func (reader *Reader) Record() Record {
	return reader.currentRecord
}

// Err returns the error encountered during reading, or nil if EOF was reached normally.
// Check this after Scan() returns false to distinguish between normal EOF and error.
//
// Common return values:
// - nil: normal EOF (all records read successfully)
// - ErrTruncated: partial record at EOF (expected in crash scenario)
// - ErrCorrupted: invalid magic number (filesystem damage)
// - ErrChecksumMismatch: CRC32 validation failed (data corruption)
func (reader *Reader) Err() error {
	return reader.lastError
}

// Close closes the underlying WAL file handle.
func (reader *Reader) Close() error {
	if reader.file != nil {
		return reader.file.Close()
	}
	return nil
}

// readRecord reads the next complete record from the file.
// Returns (record, nil) on success, (empty, error) on failure.
//
// Record format: [Magic:4][Timestamp:8][Size:4][Payload:N][CRC32:4]
//
// Error handling:
// - io.EOF: normal end of file
// - io.ErrUnexpectedEOF: incomplete record at EOF (tail truncation)
// - ErrCorrupted: invalid magic
// - ErrChecksumMismatch: CRC32 validation failed
func (reader *Reader) readRecord() (Record, error) {
	// Read header: Magic(4) + Timestamp(8) + Size(4) = 16 bytes
	headerBuffer := make([]byte, RecordHeaderSize)
	_, headerReadError := io.ReadFull(reader.bufferedReader, headerBuffer)

	if headerReadError != nil {
		if headerReadError == io.EOF {
			// Normal end of file
			return Record{}, io.EOF
		}
		// io.ErrUnexpectedEOF: incomplete header
		if headerReadError == io.ErrUnexpectedEOF {
			return Record{}, ErrTruncated
		}
		return Record{}, headerReadError
	}

	// Parse header
	magic := binary.BigEndian.Uint32(headerBuffer[0:4])
	timestampNano := binary.BigEndian.Uint64(headerBuffer[4:12])
	payloadSize := binary.BigEndian.Uint32(headerBuffer[12:16])

	// Validate magic
	if magicValidationError := validateMagic(magic); magicValidationError != nil {
		return Record{}, magicValidationError
	}

	// Read payload
	payloadBuffer := make([]byte, payloadSize)
	_, payloadReadError := io.ReadFull(reader.bufferedReader, payloadBuffer)

	if payloadReadError != nil {
		if payloadReadError == io.ErrUnexpectedEOF {
			return Record{}, ErrTruncated
		}
		return Record{}, payloadReadError
	}

	// Read checksum (4 bytes)
	checksumBuffer := make([]byte, RecordChecksumSize)
	_, checksumReadError := io.ReadFull(reader.bufferedReader, checksumBuffer)

	if checksumReadError != nil {
		if checksumReadError == io.ErrUnexpectedEOF {
			return Record{}, ErrTruncated
		}
		return Record{}, checksumReadError
	}

	storedChecksum := binary.BigEndian.Uint32(checksumBuffer)

	// Verify checksum: computed over header + payload
	computedChecksum := crc32.ChecksumIEEE(headerBuffer)
	computedChecksum = crc32.Update(computedChecksum, crc32.IEEETable, payloadBuffer)

	if computedChecksum != storedChecksum {
		return Record{}, ErrChecksumMismatch
	}

	// Return valid record
	return Record{
		Timestamp: time.Unix(0, int64(timestampNano)),
		Data:      payloadBuffer,
	}, nil
}

// validateMagic checks if the magic number is valid.
// Currently only supports current protocol version (0xCAFEBABE).
func validateMagic(magic uint32) error {
	if magic != MagicNumber {
		return ErrCorrupted
	}
	return nil
}

// ReadRecordAt reads a single complete WAL record at a specific file offset.
//
// This is used by the KV Store (Phase 2) for random access to WAL records.
// Unlike Scan() which uses buffered I/O, ReadRecordAt uses os.File.ReadAt for direct offset access.
//
// Parameters:
//   - f: open file handle (must support ReadAt)
//   - offset: byte offset within the file where the record starts
//
// Returns (record, nil) on success, (empty, error) on failure.
// Errors:
//   - io.EOF: offset points past end of file
//   - io.ErrUnexpectedEOF: incomplete record (partial header/payload/checksum)
//   - ErrCorrupted: invalid magic number
//   - ErrChecksumMismatch: CRC32 validation failed
func ReadRecordAt(f *os.File, offset int64) (Record, error) {
	// Read header: Magic(4) + Timestamp(8) + Size(4) = 16 bytes
	headerBuffer := make([]byte, RecordHeaderSize)
	n, err := f.ReadAt(headerBuffer, offset)
	if err != nil {
		if err == io.EOF && n == 0 {
			return Record{}, io.EOF
		}
		if err == io.EOF || n < RecordHeaderSize {
			return Record{}, ErrTruncated
		}
		return Record{}, err
	}

	// Parse header
	magic := binary.BigEndian.Uint32(headerBuffer[0:4])
	timestampNano := binary.BigEndian.Uint64(headerBuffer[4:12])
	payloadSize := binary.BigEndian.Uint32(headerBuffer[12:16])

	// Validate magic
	if magicValidationError := validateMagic(magic); magicValidationError != nil {
		return Record{}, magicValidationError
	}

	// Read payload
	payloadBuffer := make([]byte, payloadSize)
	n, err = f.ReadAt(payloadBuffer, offset+int64(RecordHeaderSize))
	if err != nil {
		if err == io.EOF || n < int(payloadSize) {
			return Record{}, ErrTruncated
		}
		return Record{}, err
	}

	// Read checksum (4 bytes)
	checksumBuffer := make([]byte, RecordChecksumSize)
	n, err = f.ReadAt(checksumBuffer, offset+int64(RecordHeaderSize)+int64(payloadSize))
	if err != nil {
		if err == io.EOF || n < RecordChecksumSize {
			return Record{}, ErrTruncated
		}
		return Record{}, err
	}

	storedChecksum := binary.BigEndian.Uint32(checksumBuffer)

	// Verify checksum: computed over header + payload
	computedChecksum := crc32.ChecksumIEEE(headerBuffer)
	computedChecksum = crc32.Update(computedChecksum, crc32.IEEETable, payloadBuffer)

	if computedChecksum != storedChecksum {
		return Record{}, ErrChecksumMismatch
	}

	// Return valid record
	return Record{
		Timestamp: time.Unix(0, int64(timestampNano)),
		Data:      payloadBuffer,
	}, nil
}

// DecodeEvent reconstructs a domain.Event from WAL payload bytes.
// This reverses the encoding done by EncodeEvent in encode.go.
//
// Format: [SourceLen:2][Source:N][PayloadLen:2][Payload:M]
//
// Returns ErrCorrupted if the payload is malformed or truncated.
// Note: ReceivedAt must be set by caller from Record.Timestamp.
func DecodeEvent(data []byte) (*domain.Event, error) {
	if len(data) < 2 {
		return nil, ErrCorrupted
	}

	offset := 0

	// Read source length (2 bytes)
	sourceLength := binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2

	// Check bounds before reading source
	if len(data) < offset+int(sourceLength)+2 {
		return nil, ErrCorrupted
	}

	// Read source string
	sourceBytes := data[offset : offset+int(sourceLength)]
	source := string(sourceBytes)
	offset += int(sourceLength)

	// Read payload length (2 bytes)
	payloadLength := binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2

	// Check bounds before reading payload
	if len(data) < offset+int(payloadLength) {
		return nil, ErrCorrupted
	}

	// Read payload string
	payloadBytes := data[offset : offset+int(payloadLength)]
	payload := string(payloadBytes)

	return &domain.Event{
		Source:  source,
		Payload: payload,
	}, nil
}
