// Package wal는 순수 Go 기반 Sequential Write-Ahead Log를 구현한다.
//
// 계층 내 위치: Infrastructure Layer.
// 외부 라이브러리 없이 os 패키지만 사용하여 최고 속도의 순차 쓰기를 달성한다.
//
// 바이너리 프로토콜:
//   [Magic:4] [Timestamp:8] [Size:4] [Payload:N] [CRC32:4]
//
// 성능 특성:
//   - 단일 버퍼드 채널 없음 (동기 순차 쓰기만 지원)
//   - O_APPEND 모드로 concurrent-safe한 append 보장
//   - Sync() 정책은 설정 가능 (매 쓰기 vs 일정 시간 vs 일정 바이트 수)
package wal

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"sync"
	"time"

	"github.com/junyoung/core-x/internal/domain"
)

const (
	// MagicNumber는 WAL 파일 식별용 매직 넘버다.
	MagicNumber uint32 = 0xCAFEBABE

	// RecordHeaderSize는 헤더(Magic + Timestamp + Size) 크기.
	RecordHeaderSize = 4 + 8 + 4

	// RecordChecksumSize는 체크섬(CRC32) 크기.
	RecordChecksumSize = 4

	// RecordMinSize는 오버헤드 합계 (헤더 + 체크섬).
	RecordMinSize = RecordHeaderSize + RecordChecksumSize
)

// SyncPolicy는 fsync 호출 시점을 결정한다.
type SyncPolicy int

const (
	// SyncImmediate : 매 Write() 호출마다 fsync.
	// 장점: 최고 안전성 (power loss 시에도 데이터 보존)
	// 단점: 가장 느림 (I/O 대기)
	SyncImmediate SyncPolicy = iota

	// SyncInterval: 설정한 시간 간격마다 fsync.
	// 예: 100ms마다 — 버스트 쓰기 시 여러 레코드를 배치로 처리
	// 장점: 성능과 안전성의 균형
	// 단점: 간격 내 power loss 시 그 기간의 데이터 손실 가능
	SyncInterval

	// SyncNever: fsync 호출 안 함 (커널 버퍼에만 기록).
	// 장점: 최고 성능
	// 단점: 최소 안전성 (OS crash 시 손실 가능)
	SyncNever
)

// Config는 WAL Writer의 설정을 정의한다.
type Config struct {
	// Path: WAL 파일 경로.
	// 파일이 없으면 생성되고, 있으면 append 모드로 열린다.
	Path string

	// SyncPolicy: fsync 호출 전략.
	// 기본값: SyncInterval (100ms)
	SyncPolicy SyncPolicy

	// SyncInterval: SyncPolicy == SyncInterval일 때 fsync 호출 간격.
	// 무시됨: SyncPolicy == SyncImmediate 또는 SyncNever.
	SyncInterval time.Duration
}

// Writer는 Sequential WAL 라이터다.
//
// 필드:
//   - file: O_APPEND 모드로 열린 파일 디스크립터.
//   - mu: Write() 호출의 원자성을 보장하는 뮤텍스.
//   - cfg: 설정 (SyncPolicy, SyncInterval).
//
// SyncInterval 정책용 필드:
//   - syncDone: 백그라운드 fsync 고루틴에 종료 신호를 보내는 채널.
//   - syncWg: 백그라운드 고루틴의 완료를 기다리는 WaitGroup.
type Writer struct {
	file *os.File
	mu   sync.Mutex
	cfg  Config

	// SyncInterval 정책용 필드
	syncDone chan struct{}
	syncWg   sync.WaitGroup
}

// NewWriter는 WAL 파일을 열고 Writer를 생성한다.
//
// 파일이 없으면 새로 생성되고, 있으면 append 모드로 열린다.
// SyncInterval 정책이면 백그라운드 고루틴을 시작하여 주기적으로 fsync를 호출한다.
//
// 사전 조건: cfg.Path는 유효한 파일 경로여야 한다.
func NewWriter(cfg Config) (*Writer, error) {
	f, err := os.OpenFile(
		cfg.Path,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY,
		0600,
	)
	if err != nil {
		return nil, err
	}

	w := &Writer{
		file: f,
		cfg:  cfg,
	}

	// SyncInterval 정책이면 백그라운드 fsync 고루틴 시작.
	if cfg.SyncPolicy == SyncInterval {
		w.syncDone = make(chan struct{})
		w.startSyncTicker()
	}

	return w, nil
}

