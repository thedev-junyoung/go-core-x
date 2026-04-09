# Core-X Development Workflow

**Goal**: Repeat the same pattern every feature development to maintain consistency.

---

## Master Workflow Diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                      New feature / issue discovered                 │
│                    (e.g., "Need to implement WAL Reader")          │
└──────────────────────────┬──────────────────────────────────────────┘
                           │
                           ▼
          ┌────────────────────────────────┐
          │  @core-x-principal-architect   │
          │  Input: current state + reqs   │
          │  Output: design + initial code │
          └────────────────┬───────────────┘
                           │
           ┌───────────────▼───────────────┐
           │ Validate design proposal      │
           │ □ Trade-off table understood? │
           │ □ Code compiles?              │
           │ □ Gotchas identified?         │
           │ □ Edge cases clear?           │
           └───┬─────────────────────┬─────┘
               │ (approve)           │ (needs revision)
               │                     └────────┐
               │                              │
               ▼                              │
     ┌─────────────────────┐                │
     │ Approval message    │                │
     │ "Good, proceed"     │                │
     └──────────┬──────────┘                │
                │                           │
                ▼                           ▼
     ┌──────────────────────────────┐
     │ Validate & Record (ADR)      │
     │ □ Design Approved by User    │
     │ □ ADR written to docs/adr/   │
     └────────────┬─────────────────┘
                  │
                  ▼
    ┌──────────────────────────────┐    request architect
    │ @superpowers:writing-plans   │    again with feedback
    │ Input: design proposal       │
    │ Output: step-by-step plan    │
    └────────────┬─────────────────┘
                 │
     ┌───────────▼───────────┐
     │ Validate plan         │
     │ □ Step order correct? │
     │ □ Dependencies clear? │
     │ □ Success criteria?   │
     └───┬──────────┬────────┘
         │ (ok)     │ (revise)
         │          └─────┐
         │                ▼
         │        writing-plans or
         │        architect again
         │
         ▼
    ┌──────────────────────────┐
    │ Per-step implementation  │
    │ (see detailed loop below)│
    └──────────┬───────────────┘
               │
               ▼
    ┌──────────────────────────────┐
    │ @superpowers:verification    │
    │ Final validation             │
    │ □ All tests pass?            │
    │ □ Benchmarks ok?             │
    │ □ Docs updated?              │
    └───┬────────────────┬─────────┘
        │ (all pass)      │ (failed)
        │                 └─────┐
        │                       │
        ▼                       ▼
    ┌─────────┐        ┌──────────┐
    │ commit  │        │ fix &    │
    │ + docs  │        │ re-verify│
    └─────────┘        └──────────┘
```

---

## Per-Step Implementation Loop (Detailed)

```
FOR EACH Task IN Implementation_Plan:

├─ 1️⃣ TDD: Write Tests (RED)
│  │
│  ├─ @superpowers:test-driven-development
│  │  Input: function name + expected behavior
│  │  Output: test code (RED state)
│  │
│  ├─ Verify: Does test actually FAIL?
│  └─ Result: test_*.go created + FAIL confirmed
│
├─ 2️⃣ Implement (GREEN)
│  │
│  ├─ Write minimal code to pass tests
│  ├─ go test -v (verify all new tests PASS)
│  └─ Result: all new tests GREEN
│
├─ 3️⃣ Refactor
│  │
│  ├─ Remove code duplication
│  ├─ Add architecture comments
│  └─ Verify: tests still PASS
│
├─ 4️⃣ Partial Code Review
│  │
│  ├─ @superpowers:requesting-code-review
│  │  Input: just-written function
│  │  Output: feedback (edge cases, performance, style)
│  │
│  ├─ Apply fixes (if any)
│  └─ Result: review approval
│
└─ Next step in plan
```

---

## Per-Phase Deliverables

### Phase 2 Deliverables

```
Task 1: WAL Writer Design & Implementation
├─ Input:  (Problem definition)
├─ Agent:  principal-architect → see AGENTS.md
├─ Output:
│  ├─ internal/infrastructure/storage/wal/writer.go ✅
│  ├─ internal/infrastructure/storage/wal/encode.go ✅
│  ├─ bench/wal_bench_test.go (performance)
│  └─ docs/WAL_DESIGN.md
└─ Status: ✅ COMPLETE

Task 2: WAL Reader Design (in progress)
├─ Input:  (Problem definition) ✅
├─ Agent:  principal-architect (design received) ✅
├─ Output:
│  ├─ internal/infrastructure/storage/wal/errors.go (planned)
│  ├─ internal/infrastructure/storage/wal/reader.go (planned)
│  ├─ test/wal_reader_test.go (8 cases) (planned)
│  └─ docs/WAL_READER_DESIGN.md (planned)
└─ Status: ⏳ IN PROGRESS

Task 3: Integration & E2E Tests
├─ Input:  (Reader completion)
├─ Agent:  (TBD)
├─ Output:
│  ├─ cmd/main.go modification (replay logic)
│  ├─ test/e2e_crash_simulation_test.go
│  └─ docs/OPERATION.md
└─ Status: ⏳ BLOCKED (waiting Task 2)

