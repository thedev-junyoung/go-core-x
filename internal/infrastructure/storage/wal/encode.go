package wal

import (
	"github.com/junyoung/core-x/internal/domain"
)

// encodedEventSize는 Event를 인코딩했을 때의 바이트 크기를 반환한다.
//
// 형식: [SourceLen:2][Source:N][PayloadLen:2][Payload:M]
func encodedEventSize(e *domain.Event) int {
	return 2 + len(e.Source) + 2 + len(e.Payload)
}

// appendEncodeEventInto는 buf에 Event를 직접 append하여 반환한다.
//
// 형식: [SourceLen:2][Source:N][PayloadLen:2][Payload:M]
//
// Source와 Payload는 각각 최대 65535 bytes까지 지원.
// append([]byte, string...)는 Go 컴파일러가 임시 변환 없이 직접 복사 처리하므로 0 alloc이다.
func appendEncodeEventInto(buf []byte, e *domain.Event) []byte {
	// SourceLen (2 bytes, big-endian)
	srcLen := len(e.Source)
	buf = append(buf, byte(srcLen>>8), byte(srcLen))

	// Source 데이터 (string을 append로 직접 복사, 임시 변환 없음)
	buf = append(buf, e.Source...)

	// PayloadLen (2 bytes, big-endian)
	payLen := len(e.Payload)
	buf = append(buf, byte(payLen>>8), byte(payLen))

	// Payload 데이터
	buf = append(buf, e.Payload...)

	return buf
}

// EncodeEvent는 Event를 바이너리 형식으로 직렬화한다.
//
// 형식:
//   [SourceLen:2] [Source:N] [PayloadLen:2] [Payload:M]
//
// Source와 Payload는 각각 최대 65535 bytes까지 지원.
// (실무에서는 충분한 크기)
//
// 주의: 이 함수는 새 슬라이스를 반환한다.
// 고성능 경로(WriteEvent)는 appendEncodeEventInto를 사용하여 할당을 제거한다.
//
// 반환: 직렬화된 바이너리 데이터.
func EncodeEvent(e *domain.Event) []byte {
	size := encodedEventSize(e)
	buf := make([]byte, 0, size)
	buf = appendEncodeEventInto(buf, e)
	return buf
}
