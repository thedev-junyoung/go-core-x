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
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
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
//   - bufPool: Write() 경로의 record 버퍼 재사용 (zero-alloc).
//   - sizeTracker: 파일의 누적 쓰기 바이트 수. atomic으로 관리.
//
// SyncInterval 정책용 필드:
//   - syncDone: 백그라운드 fsync 고루틴에 종료 신호를 보내는 채널.
//   - syncWg: 백그라운드 고루틴의 완료를 기다리는 WaitGroup.
type Writer struct {
	file *os.File
	mu   sync.Mutex
	cfg  Config

	// bufPool은 Write() 경로의 record 버퍼를 재사용한다.
	// Get()으로 *[]byte를 꺼내고 사용 후 Put()으로 반환한다.
	bufPool sync.Pool

	// sizeTracker는 파일에 누적된 바이트 수를 추적한다.
	// Phase 2 KV Store의 offset 계산에 사용된다.
	// 값은 O_APPEND 쓰기 후 누산되므로, 다음 쓰기의 offset을 미리 알 수 있다.
	sizeTracker atomic.Int64

	// SyncInterval 정책용 필드
	syncDone chan struct{}
	syncWg   sync.WaitGroup

	// compactionCh is closed by RunExclusiveSwap to notify listeners (e.g. replication
	// streamer) that the WAL file has been atomically swapped. A new channel is
	// assigned after each swap.
	compactionCh chan struct{}
	compactionMu sync.Mutex
}

