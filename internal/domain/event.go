// Package domain은 Core-X의 핵심 비즈니스 엔티티와 계약을 정의한다.
//
// 계층 내 위치: 최하위 도메인 계층 (Domain Layer).
// 이 패키지는 stdlib 외 어떤 것도 import하지 않는다.
//
// 외부 의존성:
//   - 알고 있는 것: 없음 (순수 도메인 타입만 존재)
//   - 모르는 것: HTTP, sync.Pool, 채널, 영속성 — 일체 없음
//
// 변경 파급 범위: 이 패키지의 모든 수정은 모든 계층에 전파된다.
// 따라서 최소화·안정성이 최우선이다. 새로운 필드나 인터페이스 추가 시
// 모든 계층의 영향을 반드시 검토해야 한다.
package domain

import "time"

// Event 는 수집 파이프라인을 흐르는 단일 인바운드 데이터 단위다.
//
// 메모리 레이아웃 최적화: 필드를 크기 내림차순으로 정렬해 구조체 패딩을 최소화했다.
//
//	time.Time (24 bytes) → Source string (16 bytes) → Payload string (16 bytes)
//	합계: 56 bytes, 패딩 낭비 없음.
//
// 왜 이 순서가 중요한가?
// 채널 버퍼에 수백만 개의 Event 포인터가 동시에 대기할 수 있다.
// 잘못 정렬된 레이아웃은 숨겨진 메모리 오버헤드를 유발하고,
// 캐시라인 경계를 넘나드는 접근으로 L1/L2 캐시 미스를 증가시킨다.
//
// Phase 2 확장: WAL 쓰기용 시퀀스 번호(uint64)를 추가할 경우
// ReceivedAt 앞에 배치해 정렬을 유지해야 한다.
type Event struct {
	ReceivedAt time.Time
	Source     string
	Payload    string
}

// Reset 은 Event를 풀에 반환하기 전에 모든 필드를 초기화한다.
//
// 왜 Reset이 pool.Release() 내부에서 호출되는가?
// 호출 책임을 Release에 집중시켜 "반환 전 초기화" 불변식을 중앙에서 강제한다.
// 각 caller가 개별적으로 Reset을 호출하도록 하면, 하나라도 빠뜨렸을 때
// 이전 요청의 데이터가 다음 요청으로 유출되는 보안 버그가 발생한다.
//
// 보안 보증: 이전 요청의 Source/Payload가 다음 요청으로 누출되지 않음을 보장한다.
// sync.Pool은 GC 이전에 객체를 재사용하므로, 초기화 없이 반환하면
// 크로스-테넌트 데이터 유출(data exfiltration via pool reuse)이 발생한다.
//
// Exported (unexported reset()가 아닌 이유): infrastructure/pool 계층이
// domain 내부를 import하지 않고도 호출할 수 있어야 하기 때문이다.
func (e *Event) Reset() {
	e.ReceivedAt = time.Time{}
	e.Source = ""
	e.Payload = ""
}

// EventProcessor 는 Event를 소비하는 모든 구현체가 따르는 계약이다.
//
// 왜 인터페이스인가?
// Phase 1: 구조화된 로그 출력 (cmd/main.go의 func 리터럴)
// Phase 2: WAL 라이터 (이 인터페이스의 새 구현체)
// 이 인터페이스 덕분에 executor.WorkerPool의 코드는 Phase 간에 변경되지 않는다.
// 오직 cmd/main.go의 Processor 필드 한 줄만 교체하면 된다.
//
// 인터페이스 디스패치 비용: itab 조회 ~2 ns.
// HTTP 수락 경로가 아닌 worker 고루틴 내부에서 호출되므로 허용 가능하다.
// 프로파일링에서 핫스팟으로 표시되면 executor에 concrete 타입을 인라인하라.
//
// 계약 (Processor 구현체가 반드시 지켜야 할 것):
//   - goroutine-safe해야 한다 (N개의 worker가 동시에 호출).
//   - Process() 반환 후 *Event를 보유(retain)해서는 안 된다.
//     pool이 즉시 회수하므로 참조를 들고 있으면 use-after-free 버그가 발생한다.
type EventProcessor interface {
	Process(e *Event)
}

// EventProcessorFunc 은 일반 함수를 EventProcessor 인터페이스로 적응시키는 어댑터다.
// 왜 이 패턴을 사용하는가?
// 별도의 named struct를 정의하지 않고도 func 리터럴이 EventProcessor를 구현할 수 있다.
// stdlib의 http.HandlerFunc 패턴과 동일하다 — Go 관용구로 잘 알려져 있어
// 새로운 개발자도 즉시 의도를 파악할 수 있다.
// cmd/main.go의 Phase 1 log-only 프로세서가 이 방식으로 주입된다.
type EventProcessorFunc func(e *Event)

// Process 는 EventProcessor를 구현한다.
func (f EventProcessorFunc) Process(e *Event) { f(e) }
