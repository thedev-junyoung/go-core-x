---
name: ADR Conventions
description: Core-X ADR 작성 스타일 — 언어, 구조, 깊이, DDIA 참조 방식
type: project
---

ADR은 한글로 작성한다. 길이는 400–600줄 수준. 코드 샘플을 반드시 포함한다.

**Why:** 결정의 context, trade-off, 거부된 대안을 코드 수준으로 기록해야 미래 설계 결정에 활용 가능.

**How to apply:**
- 각 섹션: Context (문제 정의) → Decision (결정 + 구체 구현) → Consequences (DDIA 3원칙 기준 평가) → Alternatives Considered → Related Decisions → Monitoring/Validation
- DDIA 3원칙(Reliability/Scalability/Maintainability)을 Consequences에서 명시적으로 참조
- 성능 수치는 정성적("~100 ns") 또는 정량적 추정 모두 허용, 단 측정 전 교체 금지 (premature optimization 경고 명시)
- 거부된 대안은 반드시 "거부 이유"를 기술 (YAGNI, premature complexity, 측정 미확인 등)
- ADR 간 cross-reference 필수 (Related Decisions 섹션)
- go.mod에 외부 의존성 없는 것이 설계 목표 (ADR-001 결정) — 새 ADR에서 의존성 추가 시 명시적 이유 필요
