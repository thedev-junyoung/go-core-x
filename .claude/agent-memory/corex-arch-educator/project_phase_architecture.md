---
name: Phase Architecture
description: Core-X Phase 1~5 진행 상황, ADR 매핑, 핵심 설계 결정 요약
type: project
---

Core-X는 Go로 구현하는 분산 이벤트 수집 + KV 스토어 학습 프로젝트.

## Phase 진행 현황 (2026-04-11 기준)

| Phase | 내용 | ADR | 상태 |
|-------|------|-----|------|
| 1 | 기본 수집 파이프라인, Worker Pool, sync.Pool | ADR-001~003 | 완료 |
| 2 | WAL 기록, WAL Reader | ADR-004~005 | 완료 |
| 2b | WAL 컴팩션 (stop-the-world) | ADR-006 | 완료 |
| 3 | gRPC 분산 파티셔닝, Consistent Hashing | ADR-007 | 완료 |
| 3b | Primary→Replica 비동기 WAL 스트리밍 | ADR-008 | 완료 |
| 4 | 읽기 경로(GET /kv/{key}) + Prometheus | ADR-009 | 완료 |
| 5a | Raft 리더 선출 (election only) | ADR-010 | 완료 |
| 5b | Raft 로그 복제 + Primary 역할 연결 | 미정 | 예정 |
| 5c | Read Index 기반 Follower read | 미정 | 예정 |

## 핵심 설계 결정

- **Clean Architecture**: application/ 계층은 infrastructure/ import 금지. 포트(인터페이스)로 의존성 역전.
- **두 개의 독립적인 역할 축**: 복제 역할(CORE_X_ROLE 환경변수, 정적) vs Raft 역할(동적 선출). Phase 5b에서 연결 예정.
- **Raft 타이밍**: ElectionTimeout 150~300ms, Heartbeat 50ms (논문 권장값).
- **Prometheus 포트 패턴**: IngestMetrics 인터페이스를 application 계층에 정의, Prometheus 구현체는 infrastructure/metrics에 위치. application 코드가 Prometheus를 직접 import하지 않음.
- **WAL 스트리밍**: Phase 5b 전까지 async WAL streaming(ADR-008)이 복제를 담당. Raft election과 분리.

## 주요 패키지 구조

- `internal/infrastructure/raft/` — Raft 상태 머신 (node.go, state.go, client.go, server.go)
- `internal/infrastructure/metrics/` — Prometheus 구현체들 (ingestion, replication, cluster, raft)
- `internal/infrastructure/http/kv_handler.go` — GET /kv/{key}, consistent-hash 라우팅
- `internal/application/ingestion/` — IngestionService (포트 기반 클린 아키텍처)
- `proto/ingest.proto` — KVService.Get + RaftService(RequestVote, AppendEntries)
