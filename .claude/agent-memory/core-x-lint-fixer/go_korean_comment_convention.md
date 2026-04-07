---
name: Go Korean Comment Convention for Exported Symbols
description: Korean grammar rule for Exported symbol comments — symbol name + space + postposition
type: feedback
---

**Rule:** Exported 심볼의 첫 주석은 반드시 "심볼명 + 공백 + 조사" 형식이어야 함

**Why:** 한국어 문법 규칙으로, 심볼명과 조사 사이에 공백이 있어야 올바른 문법. Go 관례와 한국어 문법을 결합하려는 프로젝트 방침.

**How to apply:**
- 모든 Exported 함수, 타입, 상수의 첫 번째 주석 검사
- 패턴: `// SymbolName[조사]` → `// SymbolName [조사]`로 수정
- 조사 종류: 은/는, 이/가, 을/를, 에/에게, 로/으로 등
- 예: `EventProcessor는` → `EventProcessor 는`, `Submit을` → `Submit 을`
- 기존 설명 내용은 절대 변경하지 말 것 (로직, "Why" 부분 유지)
- 주석 **내부**의 심볼 언급도 동일하게 적용 (예: "Config는" → "Config 는")

**Last verification (2026-04-07):** cmd/, internal/ 내 모든 .go 파일 재검사 완료. 조사가 붙어있는 모든 주석 수정 완료.
