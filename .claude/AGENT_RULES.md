# Core-X Agent Usage Rules

**Goal**: Systematize agent interaction to keep design decisions and implementation consistent throughout the project.

---

## Rule 1: When to Invoke principal-architect

### ✅ Should invoke

| Situation | Why | Example Input |
|-----------|-----|---|
| **New component design** | Pre-analyze architecture impact | "Iterator vs Callback for WAL Reader?" |
| **Performance vs reliability trade-off** | Quantify costs | "SyncImmediate vs SyncInterval(100ms) throughput diff?" |
| **Identify hidden gotchas** | Learn in advance | "Will 5 allocations per Write() become a problem later?" |
| **Cross-phase impact analysis** | Check if current design blocks future expansion | "Can we add WAL rotation in Phase 3?" |
| **Protocol design** | Manage versioning, compatibility | "Can we use Magic byte as version?" |

### ❌ Don't invoke

| Situation | Use instead | Why |
|-----------|---|---|
| Simple bug fix | `/debug` or direct fix | Design decision not needed |
| Syntax/API question | Search or Read tool | Agent overkill |
| Code style | CLAUDE.md reference | Rules already exist |
| Performance optimization (benchmark exists) | Direct implementation | Design decision not needed |

### Input Checklist

Before invoking, verify:

```
□ Current state described precisely? (file paths explicit)
□ Problem clearly stated?
□ Constraints listed? (performance, memory, compatibility)
□ Desired output format specified? (ASCII diagram? code?)
□ Why this is a design decision, not implementation?
```

---

## Rule 2: Validate Design Output

### Upon receiving design proposal

```
[ ] ASCII diagrams understandable?
[ ] Trade-off table quantitative?
[ ] Initial code skeleton actually compiles?
    → Try Go syntax check
[ ] Gotcha list impacts current project?
[ ] Phase 3/4 impact analysis reasonable?

→ After validation: explicitly say "approved"
```

### Upon receiving initial code

```
[ ] File paths correct? (internal/infrastructure/storage/wal/ etc)
[ ] import statements accurate?
[ ] struct/interface definitions follow project conventions?
[ ] Comments indicate domain/application/infrastructure separation?

→ Either directly write files or iterate with agent
```

---

## Rule 3: Progression Conditions

### Design → Implementation

```
✅ Proceed if:
□ Design includes "initial code"?
□ Protocol/data structures clearly defined? (binary format)
□ Error handling strategy specified?
□ Test case list exists?

❌ Not ready if:
→ Request "more specific" from architect
```

### Implementation → Code Review

```
✅ Proceed if:
□ All files created?
□ All tests passing?
□ Usage examples in README or comments?

❌ Not ready:
→ Continue implementation
```

### Code Review → Complete

```
✅ Proceed if:
□ All design deviations documented?
□ 3+ edge cases tested?
□ Performance benchmarks meet expectations?
□ Next Phase unaffected?

❌ Not ready:
→ Fix and re-review
```

---

## Rule 4: Agent Invocation Frequency

| Agent | Recommended Frequency | Over-use Signal |
|-------|---|---|
| principal-architect | 1 per feature/Phase | 3+ times per week |
| writing-plans | 1 per feature | Planning only, no implementation |
| TDD | 1 per function/method | Requesting TDD for every line |
| code-review | 1 per step completion | Every commit |
| verification | 1 before final completion | Every step |

**Over-use symptoms**:
- Over-reliance on agents, no original thinking
- Repeating same questions
- Blindly following agent output without verification

→ **Pause and think deeply**

---

## Rule 5: File Paths and Clarity

### Input

```
❌ Bad:
"Design WAL Reader for me"

✅ Good:
"I'm creating internal/infrastructure/storage/wal/reader.go.
 Should I use bufio.Scanner pattern or something else? Design it."
```

### Response to agent output

```
❌ Vague:
"Is this right?"

✅ Specific:
"reader.go has ReadRecord() using bufio.Reader.
 But io.ReadFull doesn't work with bufio.Reader—how to fix?"
```

---

---

## Rule 6: Mandatory Agent Order & ADR Enforcement

### 🛡️ The ADR Hand-off (CRITICAL)
Before transitioning from 'Design' (Architect) to 'Implementation' (Plans/TDD), the agent must ensure:
- **Traceability**: A corresponding ADR file must be created or updated in `docs/adr/`.
- **Persistence**: Key trade-offs and decisions must be summarized in the ADR before the planning agent starts decomposing tasks.
- **Verification**: The agent should ask: *"I've finalized the design. Should I record this in a new ADR before we start the implementation plan?"*

