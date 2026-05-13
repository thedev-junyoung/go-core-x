# Core-X Study — Question Bank

Curated questions per phase/ADR. Format:

```
### Q<id> [difficulty] <topic>
**Q**: (질문)
**Ground truth**: (코드/ADR 위치 + 핵심 포인트)
**Common wrong answers**: (자주 나오는 오답 패턴, 채점 시 참고)
```

---

## §Core — 9 Baseline Questions

These 9 are the "can you own this project" baseline. `/study core` asks all 9 in one session.

### Q-CORE-1 [easy] Raft 선택 근거
**Q**: 분산 합의 알고리즘 중 왜 Raft를 골랐어? Paxos / Zab과 비교해서.
**Ground truth**:
- Raft = "understandability for the sake of understandability" (원논문 §1)
- Paxos: 정확하지만 implementation gap 크고, multi-paxos는 사실상 다른 알고리즘
- Zab: ZooKeeper 전용, 일반화 어려움
- Raft는 leader-based + log replication을 명시적 단계로 분리 → 학습/구현 최적
- ADR-010 (Raft Leader Election) 참고
**Common wrong answers**:
- "Raft가 가장 빠르니까" — 성능 비교는 핵심이 아님
- "Paxos는 안 됨" — Paxos도 정확함. 이해/구현 난이도 차이가 본질

### Q-CORE-2 [easy] Phase 순서의 논리
**Q**: Phase 1 → 13 순서를 왜 이 순서로 잡았는지 본인 말로 30초 안에.
**Ground truth**:
- Phase 1-2: 단일 노드 성능/내구성 (HTTP + WAL + Bitcask)
- Phase 3-4: 수평 확장의 기초 (Consistent Hashing + gRPC)
- Phase 5-9: 분산 합의 정확성 (Raft 전체)
- Phase 10-12: Raft "올바르게 사용" (Linearizable, Joint Consensus, Storage Unification)
- Phase 13-14: 동시성 + 전파 (MVCC, CDC)
- 핵심 원칙: **각 phase는 이전 phase의 가정 위에 올라간다.** 분산 (Phase 3)을 단일 노드 내구성 (Phase 2) 없이 만들 수 없음
- `docs/v2/README.md`, `docs/v3/README.md` 참고

### Q-CORE-3 [medium] MVCC 도입 비용
**Q**: Phase 13 MVCC + CAS 도입했을 때 어떤 비용을 받아들였어?
**Ground truth**:
- 쓰기 +10% latency (`docs/PHASE13_MVCC_BENCHMARK.md`: 377ns → 416ns)
- 메모리 +21% (336B → 408B) — 키당 버전 슬라이스
- CAS 경로 +65% (~690ns) — JSON unmarshal + version check dominant
- 읽기 +70% (6→10ns) — 2단계 lookup (`latest[key]` → `versions[key][last]`)
- ADR-022 (MVCC) §Trade-offs
**Common wrong answers**:
- 숫자 없이 추상적 답 ("느려졌어요") — 수치를 답할 수 있어야 함

### Q-CORE-4 [hard] ADR-022 의사결정 30초 요약
**Q**: ADR-022 (MVCC)를 모르는 동료에게 30초로 설명해봐. 무엇을 풀고, 어떻게 풀었고, 어떤 trade-off를 감수했는지.
**Ground truth**: ADR-022 §Decision + §Trade-off Analysis
- 문제: 동시 read-modify-write의 lost update
- 해결: 버전 슬라이스 + CAS (latest[key] == ExpectedVersion일 때만 쓰기)
- 비용: 메모리, latency, retention 관리
- 대안: pessimistic locking (deadlock 위험, throughput 손해)
**Common wrong answers**:
- "MVCC는 PostgreSQL이 쓰는 거예요" — 비교만 하고 본인 구현 설명 못 함

