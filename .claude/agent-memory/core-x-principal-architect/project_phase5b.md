---
name: Phase 5b Log Replication + Role Transition
description: Phase 5b Steps 1–12 완전 완료: AppendEntries §5.3 + RoleController + ReplicationManager + FileMetaStore
type: project
---

Phase 5b Steps 1–12 완료: AppendEntries log replication + 동적 역할 전환 (ADR-011 전체 구현).

**Why:** Raft 합의의 핵심인 로그 복제를 Phase 5a heartbeat-only에서 실제 엔트리 복제로 확장. Phase 5c에서 WAL 연동 예정.

**How to apply:** Phase 5c 작업 시 이 설계를 기반으로 WAL을 log[] slice에 wire-in.

## 구현된 것

- `proto/ingest.proto`: `LogEntry` 메시지 추가, `AppendEntriesRequest` 확장(prevLogIndex/prevLogTerm/entries/leaderCommit), `AppendEntriesResponse` 확장(conflictIndex/conflictTerm Fast Backup)
- `server.go`: `AppendEntriesArgs` / `AppendEntriesResult` struct 도입 (파라미터 폭발 방지), `RaftHandler` 인터페이스 변경
- `client.go`: `AppendEntries(AppendEntriesArgs)` 시그니처로 변경
- `node.go`:
  - `[]LogEntry` in-memory log 추가 (Phase 5c에서 WAL로 교체 예정)
  - `commitIndex`, `nextIndex[addr]`, `matchIndex[addr]` 추가
  - `HandleAppendEntries` §5.3 전체 구현: prevLogIndex 체크, 충돌 truncation, 신규 append, leaderCommit 처리
  - Fast Backup: conflictIndex/conflictTerm 반환
  - `maybeAdvanceCommitIndex`: §5.4.2 — currentTerm 엔트리만 quorum commit
  - `sendHeartbeats`: 피어별 nextIndex 기반 엔트리 전송 + Fast Backup nextIndex 조정
  - `runLeader`: 선출 시 nextIndex[]/matchIndex[] 초기화

## 핵심 설계 결정

- `entryAt(index)` 은 선형 역순 탐색: 로그는 항상 오름차순이고 recent access 패턴이 tail이므로 O(1) 기대 케이스
- 트런케이션 이후 `truncated` 플래그로 append-only 모드 전환 → O(n) 단일 패스
- `ForceLog` 는 센티넬 단일 엔트리 전략 유지 (§5.4.1 테스트 전용)

## Steps 10–12: 동적 역할 전환

- `replication/manager.go`: `ManagedReplicationServer` (atomic active flag, null-object 패턴) + `ReplicationManager` (BecomeLeader/BecomeFollower/BecomeStandalone)
- `cluster/role_controller.go`: `RoleController` 50ms polling, `RaftRoleObserver` 인터페이스 (cluster 패키지에 정의, raft import)
- `cmd/main.go`: CORE_X_ROLE 정적 분기 제거, FileMetaStore 주입(`data/raft_meta.bin`), RoleController goroutine 시작
- gRPC 제약 해결: ManagedReplicationServer는 Serve() 전에 항상 등록, active atomic으로 StreamWAL 게이팅

## 테스트 커버리지 (총 35개)

기존 30개 + RoleController 신규 5개:
- FollowerToLeader transition
- LeaderToFollower transition
- NoCallOnSameRole (idle poll)
- CandidateTriggersFollower
- ContextCancellation
