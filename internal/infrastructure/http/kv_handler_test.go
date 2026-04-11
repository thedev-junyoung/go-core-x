package http_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/domain"
	infrahttp "github.com/junyoung/core-x/internal/infrastructure/http"
	kvstore "github.com/junyoung/core-x/internal/infrastructure/storage/kv"
)

type stubKVStore struct {
	event *domain.Event
	err   error
}

func (s *stubKVStore) Get(key string) (*domain.Event, error) {
	return s.event, s.err
}

func TestKVHandler_Get_Found(t *testing.T) {
	store := &stubKVStore{
		event: &domain.Event{
			Source:     "src1",
			Payload:    "hello",
			ReceivedAt: time.Unix(0, 1234567890),
		},
	}
	h := infrahttp.NewKVHandler(store, nil, "", nil)
	req := httptest.NewRequest(http.MethodGet, "/kv/src1", nil)
	req.SetPathValue("key", "src1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp["source"] != "src1" {
		t.Fatalf("expected source=src1, got %v", resp["source"])
	}
	if resp["payload"] != "hello" {
		t.Fatalf("expected payload=hello, got %v", resp["payload"])
	}
}

func TestKVHandler_Get_NotFound(t *testing.T) {
	store := &stubKVStore{err: kvstore.ErrKeyNotFound}
	h := infrahttp.NewKVHandler(store, nil, "", nil)
	req := httptest.NewRequest(http.MethodGet, "/kv/missing", nil)
	req.SetPathValue("key", "missing")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestKVHandler_Get_EmptyKey(t *testing.T) {
	store := &stubKVStore{}
	h := infrahttp.NewKVHandler(store, nil, "", nil)
	req := httptest.NewRequest(http.MethodGet, "/kv/", nil)
	req.SetPathValue("key", "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// Ensure stubKVStore satisfies the interface at compile time.
var _ infrahttp.KVStoreGetter = (*stubKVStore)(nil)

// Ensure errors.Is works as expected for ErrKeyNotFound.
func TestErrKeyNotFound_Is(t *testing.T) {
	if !errors.Is(kvstore.ErrKeyNotFound, kvstore.ErrKeyNotFound) {
		t.Fatal("errors.Is must match sentinel")
	}
}