### Q-CORE-5 [hard] DDIA 매핑
**Q**: DDIA Ch.5 (Replication), Ch.7 (Transactions), Ch.9 (Consistency) 각각 코드 어디에 풀려있는지.
**Ground truth**:
- Ch.5: `internal/infrastructure/raft/replication.go` (Raft log replication), `internal/infrastructure/replication/` (async WAL streaming)
- Ch.7: `internal/infrastructure/raft/mvcc_state_machine.go` (MVCC), `kv_state_machine.go` (basic apply)
- Ch.9: `internal/infrastructure/raft/node.go` (ReadIndex, Lease Read — ADR-019)
**Common wrong answers**:
- 챕터를 코드로 짚지 못하고 책 내용만 reciting

### Q-CORE-6 [medium] 벤치 결과 해석
**Q**: PHASE13_MVCC_BENCHMARK에서 CAS success가 687ns고, MVCC unconditional이 416ns야. 그 차이 ~271ns의 정체는 뭐야?
**Ground truth**:
- BenchmarkMVCC_CASSuccess는 ExpectedVersion이 매 iteration마다 달라야 해서 JSON marshal이 hot loop에 포함됨
- ~80ns marshal + ~190ns version check + map insert
- `docs/PHASE13_MVCC_BENCHMARK.md` §Analysis §3
**Common wrong answers**:
- "CAS가 더 일을 해서요" — 구체적으로 무엇을 하는지 답해야 함

### Q-CORE-7 [hard] Election timeout jitter
**Q**: Raft election timeout에 jitter (randomization)가 왜 필요해? 안 넣으면 무슨 일이 벌어져?
**Ground truth**:
- 여러 follower가 동시에 candidate로 전환 → 표가 갈려서 split vote → 아무도 quorum 못 얻음 → 다시 timeout → 무한 반복
- jitter로 한 노드가 먼저 시작할 확률 ↑ → quorum 확보 가능성 ↑
- `internal/infrastructure/raft/node.go`의 `randomElectionTimeout()` 참고
- 원논문 §5.2
**Common wrong answers**:
- "성능 때문에" — correctness 문제임
- "랜덤성이 좋아서" — 구체적 mechanism 설명 못 함

### Q-CORE-8 [hard] 직접 디버깅한 버그 1개
**Q**: Core-X 빌드하다가 본인이 손으로 잡은 버그 1개 설명해봐. (정직: 없으면 "없음"으로 답)
**Ground truth**: N/A — 정직성 체크 질문
- "없음" = 정직 = 다음 학습 단계가 명확 (실제 디버깅 경험 만들기)
- 있으면 → 버그 원인 / 진단 / 수정 흐름을 명확히 설명할 수 있어야 함
**Common wrong answers**:
- AI가 잡은 버그를 본인이 잡은 것처럼 답함 — 면접에서 1분 안에 들통

### Q-CORE-9 [medium] sync.Pool 효과 검증
**Q**: Phase 1에서 sync.Pool을 도입했어. 그 효과를 어떤 메트릭으로 검증할 수 있을까?
**Ground truth**:
- `go test -benchmem`의 allocs/op (할당 수) — pool hit 시 0 allocs/op
- pprof heap profile — allocation site가 pool로 옮겨갔는지
- GC pause 측정 — `runtime/metrics` 또는 GODEBUG=gctrace=1
- 처리량 (RPS) — GC pressure 감소로 throughput 향상
- ADR-002, ADR-003 참고
**Common wrong answers**:
- "RPS만" — allocation 자체를 측정해야 핵심 효과 확인 가능

---

## §Phase 5 — Raft Leader Election (ADR-010)

### Q5-1 [easy] 핵심 역할
**Q**: Raft의 leader가 하는 일 3가지.
**Ground truth**:
1. 클라이언트 쓰기 요청 수신 + log entry 생성
2. AppendEntries로 followers에 로그 복제
3. heartbeat 송신 (follower의 election timeout reset)
- `internal/infrastructure/raft/node.go` §Leader path

### Q5-2 [medium] Term의 의미
**Q**: Raft의 `term` (임기)이 왜 필요해? 물리적 시간이랑 뭐가 달라?
**Ground truth**:
- 분산 시스템에서 물리 clock은 drift / skew 발생 → 신뢰 불가
- term은 논리적 단조 증가 카운터. 노드들이 합의로 증가시킴
- 옛날 term의 메시지는 무시 → 좀비 leader 격리
- `internal/infrastructure/raft/node.go`의 `currentTerm` 필드

