package replication

import (
	"os"
	"sync"
)

// Receiver appends raw WAL entries received from a primary to a local WAL file.
type Receiver struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// NewReceiver opens (or creates) the replica WAL file at path for append-only writing.
func NewReceiver(path string) (*Receiver, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &Receiver{file: f, path: path}, nil
}

// Append writes raw WAL bytes to the replica WAL file and calls fsync.
func (r *Receiver) Append(entry RawEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.file.Write(entry.RawData); err != nil {
		return err
	}
	return r.file.Sync()
}

// PersistedOffset returns the current file size (last fsync'd byte offset).
func (r *Receiver) PersistedOffset() (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	info, err := r.file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Close closes the replica WAL file.
func (r *Receiver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
