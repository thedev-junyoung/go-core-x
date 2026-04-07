// Package pool은 sync.Pool을 사용해 EventPool 포트를 구현한다.
//
// 계층 내 위치: Infrastructure Layer.
// 이 패키지만이 sync.Pool 내부를 알고 있다.
// 모든 상위 계층은 application/ingestion.EventPool 인터페이스를 통해 상호작용하며,
// sync.Pool의 존재를 직접 알지 못한다.
//
// 외부 의존성:
//   - 알고 있는 것: sync (stdlib), domain.Event
//   - 모르는 것: HTTP, 채널, executor — 오직 객체 재활용에만 집중한다.
//
// 왜 channel-based free list가 아닌 sync.Pool인가?
//   - sync.Pool은 GC-aware하다: GC 실행 시 풀이 드레인되어 메모리 압력이 완화된다.
//   - channel-based 풀은 객체 참조를 영구적으로 보유한다.
//     트래픽이 낮은 야간에는 N개의 Event 객체가 GC되지 않고 메모리에 남는다.
//   - 트레이드오프: GC 사이클 사이에 풀 객체가 수거될 수 있으므로 히트가 보장되지 않는다.
//     New 함수가 폴백으로 정확히 한 번 할당한다.
//     Phase 1 목표(sustained load)에서 히트율은 거의 100%에 수렴한다.
package pool

import (
	"sync"

	"github.com/junyoung/core-x/internal/domain"
)

// EventPool 은 타입 안전한 domain.Event 재활용을 위해 sync.Pool을 래핑한다.
//
// 왜 sync.Pool을 직접 노출하지 않고 래핑하는가?
// sync.Pool의 Get()은 any를 반환해 타입 단언이 필요하다.
// 이 래퍼는 타입 단언을 한 곳에 격리해 caller 코드를 단순하게 유지한다.
// 또한 Reset() 호출 책임을 Release에 집중시켜 불변식을 강제한다.
//
// 구현 인터페이스: application/ingestion.EventPool
// 공개 상태 없음: 모든 동작은 Acquire/Release 메서드를 통해서만 이루어진다.
type EventPool struct {
	p sync.Pool
}

// New 는 사용 가능한 상태의 EventPool을 생성한다.
//
// New 함수의 역할:
// sync.Pool이 객체를 보유하지 않을 때(풀 미스, GC 후) 호출되는 폴백 생성자다.
// 이 경로에서만 힙 할당이 발생한다. sustained load에서는 이 경로가 거의 호출되지 않는다.
func New() *EventPool {
	return &EventPool{
		p: sync.Pool{
			New: func() any {
				return &domain.Event{}
			},
		},
	}
}

// Acquire 는 풀에서 Event를 가져온다.
//
// 풀 미스 시 New 함수를 통해 새 Event를 할당한다.
//
// 사전 조건: 없음.
// 사후 조건: 반환된 Event는 Reset()된 상태다 (Release에서 보장).
//
// 계약 (caller가 반드시 지켜야 할 것):
// 반드시 Release를 호출해야 한다. 누출 시 할당 압력이 증가하고
// 결국 GC 빈도가 높아져 전체 처리량이 저하된다.
func (ep *EventPool) Acquire() *domain.Event {
	return ep.p.Get().(*domain.Event) // 타입 단언: New 함수가 항상 *domain.Event를 반환하므로 안전
}

// Release 는 Event를 초기화한 뒤 풀에 반환한다.
//
// 왜 Reset()을 caller가 아닌 여기서 호출하는가?
// "반환 전 초기화" 불변식을 단일 진입점에서 강제하기 위해서다.
// 여러 caller가 Reset을 각자 호출하도록 하면, 하나라도 누락했을 때
// 이전 요청의 Source/Payload가 다음 요청으로 유출된다.
// 이는 정확성 버그인 동시에 보안 취약점이다.
//
// nil 포인터 전달은 프로그래밍 오류이며 의도적으로 패닉을 발생시킨다.
// 이렇게 해야 프로덕션이 아닌 테스트에서 즉시 드러난다.
func (ep *EventPool) Release(e *domain.Event) {
	e.Reset()       // Put 전에 초기화: 크로스-요청 데이터 유출 방지
	ep.p.Put(e)
}
