package wal

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junyoung/core-x/internal/domain"
)

// writeTestRecord는 테스트용 WAL 레코드를 직접 파일에 작성한다.
// Writer를 사용하지 않고 저수준 바이너리 형식으로 작성하여,
// 특정 필드를 손상시킨 테스트를 수행할 수 있게 한다.
func writeTestRecord(t testing.TB, file *os.File, event *domain.Event) {
	t.Helper()

	// 이벤트 인코딩
	payload := EncodeEvent(event)

	// 헤더 작성: Magic(4) + Timestamp(8) + Size(4)
	header := make([]byte, RecordHeaderSize)
	binary.BigEndian.PutUint32(header[0:4], MagicNumber)
	binary.BigEndian.PutUint64(header[4:12], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(header[12:16], uint32(len(payload)))

	// 레코드 데이터 (체크섬 제외)
	record := make([]byte, 0, len(header)+len(payload))
	record = append(record, header...)
	record = append(record, payload...)

	// CRC32 체크섬 계산
	checksum := crc32.ChecksumIEEE(record)

	// 파일에 기록: 레코드 + 체크섬
	if _, err := file.Write(record); err != nil {
		t.Fatalf("failed to write record: %v", err)
	}

	checksumBuffer := make([]byte, RecordChecksumSize)
	binary.BigEndian.PutUint32(checksumBuffer, checksum)
	if _, err := file.Write(checksumBuffer); err != nil {
		t.Fatalf("failed to write checksum: %v", err)
	}
}

// TestReader_EmptyFile는 빈 WAL 파일이 정상적으로 처리되는지 검증한다.
func TestReader_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "empty.wal")

	// 빈 파일 생성
	file, err := os.Create(walPath)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	file.Close()

	// 리더로 열기
	reader, err := NewReader(walPath)
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	defer reader.Close()

	// 첫 번째 Scan()은 false를 반환해야 함 (EOF)
	if reader.Scan() {
		t.Fatalf("expected Scan() to return false on empty file")
	}

	// Err()는 nil을 반환해야 함 (정상적인 EOF)
	if err := reader.Err(); err != nil {
		t.Fatalf("expected nil error on empty file, got %v", err)
	}
}

// TestReader_SingleRecord는 단일 레코드를 쓰고 읽을 수 있는지 검증한다.
func TestReader_SingleRecord(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "single.wal")

	// 테스트 이벤트
	testEvent := &domain.Event{
		Source:  "test-source",
		Payload: "test-payload",
	}

	// 레코드 작성
	file, err := os.Create(walPath)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	writeTestRecord(t, file, testEvent)
	file.Close()

	// 레코드 읽기
	reader, err := NewReader(walPath)
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	defer reader.Close()

	// Scan()이 true를 반환해야 함
	if !reader.Scan() {
		t.Fatalf("expected Scan() to return true, got false")
	}

	// 레코드 검증
	record := reader.Record()
	if record.Data == nil {
		t.Fatalf("expected non-nil data")
	}

	// 디코딩
	decoded, err := DecodeEvent(record.Data)
	if err != nil {
		t.Fatalf("failed to decode event: %v", err)
	}

	// 값 검증
	if decoded.Source != testEvent.Source {
		t.Fatalf("source mismatch: expected %q, got %q", testEvent.Source, decoded.Source)
	}
	if decoded.Payload != testEvent.Payload {
		t.Fatalf("payload mismatch: expected %q, got %q", testEvent.Payload, decoded.Payload)
	}

	// 다음 Scan()은 false를 반환해야 함
	if reader.Scan() {
		t.Fatalf("expected second Scan() to return false (EOF)")
	}

	// 에러는 nil이어야 함
	if err := reader.Err(); err != nil {
		t.Fatalf("expected nil error at EOF, got %v", err)
	}
}

// TestReader_MultipleRecords는 여러 레코드를 순차적으로 읽을 수 있는지 검증한다.
func TestReader_MultipleRecords(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "multiple.wal")

	// 테스트 이벤트 배열
	testEvents := []*domain.Event{
		{Source: "source1", Payload: "payload1"},
		{Source: "source2", Payload: "payload2"},
		{Source: "source3", Payload: "payload3"},
	}

	// 레코드 작성
	file, err := os.Create(walPath)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	for _, event := range testEvents {
		writeTestRecord(t, file, event)
	}
	file.Close()

	// 레코드 읽기
	reader, err := NewReader(walPath)
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	defer reader.Close()

	// 모든 레코드 검증
	recordCount := 0
	for reader.Scan() {
		record := reader.Record()
		decoded, err := DecodeEvent(record.Data)
		if err != nil {
			t.Fatalf("failed to decode event %d: %v", recordCount, err)
		}

		// 예상된 이벤트와 비교
		expectedEvent := testEvents[recordCount]
		if decoded.Source != expectedEvent.Source {
			t.Fatalf("record %d source mismatch: expected %q, got %q",
				recordCount, expectedEvent.Source, decoded.Source)
		}
		if decoded.Payload != expectedEvent.Payload {
			t.Fatalf("record %d payload mismatch: expected %q, got %q",
				recordCount, expectedEvent.Payload, decoded.Payload)
		}

		recordCount++
	}

	// 에러 확인
	if err := reader.Err(); err != nil {
		t.Fatalf("expected nil error at EOF, got %v", err)
	}

	// 읽은 레코드 개수 검증
	if recordCount != len(testEvents) {
		t.Fatalf("expected %d records, got %d", len(testEvents), recordCount)
	}
}