### Never skip this sequence

```
1. principal-architect (technical design and trade-off analysis)
   ↓ (approval)
2. ADR recording (create/update in docs/adr/) [MANDATORY]
   ↓
3. writing-plans (step-by-step implementation tasks)
   ↓
4. TDD (RED tests)
   ↓
5. implementation (minimal code for GREEN)
   ↓
6. code-review (verify against design and ADR)
   ↓ (approval)
7. verification (final check and documentation update)
   ↓
8. git commit + documentation
```

### Why this order matters

| Step | Skip consequence |
|------|---|
| Design | "This isn't what we needed" after 3 weeks |
| ADR | "Why did we do this?" forgotten after 3 months |
| Plan | Parallelize-able work done sequentially; dependencies missed |
| TDD | Bugs discovered late → 10x cost |
| Code review | Design violations and performance regressions merged to main |
| Verification | Performance worse than expected; edge cases missing |

---

## Rule 7: Documentation Pattern

### Record every major design decision

```markdown
## [Title]: [Option A vs B vs C]

**Decision**: B (SyncInterval 100ms)

**Rationale**:
- A: 10x slower throughput (unacceptable)
- C: OS crash loss window (too risky)
- B: balanced performance & reliability

**Trade-offs**:
- Power loss: max 100ms data loss (acceptable)
- Complexity: background goroutine (+10%)

**Impact**: Phase 3/4 rotation policy required

**Validation**: Benchmark 100/50/200ms results
```

Location: Source code comments (e.g., writer.go SyncPolicy section)

---

## Rule 8: Agent Error Handling

### When agent makes mistake

```
❌ Vague complaint:
"This seems wrong"
"This doesn't work"

✅ Specific error:
- Path error: "reader.go needs errors.go import but it's missing"
- Logic error: "ReadRecord() uses io.ReadFull with bufio.Reader but ReadFull doesn't work there—how?"
- Validation: "TestReader_TailTruncation fails; ErrTruncated not returned"
```

→ **Provide specific error**, then request fix

### Talking to agent

```
After agent response:
"Let me verify this more carefully..."

Specifically:
"EncodeEvent() is verified with DecodeEvent().
 But if SourceLen=2 and Source is empty, what happens?
 Re-design edge cases."
```

---

## Rule 9: Design vs Implementation Gap Management

### Track per-phase design vs actual deviations

```
Phase 2:
□ Plan: WAL Writer + Reader + integration test
□ Actual: (verify after completion)

Deviations recorded:
- Design mentioned Group Commit but deferred to Phase 3
- Reader needed 3 more allocations (protocol reason)
→ Feedback reflected in next design request
```

---

## Rule 10: Customize Agent Output Format

### When requesting design, specify output format

```
"Specify output format:

1. ASCII diagram mandatory
2. Code: only compilable Go
3. Test cases: minimum 5
4. Performance thresholds: when does it become bottleneck?
5. Phase 3/4 impact: 1 paragraph only

Give me design in this format."
```

---

## Summary: 5 Tips for Effective Agent Usage

1. **Clear input**: Not "design something" but "file X, decide between Y methods using Z"
2. **Sequential calls**: design→plan→implement, not everything at once
3. **Verify results**: test agent output yourself before trusting
4. **Document decisions**: record "why" in source comments
5. **Feedback loops**: incorporate "this was different" into next questions

---

## Phase 2 Completion Checklist

### Before Phase 2 is complete

```
Code Quality
□ go fmt applied
□ go vet passes
□ staticcheck passes
□ test coverage > 80%

Functional
□ WAL Writer + Reader both implemented
□ All 8 Reader test cases pass
□ Encode ↔ Decode roundtrip verified
□ Graceful shutdown re-validated
□ E2E test (crash simulation) passes

Documentation
□ ARCHITECTURE.md (design decisions recorded)
□ GOTCHAS.md (caveats listed)
□ OPERATION.md (operations guide)
□ Functions have comments (domain/app/infra separation)

Performance
□ Writer: 1M req/sec (SyncInterval 100ms)
□ Reader: 100MB file sequential < 1sec
□ Memory: steady state < 100MB

Next Phase Prep
□ Phase 3 requirements defined
□ Phase 3 architecture draft ready
□ Phase 3 timeline estimated

All pass → Phase 2 complete + commit
```