Task 4: Documentation
├─ Input:  (All implementation complete)
├─ Agent:  (Direct writing)
├─ Output:
│  ├─ README: WAL section
│  ├─ ARCHITECTURE.md: Phase 2 summary
│  ├─ GOTCHAS.md: caveats and limitations
│  └─ OPERATION.md: operations guide
└─ Status: ⏳ BLOCKED
```

---

## Input Templates

### principal-architect Invocation Template

```markdown
@core-x-principal-architect

## Current State
- Location: internal/infrastructure/storage/wal/
- Existing: writer.go (complete), encode.go (complete)
- Next: Reader implementation

## Problem
- WAL is write-only, cannot read
- No data recovery after crash
- Phase 3 Aggregation requires Reader

## Requirements
1. Iterator pattern design (bufio.Scanner style)
2. Tail truncation recovery strategy
3. Define 8 test cases
4. Versioning approach (future extensibility)

## Constraints
- stdlib only (no external libs)
- Protocol must be compatible with Writer (minimal writer.go changes)
- Performance: recovery path, correctness > throughput

## Desired Output
- Design proposal + initial code skeleton
- Trade-off table
- 8 test scenarios
- Phase 3/4 impact analysis

Can you design this specifically?
```

### writing-plans Invocation Template

```markdown
@superpowers:writing-plans

## Design Proposal
[paste principal-architect output here]

## Files to Implement
- internal/infrastructure/storage/wal/errors.go (new)
- internal/infrastructure/storage/wal/reader.go (new)
- test/wal_reader_test.go (new)

## Questions
- Which tasks can run in parallel?
- What's the success criterion for each step?
- What's the bottleneck?

## Desired Output
- Step-by-step todo list
- Dependencies between steps
- Estimated time (subjective)

Can you create implementation plan?
```

---

## Phase Completion Checklist

### Before Claiming Phase 2 Complete

```
Code Quality
□ go fmt applied
□ go vet passes
□ staticcheck passes
□ test coverage > 80%

Functional Completeness
□ WAL Writer + Reader both implemented
□ All 8 Reader test cases pass
□ Encode ↔ Decode roundtrip validated
□ Graceful shutdown re-validated
□ E2E test (crash simulation) passes

Documentation
□ ARCHITECTURE.md written (design decisions documented)
□ GOTCHAS.md written (limitations and caveats)
□ OPERATION.md written (operations guide)
□ ADR for every major architectural change is recorded in docs/adr/
□ All functions have comments (domain/app/infra separation explained)

Performance
□ Writer: 1M req/sec achieved (SyncInterval 100ms policy)
□ Reader: 100M byte file sequential read < 1 second
□ Memory: steady state < 100MB

Next Phase Readiness
□ Phase 3 requirements defined
□ Phase 3 architecture draft ready
□ Phase 3 timeline estimated

All pass → Phase 2 complete + commit
```

---

## Red Flags

| Warning Sign | Meaning | Response |
|------|-----------|-----------|
| Repeating same agent question 3x | Feedback not improving | Clarify input or try different agent |
| "Is this a design or impl problem?" repeating | Design insufficient | Re-engage principal-architect |
| Tests written before TDD agent | Wrong order | Discard tests, start with TDD |
| No benchmark, "assuming good performance" | No validation | Write benchmark before optimization |
| Documentation deferred to end | Won't happen | Document immediately after each step |

---

## Current Position

```
You are here:

Phase 2, Task 2 (WAL Reader)
├─ ✅ Design proposal received
├─ ⏳ Here: "Proceed with implementation?"
├─ ⏳ Next: Create implementation plan (writing-plans)
└─ ⏳ Then: TDD → implement → review → verify

After approval:

1. Use AGENTS.md + AGENT_RULES.md + WORKFLOW.md to guide each step
2. principal-architect only for design
3. Implementation via TDD order
4. Code review after each part
5. Final verification before Phase complete

Repeat same pattern for Phase 3, 4, etc.
```

---

## Learning Outcomes

Following this workflow teaches:

1. **Design**: principal-architect conversations → trade-off intuition
2. **Planning**: breaking large tasks → decomposition skills
3. **TDD**: test-first thinking → bug prevention
4. **Code review**: architecture perspective → critical reading
5. **Verification**: assumptions → proof → evidence-based decisions

→ **All non-automatable engineering thinking**

---

## Next Steps

```
Now: "Should we implement Reader?"

If YES:
1. Invoke @superpowers:writing-plans
   → receive implementation plan
2. For each step:
   a. @superpowers:test-driven-development (TDD)
   b. Direct implementation (GREEN)
   c. @superpowers:requesting-code-review (validation)
3. @superpowers:verification-before-completion (final check)
4. git commit + documentation

If NO:
→ Specify why not, adjust scope
```

---

**This workflow ensures that every Phase is consistent, traceable, and repeatable.**
