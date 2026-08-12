# procedure-adherence

Raise procedure-adherence under context load from flaky to guaranteed: retrieve+compile the governing procedure pre-turn, and block the Stop until every step of the authored chain has a corresponding tool action.

## Install

Install via the Claude Code plugin system:

```bash
/plugin install /path/to/procedure-adherence-plugin
```

Or install from a published URL (future):

```bash
/plugin install drewdrewthis/procedure-adherence-plugin
```

**Prerequisites:**
- Node.js ≥18 (for native `fetch`)
- Claude Max or Claude Pro account (OAuth authentication required)
- The gate uses the Claude Messages API via OAuth; `ANTHROPIC_API_KEY`-only setups will fail open with a logged error (noted follow-up: add `x-api-key` fallback when available)

## Corpus authoring

A corpus is a directory of procedure files, each in the format `<id>/PROCEDURE.md`. The gate depends on three authored structures:

### Frontmatter (YAML, `---`-fenced)

```yaml
---
id: onboard-vendor
keywords: [onboard, vendor, procedure, safety]
status: active
always_enforced: false  # optional, default false
---
```

| Key | Required | Meaning |
|---|---|---|
| `id` | Yes | Stable slug; must equal the directory name. The sheet and gate reference procedures by this id. |
| `keywords` | Recommended | List of retrieval hints. BM25 scores full procedure bodies, so even procedures without keywords are findable; keywords accelerate retrieval. |
| `status` | Recommended | `active` or `deprecated` (surfaced in the injected block). |
| `always_enforced` | Optional | `true` to mark this as always-enforced meta-procedure tier (default `false`). See Caveats §3 for safety guardrails. |

### Procedure Steps (`## Procedure` section)

Numbered imperative steps, one concrete action each:

```markdown
## Procedure
1. Collect the intake details for vendor.
2. Create the records in the review board.
3. Attach the contact set.
4. Confirm the approval state.
```

**Author steps as concrete single actions.** Each step maps 1:1 to one tool action (Write, Edit, Bash, etc.). A compound step ("gather X and update Y") weakens the gate's ability to detect which part was skipped.

### Follow-on Procedures (`## Follow-on procedures` section)

Chain links to the next procedure in a sequence:

```markdown
## Follow-on procedures
After the steps above are complete, follow procedure `provision-account` to carry out the required follow-on work. This is a transitive hand-off: the wider task is not finished until `provision-account` has also been completed in full.
```

**The gate's enforcement scope is the deterministic transitive closure over these `## Follow-on procedures` links, never the compiled sheet.** This means if Haiku compiles a root procedure, the gate will enforce all procedures reachable through Follow-on links (not just what the sheet names). See How it works.

## Configuration (env)

| Variable | Default | Meaning |
|---|---|---|
| `ADHERENCE_CORPUS_DIR` | (resolution order) | Path to corpus directory. Resolution order: env var → `${CLAUDE_PROJECT_DIR}/.procedure-adherence/corpus` → bundled `${CLAUDE_PLUGIN_ROOT}/corpus`. |
| `ADHERENCE_GATE_MODEL` | `claude-sonnet-4-5` | Claude model for procedure judgment. **Only Claude models are accepted** (`claude-*`); non-Claude values are refused with an `unsupported-gate-model` log and fail-open (allow). No GPT. |
| `ADHERENCE_ALWAYS_ENFORCED` | `0` | Enforce always-enforced meta-procedures? `0` (off, default) or `1` (on). See Caveats §3. |
| `ADHERENCE_EXEMPT` | (none) | Session-level exemption: set to `1` to disable all hooks for this session. Alternative: list session ids in `${CLAUDE_PROJECT_DIR}/.procedure-adherence/exempt` (one id per line). See Caveats (a). |
| `ADHERENCE_HAIKU_MODEL` | `claude-haiku-4-5` | Claude model for the pre-turn compile step. |
| `ADHERENCE_RETRIEVAL_K` | `5` | Top-K procedures retrieved by BM25 before chain-expansion. |
| `ADHERENCE_RETRY_CAP` | `3` | Max Stop-block retries per human turn before fail-open (`allow-cap-hit`). |
| `ADHERENCE_HOOK_LOG` | (none) | Absolute path for the append-only JSONL evidence log. **Opt-in — when unset, no hook-log is written** (it is not always-on). The log can carry fragments of session/tool content; keep it out of version control (see Caveats (b)). |
| `LANGWATCH_INGESTION_KEY` | (optional) | LangWatch API key for telemetry ingestion (optional). If present, the gate emits adherence verdict spans; if absent, no telemetry fetch occurs. (The JSONL hook-log is separate and only written when `ADHERENCE_HOOK_LOG` is set.) |