// Write는 이벤트를 바이너리 프로토콜에 따라 기록한다.
//
// 절차:
//   1. 헤더 작성: Magic + Timestamp + Size
//   2. Payload 작성
//   3. CRC32 체크섬 계산 및 작성
//   4. SyncPolicy에 따라 fsync 호출:
//      - SyncImmediate: 즉시 fsync
//      - SyncInterval: 백그라운드 고루틴이 주기적으로 처리 (Write 경로는 fsync 미포함)
//      - SyncNever: fsync 호출 안 함
//
// 동시성: mu로 보호됨 (여러 고루틴에서 동시 호출 안전).
//
// 반환: 성공 시 nil, 에러 시 오류 객체.
func (w *Writer) Write(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 헤더: Magic (4) + Timestamp (8) + Size (4)
	header := make([]byte, RecordHeaderSize)
	binary.BigEndian.PutUint32(header[0:4], MagicNumber)
	binary.BigEndian.PutUint64(header[4:12], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(header[12:16], uint32(len(payload)))

	// 전체 레코드 데이터 (체크섬 제외).
	record := make([]byte, 0, len(header)+len(payload)+RecordChecksumSize)
	record = append(record, header...)
	record = append(record, payload...)

	// CRC32 체크섬 계산.
	checksum := crc32.ChecksumIEEE(record)

	// 파일에 기록: 레코드 + 체크섬.
	if _, err := w.file.Write(record); err != nil {
		return err
	}

	checksumBuf := make([]byte, RecordChecksumSize)
	binary.BigEndian.PutUint32(checksumBuf, checksum)
	if _, err := w.file.Write(checksumBuf); err != nil {
		return err
	}

	// SyncImmediate 정책: 즉시 fsync 호출.
	if w.cfg.SyncPolicy == SyncImmediate {
		if err := w.file.Sync(); err != nil {
			return err
		}
	}
	// SyncInterval: 백그라운드 고루틴이 처리 (Write 경로는 fsync 미포함).
	// SyncNever: fsync 호출 안 함.

	return nil
}

// WriteEvent는 Event를 바이너리 프로토콜에 따라 기록한다.
//
// application/ingestion.WALWriter 포트를 구현한다.
// Event를 내부적으로 바이너리로 encode한 후 Write()를 호출한다.
func (w *Writer) WriteEvent(e *domain.Event) error {
	payload := EncodeEvent(e)
	return w.Write(payload)
}

// Sync는 명시적 fsync 호출을 강제한다.
//
// SyncNever 정책이어도 호출 가능하며, 항상 OS 버퍼를 디스크에 플러시한다.
// SyncInterval 정책에서도 호출 가능하며, 백그라운드 고루틴과 경쟁하지 않으려면
// 뮤텍스로 보호된다.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.file.Sync()
}

// Close는 WAL 라이터를 종료한다.
//
// 절차:
//   1. SyncInterval 정책이면 백그라운드 fsync 고루틴 종료 신호 전송 및 대기.
//   2. 마지막 fsync 호출 (모든 정책).
//   3. 파일 닫기.
//
// Graceful shutdown 보증:
// 이미 큐에 있는 Write() 호출들이 완료된 후 fsync가 호출된다.
// 왜냐하면 백그라운드 고루틴이 종료될 때까지 대기하고, 그 후 명시적 Sync() 호출하기 때문.
func (w *Writer) Close() error {
	// SyncInterval 정책: 백그라운드 고루틴 종료.
	if w.cfg.SyncPolicy == SyncInterval && w.syncDone != nil {
		close(w.syncDone)
		w.syncWg.Wait() // 고루틴이 종료될 때까지 대기.
	}

	// 마지막 fsync.
	if err := w.Sync(); err != nil {
		return err
	}

	// 파일 닫기.
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// startSyncTicker는 SyncInterval 정책용 백그라운드 fsync 고루틴을 시작한다.
//
// 동작:
//   - 주기적으로 ticker 신호를 받으면 Sync() 호출.
//   - syncDone 채널 닫히면 즉시 루프 종료.
//
// 뮤텍스 획득:
//   Sync() 호출 시 mu를 획득하므로 Write()와 경쟁하지 않음.
//   따라서 동시에 여러 Write()가 진행 중이어도 안전.
func (w *Writer) startSyncTicker() {
	w.syncWg.Add(1)
	go func() {
		defer w.syncWg.Done()

		ticker := time.NewTicker(w.cfg.SyncInterval)
		defer ticker.Stop()

		for {
			select {
			case <-w.syncDone:
				// 종료 신호 수신 — 고루틴 종료.
				return
			case <-ticker.C:
				// 주기 만료 — fsync 호출 (뮤텍스로 보호됨).
				_ = w.Sync() // 에러 무시 (배경 작업이므로 silent fail)
			}
		}
	}()
}
