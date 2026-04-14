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

1. Step Contract
   - @core-x-principal-architect and @core-x-forge-engineer agree on implementation scope
   - Output: docs/spec/SPEC_FOR_STEP.md (success criteria + data consistency invariants)

2. Adversarial Implementation
   - Forge: @core-x-forge-engineer writes the code
   - Nemesis: @chaos-auditor immediately challenges — "this code breaks under scenario X"
   - Forge: revises and optimizes based on the critique (repeat at least 3 rounds)

3. Chaos Verification
   - @chaos-auditor executes designed chaos scenarios (network partition, process crash, etc.)
   - Confirm all invariants in SPEC_FOR_STEP.md are satisfied via result logs

4. Final Approval
   - @core-x-principal-architect confirms conformance to original design and approves
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

Task 2: WAL Reader Design & Implementation
├─ Input:  (Problem definition) ✅
├─ Agent:  principal-architect (design received) ✅
├─ Output:
│  ├─ internal/infrastructure/storage/wal/errors.go ✅
│  ├─ internal/infrastructure/storage/wal/reader.go ✅
│  ├─ internal/infrastructure/storage/wal/reader_test.go ✅
│  └─ docs/adr/0004-wal-reader-design.md ✅
└─ Status: ✅ COMPLETE

Task 3: Hash Index KV Store (Bitcask Model)
├─ Input:  (WAL Reader completion) ✅
├─ Agent:  principal-architect ✅
├─ Output:
│  ├─ internal/infrastructure/storage/kv/ ✅
│  └─ docs/adr/0005-hash-index-kv-store-bitcask.md ✅
└─ Status: ✅ COMPLETE

Task 4: Integration & E2E Tests
├─ Input:  (KV Store completion)
├─ Agent:  (TBD)
├─ Output:
│  ├─ cmd/main.go modification (replay logic)
│  ├─ test/e2e_crash_simulation_test.go
│  └─ docs/OPERATION.md
└─ Status: ⏳ BLOCKED (waiting Task 3)

Task 5: Documentation
├─ Input:  (All implementation complete)
├─ Agent:  (Direct writing)
├─ Output:
│  ├─ README: WAL + KV section
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

Reliability
□ WAL checksum validation passes even on partial write
□ Bitcask index 100% rebuilt after abrupt process crash + restart
□ Linearizability maintained under network partition

Performance
□ Write latency P99 < 50ms (3-node quorum)
□ Memory usage stable under 100k events/sec sustained load
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

Phase 2, Task 4 (Integration & E2E Tests)
├─ ✅ WAL Writer: complete
├─ ✅ WAL Reader: complete
├─ ✅ Hash Index KV Store (Bitcask): complete
├─ ⏳ Here: Integration & E2E Tests
└─ ⏳ Next: Documentation → Phase 2 complete

Next steps:
1. cmd/main.go에 Recover() 연동 (crash recovery replay)
2. e2e_crash_simulation_test.go 작성
3. OPERATION.md, ARCHITECTURE.md, GOTCHAS.md 작성
4. Phase 2 completion checklist 통과 → commit
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
