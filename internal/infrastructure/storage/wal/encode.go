package wal

import (
	"bytes"
	"encoding/binary"

	"github.com/junyoung/core-x/internal/domain"
)

// EncodeEvent는 Event를 바이너리 형식으로 직렬화한다.
//
// 형식:
//   [SourceLen:2] [Source:N] [PayloadLen:2] [Payload:M]
//
// Source와 Payload는 각각 최대 65535 bytes까지 지원.
// (실무에서는 충분한 크기)
//
// 반환: 직렬화된 바이너리 데이터.
func EncodeEvent(e *domain.Event) []byte {
	var buf bytes.Buffer

	// Source: 길이 (2 bytes) + 데이터
	sourceBytes := []byte(e.Source)
	binary.Write(&buf, binary.BigEndian, uint16(len(sourceBytes)))
	buf.Write(sourceBytes)

	// Payload: 길이 (2 bytes) + 데이터
	payloadBytes := []byte(e.Payload)
	binary.Write(&buf, binary.BigEndian, uint16(len(payloadBytes)))
	buf.Write(payloadBytes)

	return buf.Bytes()
}