### Q5-3 [hard] Quorum의 의미
**Q**: 3-node 클러스터에서 quorum = 2인 이유. 왜 majority인가?
**Ground truth**:
- 두 quorum은 반드시 교집합을 가짐 ((N+1)/2) — overlapping quorums
- 새 leader가 선출되려면 quorum의 vote 필요 → 그 quorum 중 최소 1명은 이전 leader의 commit log 보유 → leader completeness
- ADR-010, 원논문 §5.4

### Q5-4 [medium] Election restriction
**Q**: Phase 5b에서 election restriction을 왜 추가했어?
**Ground truth**:
- candidate가 자기 log가 최신이 아니면 leader가 되면 안 됨
- 그래야 commit된 log entry가 새 leader에서 보존됨 (Leader Completeness Property)
- `RequestVote`에서 `lastLogIndex`/`lastLogTerm` 비교
- ADR-011

---

## §Phase 8 — Async WAL Replication (ADR-008)

### Q8-1 [medium] Async vs Raft 복제 차이
**Q**: Phase 8의 async WAL streaming이랑 Phase 5의 Raft log replication이 뭐가 달라?
**Ground truth**:
- Async: 다른 노드로 best-effort 전송, 합의 없음 → 데이터 손실 가능
- Raft: quorum 확인 후 commit → 손실 없음
- Phase 12 Storage Unification 후 async는 사실상 dead code (KV는 Raft만 사용)
- ADR-008, ADR-021

---

## §Phase 10 — Linearizable Read (ADR-019)

### Q10-1 [medium] Stale read 시나리오
**Q**: ReadIndex 없이 leader가 그냥 본인 데이터를 반환하면 어떤 stale read가 발생할 수 있어?
**Ground truth**:
- 옛 leader가 partition으로 격리됨 → 새 leader는 새 데이터 commit
- 옛 leader는 자기가 아직 leader인 줄 알고 stale data 반환
- ADR-019 §Problem

### Q10-2 [hard] ReadIndex 메커니즘
**Q**: ReadIndex protocol이 quorum heartbeat을 보내는 이유는?
**Ground truth**:
- "나 아직 진짜 leader인가?" 확인
- quorum의 응답이 오면 → 그 시점에는 다른 leader가 commit할 수 없음
- 그 시점의 commitIndex까지 apply된 후 read 수행
- ADR-019 §ReadIndex

### Q10-3 [hard] Lease Read의 위험
**Q**: Lease Read는 ReadIndex보다 빠른데, 어떤 가정에 의존하지? 그 가정이 깨지면?
**Ground truth**:
- 가정: heartbeat 간격 동안 다른 leader가 election에 성공할 수 없다 (clock 정확성)
- 가정 위반: clock drift, NTP 동기화 실패 → 옛 leader가 자기가 lease 보유라 착각 → stale read
- Google Spanner는 TrueTime으로 이 문제를 하드웨어로 해결
- 일반 서버는 위험 → ADR-019에서 `leaseEnabled` 환경변수로 opt-in

---

## §Phase 11 — Joint Consensus (ADR-020)

### Q11-1 [medium] 멤버 변경이 위험한 이유
**Q**: 3-node에 4번째 노드 추가 중에 split-brain이 가능한 시나리오 설명.
**Ground truth**:
- 구 quorum = 2 ({A,B,C}), 신 quorum = 3 ({A,B,C,D})
- 전환 중에 {A,B}가 구 규칙으로 commit, {C,D}는 신 규칙으로 분리 commit 가능
- 같은 index에 다른 값 commit → 데이터 손실
- ADR-020 §Problem