// TestReader_TailTruncation는 파일 끝에서 레코드가 잘렸을 때 복구를 검증한다.
func TestReader_TailTruncation(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "truncated.wal")

	// 100개 레코드 작성
	file, err := os.Create(walPath)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	testEvent := &domain.Event{
		Source:  "complete",
		Payload: "record",
	}
	for i := 0; i < 100; i++ {
		writeTestRecord(t, file, testEvent)
	}
	file.Close()

	// 파일 끝에서 10바이트 자르기
	file, err = os.OpenFile(walPath, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("failed to open file for truncation: %v", err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("failed to stat file: %v", err)
	}
	if err := file.Truncate(info.Size() - 10); err != nil {
		t.Fatalf("failed to truncate file: %v", err)
	}
	file.Close()

	// 레코더 열기 및 읽기
	reader, err := NewReader(walPath)
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	defer reader.Close()

	// 완전한 레코드 복구
	recoveredCount := 0
	for reader.Scan() {
		recoveredCount++
	}

	// 99개 레코드 복구 확인 (마지막 레코드는 잘려서 복구 불가)
	if recoveredCount != 99 {
		t.Fatalf("expected 99 recovered records, got %d", recoveredCount)
	}

	// ErrTruncated 에러 확인
	if err := reader.Err(); err == nil || err != ErrTruncated {
		t.Fatalf("expected ErrTruncated, got %v", err)
	}
}

// TestReader_HeaderTruncation는 헤더가 불완전할 때 ErrTruncated를 반환하는지 검증한다.
func TestReader_HeaderTruncation(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "header_truncated.wal")

	// 2바이트만 포함하는 파일 생성 (헤더는 16바이트가 필요)
	file, err := os.Create(walPath)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	file.Write([]byte{0x00, 0x01})
	file.Close()

	// 리더로 열기
	reader, err := NewReader(walPath)
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	defer reader.Close()

	// Scan()은 false를 반환해야 함
	if reader.Scan() {
		t.Fatalf("expected Scan() to return false on truncated header")
	}

	// ErrTruncated 확인
	if err := reader.Err(); err != ErrTruncated {
		t.Fatalf("expected ErrTruncated, got %v", err)
	}
}

// TestReader_CorruptedMagic는 매직 넘버가 잘못되었을 때 ErrCorrupted를 반환하는지 검증한다.
func TestReader_CorruptedMagic(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "corrupted_magic.wal")

	// 손상된 매직 넘버로 레코드 생성
	file, err := os.Create(walPath)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	// 잘못된 매직으로 헤더 작성
	payload := []byte("test-payload")
	header := make([]byte, RecordHeaderSize)
	binary.BigEndian.PutUint32(header[0:4], 0xDEADBEEF) // 잘못된 매직
	binary.BigEndian.PutUint64(header[4:12], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(header[12:16], uint32(len(payload)))

	// 레코드 및 체크섬 작성
	record := make([]byte, 0, len(header)+len(payload))
	record = append(record, header...)
	record = append(record, payload...)
	checksum := crc32.ChecksumIEEE(record)

	file.Write(record)
	checksumBuffer := make([]byte, RecordChecksumSize)
	binary.BigEndian.PutUint32(checksumBuffer, checksum)
	file.Write(checksumBuffer)
	file.Close()

	// 리더로 열기
	reader, err := NewReader(walPath)
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	defer reader.Close()

	// Scan()은 false를 반환해야 함
	if reader.Scan() {
		t.Fatalf("expected Scan() to return false on corrupted magic")
	}

	// ErrCorrupted 확인
	if err := reader.Err(); err != ErrCorrupted {
		t.Fatalf("expected ErrCorrupted, got %v", err)
	}
}

