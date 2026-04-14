# Core-X Principal Architect — Memory Index

- [User Profile](user_profile.md) — Go backend dev, DDIA-driven, building high-perf distributed engine
- [Phase 1 Architecture](project_phase1.md) — Zero-alloc HTTP ingestion: sync.Pool + fixed worker pool + buffered channel backpressure
- [ADR Conventions](project_adr_conventions.md) — ADR 작성 스타일: 한글, 400–600줄, 코드 샘플 포함, DDIA 3원칙 명시적 참조
- [Load Generator & Chaos Tooling](project_tooling.md) — tools/ 구현 완료: loadgen 70k RPS, chaos test 3-node OS-process isolation
- [Phase 8 Durability Design](project_phase8.md) — ADR-016 확정: Bitcask 영속화, SyncInterval, lastApplied 체크포인트, fail-fast 복구