### Q11-2 [hard] Joint Consensus 작동 원리
**Q**: Joint Consensus의 C_old,new 단계에서 commit 조건은?
**Ground truth**:
- 구 quorum AND 신 quorum 둘 다의 동의 필요
- 두 quorum은 수학적으로 반드시 한 노드 이상 겹침
- 그 노드가 "이미 투표함" → 다른 conflicting commit 불가능
- ADR-020 §Joint Consensus

---

## §Phase 12 — Storage Unification (ADR-021)

### Q12-1 [medium] Phase 12 동기
**Q**: ADR-018에서 dual write path를 의도적으로 분리했었는데, ADR-021에서 통합한 이유는?
**Ground truth**:
- ADR-018: 학습 목적으로 두 경로(/ingest = Bitcask 직접, /raft/kv = Raft 통과)를 분리
- ADR-021 시점: v1 학습 목적 달성. /ingest는 복제 보장이 없어 실용 불가 → Raft 통과로 통합
- 단일 storage backend, 단일 recovery path → 운영성 ↑
- ADR-018, ADR-021

### Q12-2 [hard] 통합 후 latency 영향
**Q**: Phase 12 적용 후 /ingest의 latency가 어떻게 변했어?
**Ground truth**:
- in-process apply (Phase 13 벤치): 416ns
- 단일 노드 Raft end-to-end p99 (low load): ~65ms (`docs/END_TO_END_LATENCY_BENCHMARK.md`)
- ~16만배 증가. WAL fsync 100ms 배치가 dominant
- Throughput ceiling: ~306 RPS

---

## §Phase 13 — MVCC (ADR-022)

### Q13-1 [easy] Lost update 시나리오
**Q**: MVCC 없을 때 lost update 발생 시나리오.
**Ground truth**:
- T1 read x=100, T2 read x=100, T1 write x=90, T2 write x=80 → T1 변경 유실
- 두 변경 모두 반영되려면 x=70 이어야 함
- ADR-022 §Problem

### Q13-2 [medium] CAS 동작
**Q**: `expected_version=0`과 `expected_version=N (N>0)`의 차이.
**Ground truth**:
- `0`: 무조건 쓰기 (INV-MV4). 충돌 없음
- `N`: latest[key] == N일 때만 쓰기. 다르면 409 conflict
- `mvcc_state_machine.go`의 `apply()` §CAS check

### Q13-3 [hard] CAS conflict 후 lastApplied
**Q**: CAS 충돌 시 `lastApplied`가 advance하지 않는 이유는? Side effect는?
**Ground truth**:
- INV-MV3: 충돌 = 상태 변경 없음 → 진짜 apply 아님 → lastApplied advance 안 함
- 하지만 `notifyWaiters`는 호출 → HTTP handler가 영원히 대기하지 않게
- `conflictResults[index]`에 충돌 시점 version 기록 → 409 응답 body에 포함
- ADR-022 §Invariants

---

## §Phase 14 — CDC (ADR-023)

### Q14-1 [easy] CDC가 푸는 문제
**Q**: CDC가 없으면 외부 시스템이 KV 변경을 어떻게 알아채야 해?
**Ground truth**:
- Polling — 비효율, lag, 부하
- CDC = push-based, 효율적, 실시간
- ADR-023 §Context

### Q14-2 [hard] Slow consumer 처리
**Q**: 느린 구독자 1명이 있을 때 어떻게 처리해? 왜 그 방식을 골랐어?
**Ground truth**:
- Bounded buffer (256) + non-blocking send + drop counter
- 이유: apply()는 Raft 핫패스. 단일 slow consumer가 apply()를 블록하면 전체 클러스터 처리량 붕괴
- Drop된 소비자는 `?offset=N`으로 replay
- ADR-023 §Slow Consumer

### Q14-3 [hard] At-least-once의 의미
**Q**: CDC가 at-least-once인 이유. exactly-once는 왜 안 했어?
**Ground truth**:
- apply 후 발행 → crash between apply and publish 시 이벤트 1건 누락
- 재시작 후 Raft log replay → 중복 발행
- → at-least-once
- exactly-once는 2PC 또는 durable cursor 필요 → 학습 scope 벗어남
- ADR-023 §Trade-offs