// TestReader_ChecksumMismatch는 체크섬 불일치 시 ErrChecksumMismatch를 반환하는지 검증한다.
func TestReader_ChecksumMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	walPath := filepath.Join(tmpDir, "checksum_mismatch.wal")

	// 올바른 레코드 작성
	file, err := os.Create(walPath)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	payload := []byte("test-payload")
	header := make([]byte, RecordHeaderSize)
	binary.BigEndian.PutUint32(header[0:4], MagicNumber)
	binary.BigEndian.PutUint64(header[4:12], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(header[12:16], uint32(len(payload)))

	record := make([]byte, 0, len(header)+len(payload))
	record = append(record, header...)
	record = append(record, payload...)
	checksum := crc32.ChecksumIEEE(record)

	// 올바른 체크섬을 1씩 증가시켜 손상시킴
	corruptedChecksum := checksum + 1

	file.Write(record)
	checksumBuffer := make([]byte, RecordChecksumSize)
	binary.BigEndian.PutUint32(checksumBuffer, corruptedChecksum)
	file.Write(checksumBuffer)
	file.Close()

	// 리더로 열기
	reader, err := NewReader(walPath)
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	defer reader.Close()

	// Scan()은 false를 반환해야 함
	if reader.Scan() {
		t.Fatalf("expected Scan() to return false on checksum mismatch")
	}

	// ErrChecksumMismatch 확인
	if err := reader.Err(); err != ErrChecksumMismatch {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
}

// TestDecodeEvent_RoundTrip는 EncodeEvent → DecodeEvent 라운드트립을 검증한다.
func TestDecodeEvent_RoundTrip(t *testing.T) {
	tests := []struct {
		name          string
		originalEvent *domain.Event
	}{
		{
			name: "NormalEvent",
			originalEvent: &domain.Event{
				Source:  "test-source",
				Payload: "test-payload",
			},
		},
		{
			name: "EmptySource",
			originalEvent: &domain.Event{
				Source:  "",
				Payload: "payload-data",
			},
		},
		{
			name: "EmptyPayload",
			originalEvent: &domain.Event{
				Source:  "source-data",
				Payload: "",
			},
		},
		{
			name: "BothEmpty",
			originalEvent: &domain.Event{
				Source:  "",
				Payload: "",
			},
		},
		{
			name: "LargePayload",
			originalEvent: &domain.Event{
				Source:  "source",
				Payload: string(bytes.Repeat([]byte("x"), 10000)),
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(subTest *testing.T) {
			// 인코딩
			encodedBytes := EncodeEvent(testCase.originalEvent)

			// 디코딩
			decodedEvent, err := DecodeEvent(encodedBytes)
			if err != nil {
				subTest.Fatalf("failed to decode event: %v", err)
			}

			// 검증: Source
			if decodedEvent.Source != testCase.originalEvent.Source {
				subTest.Fatalf("source mismatch: expected %q, got %q",
					testCase.originalEvent.Source, decodedEvent.Source)
			}

			// 검증: Payload
			if decodedEvent.Payload != testCase.originalEvent.Payload {
				subTest.Fatalf("payload mismatch: expected %q (len=%d), got %q (len=%d)",
					testCase.originalEvent.Payload, len(testCase.originalEvent.Payload),
					decodedEvent.Payload, len(decodedEvent.Payload))
			}
		})
	}
}

// TestDecodeEvent_EdgeCases는 DecodeEvent의 경계 조건을 검증한다.
func TestDecodeEvent_EdgeCases(t *testing.T) {
	tests := []struct {
		name            string
		corruptedData   []byte
		expectedErr     error
	}{
		{
			name:          "EmptyData",
			corruptedData: []byte{},
			expectedErr:   ErrCorrupted,
		},
		{
			name:          "TruncatedSourceLength",
			corruptedData: []byte{0x00},
			expectedErr:   ErrCorrupted,
		},
		{
			name:          "TruncatedSource",
			corruptedData: []byte{0x00, 0x05, 0x41, 0x42}, // 5바이트 요구, 2바이트만 제공
			expectedErr:   ErrCorrupted,
		},
		{
			name:          "TruncatedPayloadLength",
			corruptedData: []byte{0x00, 0x02, 0x41, 0x42}, // source 2바이트, 그 다음 payloadLen 없음
			expectedErr:   ErrCorrupted,
		},
		{
			name:          "TruncatedPayload",
			corruptedData: []byte{0x00, 0x02, 0x41, 0x42, 0x00, 0x05, 0x43}, // payload 5바이트 요구, 1바이트만 제공
			expectedErr:   ErrCorrupted,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(subTest *testing.T) {
			_, err := DecodeEvent(testCase.corruptedData)
			if err != testCase.expectedErr {
				subTest.Fatalf("expected %v, got %v", testCase.expectedErr, err)
			}
		})
	}
}
