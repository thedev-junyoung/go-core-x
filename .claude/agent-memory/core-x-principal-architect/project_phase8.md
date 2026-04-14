---
name: Phase 8 Durability Design
description: Phase 8 Raft KV Durability — KVStateMachine Bitcask 영속화 설계 결정 요약
type: project
---

KVStateMachine의 인메모리 맵을 Bitcask KVStore로 영속화하는 Phase 8 설계가 ADR-016으로 확정됐다.

핵심 결정:
- 쓰기 순서: Bitcask.WriteKV → lastApplied atomic 증가 → notifyWaiters (역전 절대 금지)
- __raft_last_applied__ 메타 레코드를 매 apply마다 KV WAL에 append (체크포인트)
- SyncPolicy: SyncInterval(100ms) — Raft WAL이 이미 SyncImmediate이므로 이중 fsync 불필요
- 재시작 복구: bitcaskLastApplied < raftLastApplied면 Raft 로그 갭 구간 재적용
- bitcaskLastApplied > raftLastApplied면 fail-fast (os.Exit(1))

**Why:** 인메모리 KVStateMachine은 재시작 시 데이터 전부 소실. Raft WAL(SyncImmediate)이 primary durability를 제공하므로 KV WAL은 보조 레이어로 SyncInterval 허용.

**How to apply:** Phase 9 스냅샷 설계 시 이 ADR의 lastAppliedIndex 체크포인트가 snapshot index의 전제 조건임을 기억한다. KVDurableStore 인터페이스는 테스트에서 MemKVDurableStore로 교체 가능하게 설계됨.

구현 완료 (Step 1–3 + cmd/main.go 통합):
- internal/infrastructure/storage/kv/wal_codec.go — KV WAL 인코딩 (set/del/meta 레코드 타입)
- internal/infrastructure/storage/kv/raft_store.go — WriteKV, DeleteKV, GetKV, RecoverKV
- internal/infrastructure/storage/wal/writer.go — WriteOffset(payload) 메서드 추가 (offset 반환)
- internal/infrastructure/raft/kv_state_machine.go — KVDurableStore 인터페이스 + apply() 수정 + ApplyDirect + RecoverFromStore
- cmd/main.go — Raft KV WAL(data/kv.wal) 초기화 + 재시작 갭 재적용 시퀀스

설계 주의사항:
- kv.Store.raftLastApplied 필드는 WriteKV/DeleteKV에서 atomic.StoreInt64로 업데이트
- RecoverKV는 WAL 순방향 재생 — 마지막 meta 레코드 = lastApplied
- ErrTruncated(정상)/ErrCorrupted(fatal) 분기 처리
- NewKVStateMachine(nil) = 인메모리 전용(테스트 하위 호환)
- WAL 인코딩: little-endian 2바이트 길이 prefix (기존 events.wal의 big-endian과 다름에 주의)

관련 파일:
- docs/adr/016-raft-kv-durability.md
- docs/specs/SPEC_FOR_STEP_008.md
