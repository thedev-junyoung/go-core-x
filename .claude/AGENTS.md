# Core-X Agent Workflow Guide

This document defines **when and how to use each agent** in the Core-X project.

**Principle**: "An undocumented decision does not exist" → All design and implementation decisions are traceable.

---

## Agent Responsibilities

### 1. `core-x-principal-architect` 🏗️

**When to invoke**: Design/architecture decisions are needed

**Responsibilities**:
- Analyze technical trade-offs (performance vs reliability vs maintainability)
- Propose initial code structure
- Identify hidden gotchas in advance
- Analyze Phase 3/4 impact

**Input format**:
```
@core-x-principal-architect

[Situation]
- Current state: ...
- Problem: ...
- Constraints: ...

[Requirements]
1. ...
2. ...

Specify explicitly [what] needs to be done.
```

**Output**:
- Design proposal (with ASCII diagrams)
- Trade-off table
- Risk/gotcha list
- Initial code skeleton

**Recent usage**:
- WAL Writer design ✓
- WAL integration checklist ✓
- WAL Reader design ✓
- Hash Index KV Store (Bitcask model) design ✓

---

### 2. `superpowers:writing-plans` 📐

**When to invoke**: After technical design is complete, to create implementation roadmap

**Responsibilities**:
- Convert design into step-by-step implementation plan
- Define file/function dependencies
- Specify success criteria for each step
- Determine parallel vs sequential work

---

### 3. `superpowers:test-driven-development` 🧪

**When to invoke**: Before implementation or before bug fix

**Responsibilities**:
- Write test cases for functions to implement (Red)
- Minimal implementation (Green)
- Refactor

---

### 4. `superpowers:requesting-code-review` 👀

**When to invoke**: After completing each step of implementation

**Responsibilities**:
- Validate written code against design
- Check for architecture violations
- Detect performance issues early
- Verify test coverage

---

### 5. `superpowers:systematic-debugging` 🔍

**When to invoke**: Test failure or bug discovered

**Responsibilities**:
- Root cause analysis (RCA)
- Create reproducible minimal test case (MCVE)
- Propose fix

---

### 6. `superpowers:verification-before-completion` ✅

**When to invoke**: Before claiming "work is complete"

**Responsibilities**:
- Confirm all tests pass
- Record benchmark results
- Update documentation
- Verify edge cases once more

---

## Workflow Flowchart

```
┌──────────────────────────────────────┐
│ New feature / issue identified       │
└──────────────┬───────────────────────┘
               │
               ▼
┌──────────────────────────────────────┐
│ @core-x-principal-architect          │
│ → design, trade-offs, initial code   │
└──────────────┬───────────────────────┘
               │ (approval)
               ▼
┌──────────────────────────────────────┐
│ @superpowers:writing-plans           │
│ → step-by-step implementation plan   │
└──────────────┬───────────────────────┘
               │
               ▼
       ┌───────────────────┐
       │ Per-step loop     │
       │ (see below)       │
       └───────────────────┘
               │
               ▼
┌──────────────────────────────────────┐
│ @superpowers:verification            │
│ Final check before completion        │
└──────────────┬───────────────────────┘
               │ (all pass)
               ▼
┌──────────────────────────────────────┐
│ git commit + documentation           │
└──────────────────────────────────────┘
```

---

## Per-Step Implementation Loop

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

## Checklist: Before Invoking Each Agent

### Before Design (principal-architect)
- [ ] Current state described precisely? (file paths)
- [ ] Requirements separated? (must-have vs nice-to-have)
- [ ] Expected complexity and impact mentioned?

### Before Implementation (writing-plans)
- [ ] Design understood?
- [ ] Test case list prepared beforehand?

### Before Code Review
- [ ] Implementation matches design?
- [ ] All tests pass?

### Before Completion Claim
- [ ] All edge cases handled?
- [ ] Documentation updated?
- [ ] No impact on next Phase?

---

## Phase Plan

### Phase 2 (Current) — WAL Implementation
- ✅ Writer design + implementation
- ⏳ Reader design (in progress)
- ⏳ Reader implementation (next)
- ⏳ Integration tests
- ⏳ Benchmarks

### Phase 3 — Aggregation/Transform/Forward
- TBD: principal-architect design for Processor extension
- TBD: Fanout pattern (WAL → aggregate → forward)
- TBD: Error recovery strategy

### Phase 4 — Monitoring/Operations
- TBD: Prometheus metrics design
- TBD: WAL rotation/compaction
- TBD: GOMAXPROCS auto-tuning

---

## Warnings

⚠️ **Don't just trust agents—verify**:
- Re-check design matches current project state
- Run code after review to test
- Validate performance optimization claims with benchmarks

⚠️ **Agents make mistakes**:
- File paths forgotten → re-check with Glob/Grep
- Past implementation missed → re-check with git log
- Too many features in one step → decompose

✅ **Effective agent usage**:
- Clear input = clear output
- One question per agent invocation
- Ask "why was this designed this way?" anytime

---

## Documentation Pattern

Record all major design decisions like this:

```markdown
## [Title]: [Option A vs B vs C]

**Decision**: B (SyncInterval 100ms)

**Rationale**:
- A: 10x slower throughput (unacceptable)
- C: OS crash risk in 100ms window (too risky)
- B: balanced performance and reliability

**Trade-offs**:
- Max data loss on power loss: 100ms window (acceptable)
- Added background goroutine (complexity +10%)

**Impact**: Phase 3/4 needs rotation policy

**Validation**: Benchmark 100ms vs 50ms vs 200ms
```

Location: Comments in relevant source files (e.g., writer.go SyncPolicy comments)

---

## Summary: 5 Tips for Effective Agent Usage

1. **Clear input**: Not "design something" but "in file X, decide between Y methods using Z approach"
2. **Sequential calls**: Don't ask everything at once; follow design→plan→implement order
3. **Verify results**: Don't blindly follow agent output; test it yourself
4. **Record decisions**: Document "why" in comments
5. **Feedback loop**: Incorporate "this was different than expected" into next questions

---

## Success Criteria for Phase 2 Completion

```
Code Quality
□ go fmt applied
□ go vet passes
□ staticcheck passes
□ test coverage > 80%

Functional Completeness
□ WAL Writer + Reader both implemented
□ All 8 Reader test cases pass
□ Encode ↔ Decode roundtrip verified
□ Graceful shutdown re-validated
□ E2E test (crash simulation) passes

Documentation
□ ARCHITECTURE.md written (design decisions)
□ GOTCHAS.md written (caveats)
□ OPERATION.md written (operations guide)
□ Functions have comments (domain/application/infrastructure separation)

Reliability
□ WAL checksum validation passes even on partial write
□ Bitcask index 100% rebuilt after abrupt process crash + restart
□ Linearizability maintained under network partition

Performance
□ Write latency P99 < 50ms (3-node quorum)
□ Memory usage stable under 100k events/sec sustained load
□ Writer: 1M req/sec achieved (SyncInterval 100ms)
□ Reader: 100MB file sequential read < 1sec
□ Memory: steady state < 100MB

Next Phase Prep
□ Phase 3 requirements defined
□ Phase 3 architecture draft (principal-architect ready)
□ Phase 3 timeline estimated

All pass → Phase 2 complete declaration + commit
```
