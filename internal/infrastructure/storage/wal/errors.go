// Package wal provides sentinel error types for WAL Reader operations.
package wal

import "errors"

var (
	// ErrTruncated indicates a record was truncated at end of file.
	// This is expected during crash recovery and is gracefully handled.
	// Caller should treat prior records as valid and stop reading.
	ErrTruncated = errors.New("wal: record truncated at end of file")

	// ErrCorrupted indicates the WAL file's magic number is invalid.
	// This indicates filesystem damage or the file is not a valid WAL file.
	// Recovery is not possible; investigate storage integrity.
	ErrCorrupted = errors.New("wal: magic number mismatch, file may be corrupted")

	// ErrChecksumMismatch indicates a record's CRC32 checksum validation failed.
	// This indicates payload corruption and the record cannot be trusted.
	// Recovery is not possible for this record; stop reading.
	ErrChecksumMismatch = errors.New("wal: checksum mismatch")

	// ErrVersionUnsupported indicates the WAL uses a protocol version this binary doesn't understand.
	// This occurs when reading WAL files created by a newer version of Core-X.
	// Recovery: upgrade the binary and retry.
	ErrVersionUnsupported = errors.New("wal: unsupported protocol version")
)
