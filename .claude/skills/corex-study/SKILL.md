---
name: corex-study
description: Socratic study session for Core-X concepts. Asks the user questions about a specific phase/ADR/concept, evaluates answers against the actual code, and tracks weak areas. Triggers — /corex-study, "코어엑스 공부", "코어엑스 퀴즈", "Core-X 시험 봐". For general code/book study (non-Core-X), use the global `study` skill.
---

# Core-X Study Skill

Socratic study mode for Project Core-X. The user has built most of this codebase via AI agents and now needs to make it their own. This skill forces them to explain concepts in their own words before showing them the answer.

**Core principle**: Never give the answer before the user attempts. Evaluation is grounded in actual code (file:line) and ADRs, not generic knowledge.

---

## Invocation Modes

Parse the user's invocation message to determine mode:

| User Input | Mode | Behavior |
|---|---|---|
| `/study phase 5` or `/study phase5` | **focused phase** | 3–5 questions on that phase |
| `/study --adr 022` or `/study adr-022` | **focused ADR** | 3–5 questions on that ADR |
| `/study quick` | **quick** | 1 random question, 5-min warmup |
| `/study weak` | **review** | Re-ask questions the user previously failed (read `PROGRESS.md`) |
| `/study core` | **core 9** | The 9 baseline checklist questions (see QUESTION_BANK.md §Core) |
| `/study` (no arg) | **menu** | Show options and ask user to pick |
| "공부 도와줘" or similar Korean phrase | **menu** | Same as above |

---

## Session Flow

### 1. Setup
- Determine mode from invocation.
- Load `QUESTION_BANK.md` (in this directory).
- For focused mode: also read the relevant ADR file (`docs/adr/0NN-*.md`) and any code files referenced in the question bank for that phase.
- Read `PROGRESS.md` if it exists (track weak areas across sessions).

### 2. Ask One Question at a Time
- Pick from the question bank, or generate from the ADR if no pre-made question fits.
- Present the question in Korean (user's preferred chat language).
- **Do not provide hints unless asked.** Wait for the user's answer.

Format:
```
**[Phase 5 / Q1]** election timeout에 jitter가 왜 필요해? 본인 말로 1~2문장.
```

### 3. Evaluate the Answer
Once user responds, evaluate against the ground truth:

- Read the relevant code (Grep/Read) to verify the user's claim.
- Compare against the ADR's reasoning.
- Use this rubric:

| Rating | Meaning | Action |
|---|---|---|
| ✅ **정확** | Captures the key idea + reasoning | Move to next question. Briefly cite the code/ADR section that confirms. |
| 🟡 **부분** | Right direction, missing nuance | Note what's missing. Offer 1 follow-up question to draw it out. Then move on. |
| ❌ **틀림** | Wrong reasoning or wrong direction | Cite the correct answer from code/ADR (with file:line). Explain briefly. Mark this question as weak in PROGRESS.md. |
| 🤷 **모름** | "모르겠어" or evasion | Treat as ❌. Explain answer with code citation. Mark as weak. |

**Critical**: When citing code, use `file_path:line_number` format so the user can click through.

### 4. After Each Question
- Update `PROGRESS.md`:
  - Append `[date] [mode] [question_id] [rating]`
  - Add to "weak areas" if ❌ or 🤷
  - Remove from "weak areas" if same question was ✅ this time
- Move to the next question (or end if mode is `quick`).

### 5. Session End
After 3–5 questions (or 1 for `quick`), print summary:

```
## Session Summary

- Phase / Topic: <…>
- Questions: <N>
- Results: ✅ X / 🟡 Y / ❌ Z

### Strong
- (점수 잘 받은 개념들)

### Weak — 다음에 다시 볼 것
- (failed 항목들, 코드 reference 포함)

### 추천 다음 학습
- (logical next: ADR-XXX or code file)
```

---

## Behavioral Rules

1. **Korean for chat, English for code/ADR citations.** Match user's chat language preference.
2. **Cite, don't summarize.** When evaluating, point to `file:line` instead of paraphrasing.
3. **One question at a time.** Never dump 5 questions and let user pick. Force serial engagement.
4. **No spoilers.** Even if the user is clearly struggling, don't give the answer until they say "모르겠어" or attempt.
5. **Don't gloss over weak answers.** A 🟡 still gets a follow-up, not a free pass.
6. **Track progress religiously.** Every session updates `PROGRESS.md`. Don't skip this — it's the entire point of "study weak" mode.

---

## File Layout

```
.claude/skills/study/
├── SKILL.md            ← this file (behavior spec)
├── QUESTION_BANK.md    ← curated questions per phase/ADR + 9 core
└── PROGRESS.md         ← auto-tracked weak areas (created on first session)
```

When the user adds new phases, you (or they) should append to `QUESTION_BANK.md`. Do NOT generate questions on-the-fly if a curated one exists — curated ones are vetted for difficulty calibration.

---

## When NOT to Use This Skill

- User asks "explain X to me" → that's not study mode, just explain it normally.
- User asks "fix this bug" / "implement Y" → use the regular workflow, not study.
- User is asking for help understanding code while building → answer directly.

Study mode is specifically for **testing recall and understanding of already-built features**, not for learning new ones.