// NewWriter는 WAL 파일을 열고 Writer를 생성한다.
//
// 파일이 없으면 새로 생성되고, 있으면 append 모드로 열린다.
// SyncInterval 정책이면 백그라운드 고루틴을 시작하여 주기적으로 fsync를 호출한다.
// 기존 파일의 크기를 sizeTracker로 초기화하여 offset 추적을 시작한다.
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

	// 기존 파일 크기로 sizeTracker 초기화
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	w := &Writer{
		file:         f,
		cfg:          cfg,
		compactionCh: make(chan struct{}),
		bufPool: sync.Pool{
			New: func() any {
				// 초기 용량: 헤더(16) + 일반적인 페이로드(256) + 체크섬(4)
				// 실제 payload가 크면 Write() 내에서 grow된다.
				b := make([]byte, 0, RecordHeaderSize+256+RecordChecksumSize)
				return &b  // *[]byte 저장 (boxing alloc 방지)
			},
		},
	}

	// sizeTracker를 기존 파일 크기로 초기화
	w.sizeTracker.Store(stat.Size())

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
//   1. 헤더 작성: Magic + Timestamp + Size (스택 배열, 0 alloc)
//   2. Payload 작성
//   3. CRC32 체크섬 계산 (pool 버퍼 사용, 0 alloc on pool hit)
//   4. SyncPolicy에 따라 fsync 호출:
//      - SyncImmediate: 즉시 fsync
//      - SyncInterval: 백그라운드 고루틴이 주기적으로 처리 (Write 경로는 fsync 미포함)
//      - SyncNever: fsync 호출 안 함
//
// 성능: SyncNever 정책일 때 pool 히트 시 0 allocs/op.
//
// 동시성: mu로 보호됨 (여러 고루틴에서 동시 호출 안전).
//
// 반환: 성공 시 nil, 에러 시 오류 객체.
func (w *Writer) Write(payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 헤더: [Magic:4][Timestamp:8][Size:4] = 16 bytes (스택 배열)
	var header [RecordHeaderSize]byte
	binary.BigEndian.PutUint32(header[0:4], MagicNumber)
	binary.BigEndian.PutUint64(header[4:12], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(header[12:16], uint32(len(payload)))

	// record 버퍼: pool에서 획득
	bp := w.bufPool.Get().(*[]byte)
	buf := (*bp)[:0] // 길이를 0으로 리셋, 용량은 유지

	// 필요한 크기 확인
	needed := RecordHeaderSize + len(payload) + RecordChecksumSize
	if cap(buf) < needed {
		// 용량 부족 시 grow (이 경우만 alloc 발생)
		buf = make([]byte, 0, needed)
	}

	// 헤더 + 페이로드를 버퍼에 누적
	buf = append(buf, header[:]...)
	buf = append(buf, payload...)

	// CRC32 체크섬: 헤더 + 페이로드 범위에 대해 계산
	checksum := crc32.ChecksumIEEE(buf)

	// 체크섬: 고정 4B 스택 배열
	var checksumBytes [RecordChecksumSize]byte
	binary.BigEndian.PutUint32(checksumBytes[0:4], checksum)
	buf = append(buf, checksumBytes[:]...)

	// Single Write syscall (이전: 2회 → 현재: 1회)
	_, err := w.file.Write(buf)

	// pool에 버퍼 반환 (에러 여부와 관계없이)
	*bp = buf
	w.bufPool.Put(bp)

	if err != nil {
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

// WriteEventOffset는 Event를 바이너리 프로토콜에 따라 기록하고 offset을 반환한다.
//
// Write 직전의 파일 offset을 반환한다. Phase 2 KV Store의 인덱싱에 사용된다.
// offset은 다음 번 Write의 시작 위치를 의미한다.
//
// Event를 pool 버퍼에 직접 encode하여 EncodeEvent() 중간 할당을 제거한다.
//
// 성능: 0 allocs/op (pool 히트 시, SyncNever 정책)
// 반환: (write 직전의 offset, error)
func (w *Writer) WriteEventOffset(e *domain.Event) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 인코딩된 이벤트의 크기 사전 계산
	payloadSize := encodedEventSize(e)

	// 헤더: [Magic:4][Timestamp:8][Size:4] (스택 배열)
	var header [RecordHeaderSize]byte
	binary.BigEndian.PutUint32(header[0:4], MagicNumber)
	binary.BigEndian.PutUint64(header[4:12], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint32(header[12:16], uint32(payloadSize))

	// record 버퍼: pool에서 획득
	bp := w.bufPool.Get().(*[]byte)
	buf := (*bp)[:0]

	needed := RecordHeaderSize + payloadSize + RecordChecksumSize
	if cap(buf) < needed {
		buf = make([]byte, 0, needed)
	}

	// 헤더 + 이벤트 데이터를 버퍼에 누적
	buf = append(buf, header[:]...)
	buf = appendEncodeEventInto(buf, e) // 0 alloc (pool 내부 encoding)

	// CRC32 체크섬
	checksum := crc32.ChecksumIEEE(buf)

	// 체크섬 append (스택 배열)
	var checksumBytes [RecordChecksumSize]byte
	binary.BigEndian.PutUint32(checksumBytes[0:4], checksum)
	buf = append(buf, checksumBytes[:]...)

	// Write 직전의 offset 캡처
	offset := w.sizeTracker.Load()

	// Single Write syscall
	_, err := w.file.Write(buf)

	// pool에 반환
	*bp = buf
	w.bufPool.Put(bp)

	if err != nil {
		return 0, err
	}

	// sizeTracker 업데이트: 쓰인 바이트 수 추가
	w.sizeTracker.Add(int64(len(buf)))

	// SyncImmediate 정책
	if w.cfg.SyncPolicy == SyncImmediate {
		if err := w.file.Sync(); err != nil {
			return offset, err
		}
	}

	return offset, nil
}

// WriteEvent는 Event를 바이너리 프로토콜에 따라 기록한다.
//
// application/ingestion.WALWriter 포트를 구현한다.
// 내부적으로 WriteEventOffset을 호출하고 offset을 버린다 (하위호환).
//
// 성능: 0 allocs/op (pool 히트 시, SyncNever 정책)
func (w *Writer) WriteEvent(e *domain.Event) error {
	_, err := w.WriteEventOffset(e)
	return err
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

// CompactionNotify returns a channel that is closed when RunExclusiveSwap
// completes a WAL file swap. A new channel is issued after each swap, so
// callers must re-call CompactionNotify after receiving the signal.
func (w *Writer) CompactionNotify() <-chan struct{} {
	w.compactionMu.Lock()
	defer w.compactionMu.Unlock()
	return w.compactionCh
}

// RunExclusiveSwap pauses all writes and runs fn under the writer's lock.
//
// fn must return (newFile, newSize, error). On success, the writer atomically
// replaces its underlying file handle with newFile and resets sizeTracker to
// newSize. The old file is closed.
//
// Used by kv.Store.Compact() to atomically replace the WAL file while
// blocking concurrent writes for the duration of compaction.
func (w *Writer) RunExclusiveSwap(fn func() (*os.File, int64, error)) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	newFile, newSize, err := fn()
	if err != nil {
		return err
	}

	// Signal compaction listeners and replace the channel.
	w.compactionMu.Lock()
	oldCh := w.compactionCh
	w.compactionCh = make(chan struct{})
	w.compactionMu.Unlock()
	close(oldCh)

	old := w.file
	w.file = newFile
	w.sizeTracker.Store(newSize)
	return old.Close()
}

// startSyncTicker는 SyncInterval 정책용 백그라운드 fsync 고루틴을 시작한다.
//
// 동작:
//   - 주기적으로 ticker 신호를 받으면 Sync() 호출.
//   - syncDone 채널 닫히면 즉시 루프 종료.
//   - fsync 실패 시 slog.Error로 기록 (운영 가시성 확보).
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
				if err := w.Sync(); err != nil {
					// 배경 고루틴에서 fsync 실패 — 운영자에게 가시적으로 알림.
					// 에러가 발생해도 고루틴은 계속 실행한다 (self-healing).
					// 반복적 실패는 스토리지 장애를 의미하므로 모니터링 시스템이 감지해야 한다.
					slog.Error("wal: background fsync failed",
						"path", w.cfg.Path,
						"error", err,
					)
				}
			}
		}
	}()
}