**Corpus resolution precedence** (first hit wins):
1. `ADHERENCE_CORPUS_DIR` env var
2. `${CLAUDE_PROJECT_DIR}/.procedure-adherence/corpus` (project-specific corpus)
3. `${CLAUDE_PLUGIN_ROOT}/corpus` (bundled default corpus)

## Caveats

### (a) Per-agent exemption / identity

**The Stop+UserPromptSubmit hooks fire in every Claude Code session in the enabled project.** Not every agent should be gated (an orchestrator, a reviewer, a human-driving session).

To exempt a session:
- Set `ADHERENCE_EXEMPT=1` in the session environment, OR
- List the `session_id` in `${CLAUDE_PROJECT_DIR}/.procedure-adherence/exempt` (one id per line)

An exempt session will no-op (log `exempt`) and allow all turns without any model calls or blocking.

### (b) **Deterministic never-fail guards stay OUTSIDE the plugin**

**The gate is LLM-judged and fail-open by construction: it allows on no substrate, no credentials, judge parse-error, cap-hit, and any hook exception.** This is correct for an *adherence-raising* layer — it must never wedge a session — but it means the plugin is NOT a safety interlock.

**For safety-critical determinism (never delete prod without approval, never exfiltrate a secret), a deterministic `PreToolUse` guard must live OUTSIDE this plugin.** The plugin raises the adherence floor; it is not the last line for anything that must be guaranteed.

**Trust boundary — the gate's own control state is writable by the gated agent.** Session state (`<session_id>.sheet`, `.retry`) and the exemption list live under `${CLAUDE_PROJECT_DIR}/.procedure-adherence/`, which the gated session has ordinary Write/Bash access to. A session that *wanted* to could clear its own sheet, pre-set its retry counter past the cap, or add its own id to `exempt` and bypass the gate. This is consistent with the fail-open posture above: the gate is an adherence-raiser for a cooperative agent drifting under context load, **not** an adversarial sandbox. Treat the exemption mechanism as an operator convenience, not a security control; anything that must hold against an adversarial subject belongs in a `PreToolUse` guard outside the project tree.

### (c) Literal-canary limitation

**The gate judges adherence from whether each numbered step has a corresponding tool action in the action log — presence of a matching action, not deep semantic verification of the mutation's content.** A determined subject can satisfy a step LITERALLY: emit a Write/Edit whose surface form matches the expected state file without performing the step's true intent (the "canary" write pattern). Same class as the `grep-finds-string` anti-pattern (presence ≠ correctness).

**Documented, not fixed:** the gate raises the cost of *skipping* a step to near-certain detection; it does not guarantee the step was done *correctly.* Pairs with caveat (b) — semantic correctness of safety-critical writes needs a deterministic external check.

### Always-enforced regression bar (§4.4)

**Meta-procedures (`done`, `prove`, `audit`, etc.) marked `always_enforced: true` unregress only when:** applicability-conditioned (enforce `prove` only when a claim is made), OR satisfiable-by-construction (a `done` note the subject can always write). **Default: `ADHERENCE_ALWAYS_ENFORCED=0` (off).** The bundled corpus marks only `done` as always-enforced, which is satisfiable-by-construction. The headline recipe (§3 onboard/provision/grant chain) achieves 100% without the always-enforced tier.

## How it works

The plugin wires two hooks: `UserPromptSubmit` and `Stop`.

**UserPromptSubmit (compile phase):**
1. Retrieve top-K procedures over the corpus body using BM25.
2. Chain-expand: collect all procedures reachable through `## Follow-on procedures` links.
3. Compile ONE binding instruction sheet via Claude Messages API (OAuth), naming the applicable procedure ids.
4. Write sheet to session state (`${CLAUDE_PROJECT_DIR}/.procedure-adherence/state/<session_id>.sheet`).
5. Inject sheet onto the subject's context.
6. On throttle/error: inject raw candidate bodies + `[compile unavailable]` note (never blind).

**Stop (gate phase):**
1. Read the subject's action log (tool_use + tool_result pairs).
2. Derive `enforced` from the sheet's named ids via **deterministic transitive closure over `## Follow-on procedures` links** (not the sheet).
3. Per-procedure: run Claude Sonnet judgment (`followed=true`/`false`).
4. **BLOCK on any `followed=false`:** re-inject the missing steps, force retry.
5. Bounded by cap (default 3 retries); fail-open on cap-hit or judge error.

**Key invariant:** The enforcement scope is the deterministic transitive closure over authored `## Follow-on procedures` links, preserving reproducibility even if Haiku compiles an incomplete sheet.
