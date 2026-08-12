# DESIGN — `procedure-adherence` Claude Code plugin

**Status:** design + build plan. PORT of the sc#784-proven recipe (FINDINGS §k–§o) — not a novel design. The mechanism (chain-expansion retrieval → Haiku compile → per-procedure blocking Stop gate ≡ action-judge, chain-closed, block+force+cap+fail-open) is PROVEN: powered (n=7/arm), model-invariant (11/11 = 3/3 across Opus/Sonnet/Haiku subjects), GPT-free gate (Sonnet ≡ gpt-5.1). This doc designs the **packaging** (harness → plugin) and the **caveats**. Do not re-litigate the mechanism.

**Source of truth for the recipe:** `…/spike-784-adherence/strategies/hooks-lib.mjs` (all anchors below cite it).

---

## 1. Goal

Ship an installable Claude Code plugin that, when enabled in a project with a procedure corpus, raises procedure-adherence under context load from unreliable/flaky to guaranteed — by retrieving+compiling the governing procedure pre-turn and blocking the Stop until every step of the authored chain has a corresponding tool action.

## 2. Non-goals

- **Not a safety interlock.** The gate is LLM-judged and fail-open (see §6b). Deterministic never-fail guards stay OUTSIDE the plugin.
- **Not re-proving the mechanism.** Powered/model-invariant/GPT-free already established; the DoD is *reproduce the vendor result through the installed plugin*, not re-run the science.
- **No GPT in the shipped runtime** (owner constraint). The OpenAI gate path (`callOpenAIJudge`, hooks-lib.mjs:401-449) is DROPPED from the plugin; gate is pinned to `claude-*`.
- **No corpus authoring tooling** in v1 (adopter hand-writes / forks the bundled corpus). The `generate-corpus.ts` generator stays in the spike.
- **Not the always-enforced meta-proc tier as a default-on headline** — it regresses (§n); ships default-OFF + applicability-conditioned (see §5).

---

## 3. Architecture

### 3.1 File structure (new repo root = plugin root)

```
procedure-adherence-plugin/
├── .claude-plugin/
│   └── plugin.json                 # manifest (name/version/description/author)
├── hooks/
│   ├── hooks.json                  # wires UserPromptSubmit + Stop → entry scripts
│   ├── compile.sh                  # UserPromptSubmit entry: exec node lib.mjs compile
│   ├── gate.sh                     # Stop entry: exec node lib.mjs gate
│   └── lib.mjs                     # the ported runtime (node builtins only)
├── corpus/                         # BUNDLED default/example corpus (adopter forks/replaces)
│   ├── manifest.json
│   └── <id>/PROCEDURE.md …
├── README.md                       # install, corpus authoring, caveats, exemption
├── LICENSE                         # MIT (match ralph-loop)
└── DESIGN.md                       # this doc
```

`hooks/*.sh` are thin: they `exec "$NODE" "${CLAUDE_PLUGIN_ROOT}/hooks/lib.mjs" <mode>` so all logic lives in one auditable `lib.mjs` (mirrors the spike's single-source `hooks-lib.mjs`). `${CLAUDE_PLUGIN_ROOT}` is exported to hook subprocesses (verified).

### 3.2 Hook mapping (proven → plugin)

| Plugin hook | Mode | Ported from | Behavior |
|---|---|---|---|
| `UserPromptSubmit` | `compile` | `runH1Compile` hooks-lib.mjs:654-698 | BM25-retrieve top-K over corpus bodies, chain-expand, Haiku-compile ONE binding instruction sheet via OAuth Messages API, write sheet to a per-session state file, print sheet on stdout (folded into subject context). Throttle/empty ⇒ inject raw candidate bodies + `[compile unavailable]` note (never blind). exit 0. |
| `Stop` | `gate` | `runH3Verify` hooks-lib.mjs:1147-1399 | Read the action log; derive `enforced` from the sheet's named ids, **chain-close over `## Follow-on` (deterministic, never the sheet)**; per-procedure Sonnet `followed` judgment ≡ the action-judge; BLOCK (`{"decision":"block","reason":…}`) on any `followed=false`, re-inject the missing steps, forced retry; bounded by cap; fail-open on no-substrate/no-cred/judge-error. exit 0. |

Shared library functions port **verbatim** (pure, node-builtins-only): corpus+BM25+retrieval `parseFrontmatter/loadCorpus/tokenize/bm25/followOnIds/expandChain/retrieve/closeEnforcedChain/alwaysEnforcedIds/formatRetrievedBodies` (hooks-lib.mjs:58-274), OAuth `readOAuthToken/defaultCredsPath/callHaiku` + `CLAUDE_CODE_SPOOF` (43-44, 280-369), gate judge `buildPerProcJudgeSystem/User/parsePerProcVerdict` (457-502), compile prompts (508-526), `compiledIdsFromSheet` (564-567), step parsing `parseProcedureSteps/stepIsMutating` + verb sets (751-789).

### 3.3 Runtime resolution (the packaging decisions)

| Concern | Decision |
|---|---|
| **Corpus dir** | Resolve at hook time, first hit wins: `ADHERENCE_CORPUS_DIR` env → `${CLAUDE_PROJECT_DIR}/.procedure-adherence/corpus` (adopter's project corpus) → `${CLAUDE_PLUGIN_ROOT}/corpus` (bundled default). Documented in README. |
| **Gate model pin** | `ADHERENCE_GATE_MODEL` default `claude-sonnet-4-5` (the proven floor — §o: Sonnet ≡ gpt-5.1, Haiku over-blocks). A **non-`claude-*` value is refused**: log `unsupported-gate-model` and fail-open (allow), never route to OpenAI. `callHaiku`'s `model` param drives it (name is legacy; it calls whatever `claude-*` id you pass). |
| **OAuth creds** | `readOAuthToken(defaultCredsPath())`: fresh read of `claudeAiOauth.accessToken` from `$CLAUDE_CONFIG_DIR/.credentials.json` each call (picks up in-place refresh). Headers: `Authorization: Bearer <tok>`, `anthropic-version: 2023-06-01`, `anthropic-beta: oauth-2025-04-20`. First `system` block MUST be `CLAUDE_CODE_SPOOF` (the "You are Claude Code…" identity) or the Max/Pro OAuth credential is rejected 401/403. |
| **No nested `claude`** | Compile+gate call the DIRECT `fetch` Messages API, NEVER `claude -p --model …` — a nested claude inherits `CLAUDE_CONFIG_DIR` and re-fires this same `UserPromptSubmit` hook → infinite loop (L4, hooks-lib.mjs:16-22). Load-bearing invariant. |
| **Per-session state** | `${CLAUDE_PROJECT_DIR}/.procedure-adherence/state/<session_id>.sheet` (compiled sheet) + `.<session_id>.retry` (block counter). Keyed on `session_id` from hook stdin, so parallel sessions don't collide (cf. ralph-loop's session isolation). |
| **Node** | `hooks/*.sh` resolve `node` from PATH (`command -v node`); require ≥ node 18 (native `fetch`). README states the prerequisite. |

### 3.4 Porting deltas (harness → plugin) — the real design work

The spike ran inside a bespoke harness (tee shim + authored scenario). Three harness assumptions do NOT exist in an installed plugin and must be redesigned. **These are packaging deltas, not mechanism changes.**

| # | Harness assumption | Plugin reality | Design |
|---|---|---|---|
| **D1 — action-log source** | `readActionLogAcrossTurns(transcriptDir)` reads the tee'd `<n>.stream.jsonl` + `.counter` (hooks-lib.mjs:804-949) — artifacts only `tee-substrate.ts` writes. | No tee. Stop-hook stdin provides `transcript_path` (session JSONL on disk), `session_id`, `stop_hook_active`. | **Replace** `readActionLogAcrossTurns` with `readActionLogFromTranscript(transcript_path)` extracting `tool_use`/`tool_result` in order → the same judge-shaped action-log string. The spike already reads `transcript_path` for its observe-only verify (602-633); extend that to the whole-session action log to preserve **gate-pass ≡ judge-pass**. v1 scope = whole session (matches the proven whole-substrate scope). Turn-scoping (only actions since the last human `UserPromptSubmit`) is a noted follow-up — benign for single-task sessions; for long multi-task sessions it over-includes satisfied procs (still `followed=true`, only a cost/latency cost, bounded by cap). |
| **D2 — applicable set** | `env.applicable` = the scenario's AUTHORED ground-truth ids; `enforced = applicable ∩ sheet`. | No authored set in production. | `enforced = closeEnforcedChain(compiledIdsFromSheet(sheet, corpus), allCorpusIds, byId)`. The sheet's named ids ARE the applicability decision (Haiku chose the governing proc); chain-closure domain = all corpus ids (closure only follows AUTHORED `## Follow-on` links, so it can never pull an unlinked proc). **"Distractor turn" dissolves in production**: if no procedure governs, the compile prints "none applies", names no ids, `enforced` is empty → allow-noop. Cleaner than the harness. |
| **D3 — telemetry emit** | Harness (`run-h3.ts`) emits the judge span AFTER the run by reading the hook log. | No harness/post-run step. | The **Stop hook itself** emits the verdict span fire-and-forget (`emitJudgeVerdict`, telemetry-judge.ts:241-317, key-gated) at decision time, in addition to the JSONL hook-log line it already writes. The JSONL hook-log line IS the fallback; telemetry is the additive layer. Port as a bare `fetch` (node builtins). See §7. |

Two more ported invariants that survive unchanged: the Stop loop-guard (`stop_hook_active===true` yields; the retry counter, now keyed on `session_id`+block-count, bounds re-fires and guarantees termination) and the "hook error ⇒ exit 0" catch-all (a hook failure must never abort the subject turn, hooks-lib.mjs:1450-1454).

---

## 4. Corpus conventions

A corpus is a directory of `<id>/PROCEDURE.md`. The gate depends on three authored structures; everything else is prose the BM25 body-scorer indexes.

### 4.1 Frontmatter (`---`-fenced, parsed by `parseFrontmatter`)

| Key | Required | Meaning |
|---|---|---|
| `id` | yes | Stable slug; must equal the dir name. The sheet/gate reference procedures by this id. |
| `keywords` | rec. | `[a, b]` list — retrieval hints (NOTE: BM25 scores full BODIES, not just keywords, so a target phrased without its own vocab still lands in top-K; hooks-lib.mjs:118-124). |
| `status` | rec. | `active` / `deprecated` (surfaced in the injected block). |
| `always_enforced` | opt. | `true` ⇒ the always-enforced meta tier (§4.4). **Default false.** |

### 4.2 `## Procedure` — the action-checkable steps (load-bearing)

Numbered imperative steps: `1. <verb> …`. `parseProcedureSteps` (751-765) reads exactly this section. `stepIsMutating` (783-789) classifies each by leading verb (MUTATING vs READING sets, 771-780) → the gate's "this step needs a Write/Edit" vs "read/verify" requirement. **Author steps as concrete single actions** (each maps 1:1 to one tool action); a compound step ("gather X and update Y") splits the mutation signal and weakens the gate.

### 4.3 `## Follow-on procedures` — the authored chain (load-bearing)

`followOnIds` (166-171) matches ONLY `follow procedure `<id>`` inside a `## Follow-on procedures` section — NOT `## Escalation` (every proc has `escalate-ticket`) and NOT `## Related procedures`. This section is the transitive hand-off (e.g. `onboard-vendor → provision-account → grant-access`) and is the SOLE input to `closeEnforcedChain` — **the gate's enforcement scope is the deterministic transitive closure over these links, never the compiled sheet** (this is the H4 fix: it re-adds a hop Haiku dropped from the sheet; §k).

### 4.4 The `always_enforced` tier — ship default-OFF, carry the §n lesson

Meta-procs (`done`, `prove`, `audit`, `format`, `improvise-lookup`) marked `always_enforced: true` are unioned into `enforced` on real task turns only (`alwaysEnforcedIds`, 258-260; union guard requires `enforced` already non-empty, 1196-1203).

**§n regression (MUST carry into README + default config):** uniform enforcement of context-INAPPLICABLE meta-procs is unsafe. `prove` (substantiate a claim) and `improvise-lookup` (no covering proc → author one) are inapplicable to a covered non-claim task → the gate judges them `followed=false` on every retry → permanent block → cap-hit → retry churn (§n: 313 turns → timeout → run aborted). **Structural, deterministic, recurs every run — not a flake.** Therefore:

- Ship `ADHERENCE_ALWAYS_ENFORCED` **default `0` (off).** The headline recipe does not need it (§k–§m 100% without it).
- Only meta-procs that are **applicability-conditioned** (enforce `prove` only when a claim is made) OR **satisfiable-by-construction** (a `done` note the subject can always write) are safe to mark `always_enforced`. README documents this bar; the bundled `done` is satisfiable-by-construction, `prove`/`improvise-lookup` ship UNmarked.

---

## 5. The 3 adoption-audit caveats (REQUIRED)

### (a) Per-agent exemption / identity

The Stop+UserPromptSubmit hooks fire in EVERY Claude Code session in the enabled project (cf. ralph-loop's `session_id` isolation). Not every agent should be gated (an orchestrator, a reviewer, a human-driving session). **Design:** the gate no-ops (allow, log `exempt`) when ANY holds — checked first, before any model call:
- `ADHERENCE_EXEMPT=1` in the session env, OR
- the `session_id` is listed in `${CLAUDE_PROJECT_DIR}/.procedure-adherence/exempt` (one id/line), OR
- (optional) a `agent`/persona tag in hook stdin matches a denylist.

Opt-IN posture is also supported (README): enable the plugin only in projects/sessions that want enforcement. Identity scoping is a first-class config surface, not an afterthought.

### (b) Deterministic never-fail guards stay OUTSIDE the plugin

The gate is **LLM-judged and fail-open by construction**: it allows on no-substrate (1219), no-cred/no-key (per-proc `judgeOk=false` ⇒ fail-open, 1235-1246/1294-1298), judge parse-error, cap-hit (1317), and any hook exception (1450). This is correct for an *adherence-raising* layer — it must never wedge a session — but it means **the plugin is NOT a safety interlock.** For safety-critical determinism (never delete prod without approval, never exfiltrate a secret), a deterministic `PreToolUse` guard must live OUTSIDE this plugin. README states this in bold: *the plugin raises the adherence floor; it is not the last line for anything that must be guaranteed.*

### (c) Literal-canary limitation

The gate judges `followed` from whether each numbered step has a **corresponding tool action** in the (truncated ~600-char) action log — presence of a matching action, not deep semantic verification of the mutation's content. A determined subject can satisfy a step LITERALLY: emit a Write/Edit whose surface form matches the step's described artifact (a "canary" write to the expected state file) without performing the step's true intent. Same class as the `grep-finds-string` AC anti-pattern (presence of a token ≠ the load-bearing content). **Documented, not fixed:** the gate raises the cost of *skipping* a step to near-certain detection; it does not guarantee the step was done *correctly*. Pairs with (b) — semantic correctness of safety-critical writes needs a deterministic external check.

---

## 6. Optional LangWatch telemetry

Additive observability, **key-gated, fire-and-forget, JSONL-fallback** — a LangWatch problem can never fail or slow a turn.

- **Opt-in JSONL fallback:** when `ADHERENCE_HOOK_LOG` is set, every gate decision is one JSONL line in the hook log (`logHookEvent` 593-600) — decision, `enforced`, `enforcedVia`, per-proc `followed`/`missingSteps`/`reasoning`, `blockedProcs`, retry. When unset, no hook-log is written (README: it is not always-on). With the env var set, this is the durable run-data with zero external dependency.
- **Additive span (key-gated):** if an ingestion key resolves (`LANGWATCH_INGESTION_KEY` env → project `.env`; `loadIngestionKey`/precedence per sandbox.ts:105-131), the Stop hook fires `emitJudgeVerdict` (telemetry-judge.ts:241-317) → OTLP `POST /v1/traces` with the `sc784.judge.verdict` span (per-proc `followed`/attribution/reasoning + adherence rate; scope `sc784.judge`). No key ⇒ `{emitted:false, reason:"no-ingestion-key"}` with ZERO fetch. Every emit mints + logs a `traceId` for query-back (`get_trace <traceId>`). A bare HTTP 200 is not proof — check `partialSuccess.rejectedSpans` (telemetry-judge.ts:292-308).
- Span name/scope: rename `sc784.*` → `procedure_adherence.*` for the plugin (cosmetic; note the trace query strings change).

---

## 7. Repo-location recommendation

**Recommend: standalone repo `drewdrewthis/procedure-adherence-plugin`** (plugin root = repo root; `.claude-plugin/plugin.json` at top).

Why standalone over a marketplace subdir:
- **Own lifecycle.** Non-trivial runtime (~500-line `lib.mjs` + a corpus + node) wants its own versioning, issues, and CI (offline smoke of the pure functions — bm25/parse/step-classify need no bucket).
- **Corpus is the fork point.** Adopters fork the repo and replace `corpus/` with their own procedures — a standalone repo is the natural "template" fork; a marketplace subdir is not independently forkable.
- **Marketplace is NOT precluded.** A standalone repo is still listable in any `marketplace.json` via `source: {source:"url", url:"https://github.com/drewdrewthis/procedure-adherence-plugin.git", sha:"…"}` (the exact convention used by many entries in `claude-plugins-official`) — distribution and lifecycle both satisfied.
- **One-way-door note:** standalone → marketplace-subdir later is a cheap vendor-in; marketplace-subdir → standalone later is a painful extract (entangled version/issue history). Start standalone (reversible), the order's default.

Direct install for the DoD test: `/plugin install <path-or-url>` (or add a local marketplace entry `source: "./"`).

---

## 8. Build plan

Ordered, atomic. Judgment-bearing steps → `coder`; mechanical → `fast-coder` (per `~/.claude/references/model-selection.md`).

| # | Step | Files | Agent |
|---|---|---|---|
| 1 | Scaffold repo: `.claude-plugin/plugin.json`, `hooks/hooks.json`, `hooks/{compile,gate}.sh`, LICENSE (MIT), README skeleton | new | fast-coder |
| 2 | Port pure lib verbatim into `hooks/lib.mjs`: corpus/BM25/retrieval/chain (58-274), OAuth+`callHaiku`+spoof (43-44,280-369), gate prompts+parse (457-526), `compiledIdsFromSheet` (564-567), step parse+classify (751-789) | `hooks/lib.mjs` | fast-coder |
| 3 | **D1** — replace `readActionLogAcrossTurns` with `readActionLogFromTranscript(transcript_path)` (whole-session, judge-shaped) | `hooks/lib.mjs` | coder |
| 4 | **D2** — `runGate`: derive `enforced` from `closeEnforcedChain(compiledIdsFromSheet(...), allCorpusIds, byId)` (drop `env.applicable`); union always-enforced on real task turns; **drop the OpenAI path**, pin gate to `claude-*`, refuse non-claude | `hooks/lib.mjs` | coder |
| 5 | **D3** — inline `emitJudgeVerdict` (fire-and-forget, key-gated) + rename spans `procedure_adherence.*`; keep JSONL log always-on | `hooks/lib.mjs` | coder |
| 6 | Exemption + session-keyed state/retry (§5a): exempt-check first; `state/<session_id>.{sheet,retry}`; retry reset on new human turn | `hooks/lib.mjs` | coder |
| 7 | Bundle a small default `corpus/` (fork/trim the spike's vendor chain + a satisfiable `done`); README: install, corpus authoring, §4.4 bar, all §5 caveats | `corpus/`, README | fast-coder |
| 8 | Offline smoke (`node --test` or a `smoke.mjs`): import lib, exercise bm25/parseFrontmatter/followOnIds/closeEnforcedChain/parseProcedureSteps/stepIsMutating/`compiledIdsFromSheet` + a mocked-fetch gate block/allow. Zero bucket. | `test/` | coder |
| 9 | **DoD harness test** — install the plugin into the sc#784 sandbox as the subject's hooks; run the vendor scenario; capture run-data + LW query-back (see ACs) | spike harness | coder |
| 10 | PR review-ready (never merge): DESIGN + run-data + AC evidence + §5 caveats in the PR body | — | orchestrator |

---

## 9. Risks

| Risk | Severity | Mitigation |
|---|---|---|
| **D1 transcript shape drift** — `transcript_path` JSONL format differs from the tee'd stream-json; action log comes out empty → gate fail-opens silently (un-enforced green) | High | Step-8 smoke asserts a non-empty action log from a real `transcript_path` fixture; step-9 asserts `blocks≥1` on the vendor miss (a silent fail-open shows as `allow-no-substrate` in the log — caught). |
| **D2 wrong governing proc** — Haiku compiles the wrong proc into the sheet → gate enforces an inapplicable proc → false block | Med | Chain-closure only follows authored links (can't wander); retry-cap + fail-open bound it; compile prompt already constrains to "which ONE candidate governs". Surfaced in the hook log (`enforced`/`enforcedVia`). |
| **Always-enforced regression** (§n) if an adopter flips it on with inapplicable metas | High (recurs every run) | Default OFF; README bar (applicability-conditioned / satisfiable-by-construction); bundled corpus ships only a safe `done`. |
| **OAuth-only auth** — adopters on `ANTHROPIC_API_KEY` (no Max/Pro OAuth) can't call the gate → every turn fail-opens | Med | Documented prerequisite; noted follow-up: add an `x-api-key` path when `ANTHROPIC_API_KEY` is set (OAuth stays the proven default). |
| **Gate cost/latency** — up to `cap × \|enforced\|` sequential Sonnet calls per Stop on the subject's critical path | Med | Hook timeout headroom (spike used 150s); cap default 3; whole-session scope is the cost driver D1 turn-scoping would cut (follow-up). |
| **Fail-open = hollow green** (one-way perception risk) — a mis-wired plugin allows everything and looks installed | Med | The DoD AC is *fire-on-miss→force with on-disk forced step + block in the log*, NOT "installed + green"; §6 JSONL log makes fail-open decisions auditable. |

**One-way doors:** repo-location (§7 — start standalone, reversible); span namespace `procedure_adherence.*` (rename now, before any adopter builds dashboards on it).

---

## 10. Alternatives considered

- **Marketplace subdir instead of standalone repo** — rejected (§7): entangles lifecycle, not forkable as a corpus template; standalone can still be marketplace-listed by URL.
- **Keep the tee substrate in the plugin** — rejected: the tee is a harness wrapper around `claude -p`; an installed plugin has no control over how the subject is launched. `transcript_path` is the CC-native, always-present action-log source (D1).
- **Keep `env.applicable` (authored set)** — impossible in production (no authored ground truth). Sheet-named ids + deterministic chain-closure is the faithful production analog (D2).
- **Keep the OpenAI gate as an option** — rejected: owner constraint (no GPT in runtime) + Sonnet proven at parity (§o). Dropping it also removes a key-management surface.
- **Always-enforced tier ON by default** — rejected (§n regression).
- **Post-run telemetry sidecar** (mirror the harness) — rejected: no post-run step exists in an installed plugin; the hook emits inline (D3).

---

## 11. AC draft
<!-- ACs ready for ac-reviewer — revised per review 2026-07-11 -->

Evidence shapes are proof-it-works (observed this turn), per `~/.claude/references/principles/acceptance-criteria.md`. Data-first: every run yields durable run-data + a LangWatch query-back.

**Systemic note:** several ACs below cite the plugin's OWN hook-log JSONL as evidence (a self-report) — acceptable for decision-TRACE assertions (what did the gate log?), but a CAUSAL claim (a block *caused* a step) needs an INDEPENDENT on-disk artifact pair, not just the gate's account of itself. AC1a does this correctly (pre-block/post-retry `state/*.json` pair); AC5/AC7/AC12 are observational-only (hook-log alone), acceptable since they assert decision-trace behavior, not causation.

**Headline / DoD**
- **AC1a — Per-run enforcement, ENACTABILITY-IMMUNE.** On the vendor run the gate emits `decision:"block"` whose `blockedProcs:["grant-access"]` and `perProc["grant-access"].followed=false`/`missingSteps` show the expiry step ABSENT from the pre-Stop action log; the forced retry then produces the on-disk audit-ledger binding carrying the contract-derived `expires_at`; the run then closes `allow-complete-after-retry`. *Fails if:* the on-disk `expires_at` is already present in the PRE-block `state/*.json` snapshot (the subject did it unforced — enactability, not enforcement, the §k confound), or no block precedes the completion. *Evidence:* the block event's `missingSteps` + a pre-block `state/*.json` snapshot lacking `expires_at` + the post-retry snapshot carrying it.
- **AC1b — Baseline is FLAKY (the delta).** Over N≥5 baseline-vendor runs on the SAME seed, baseline-arm adherence is bimodal/sub-1.0 — at least 1 of N runs misses a step (onboard-vendor / provision-account / grant-access) — while the plugin arm reaches full-chain completion on N/N runs, with the gate firing every run it's needed (`blocks≥1`, `retryForcedCompletion=true`). *Fails if:* every baseline run also completes the full chain (no discrimination — the mechanism is unproven on this population), or any plugin run falls short of full-chain completion. *Evidence:* a per-run outcome table + gate-summary drawn from `run-data/` for both arms, citing the exact `seedVendorProject(...)` invocation used to hold the seed constant across runs. State the finding as "baseline is flaky (≥1 of N runs misses a step)" — NEVER as a powered miss-rate (e.g. never "X% of runs fail").
- **AC2 — LangWatch query-back, RIGHT project.** The Stop hook's emitted verdict span is independently retrievable from the INGESTION key's OWN project (`mr-krusty-klaws-iw3n10`, the project the Claude-gate runs write to) — NOT the session's default-keyed LangWatch MCP project, which is a different project and produces a false-negative. *Fails if:* `get_trace <traceId>` scoped to `mr-krusty-klaws-iw3n10` returns nothing, or `rejectedSpans>0`. *Evidence:* `get_trace <traceId>` (or `GET /api/traces/{id}` with `X-Auth-Token`) against `mr-krusty-klaws-iw3n10` showing the `procedure_adherence.judge.verdict` span with per-proc `followed`.
- **AC3 — DATA-FIRST: run-data survives teardown.** Before sandbox teardown, the DoD run (build-plan step 9) copies checkpoint + hook-events + `state/*.json` + the compiled sheet into a durable path — the worktree's `run-data/<arm>/`, NEVER `/tmp`; the copy is secret-scan clean. *Fails if:* the AC1a/AC1b/AC2 evidence exists only under an ephemeral `/tmp` sandbox at review time (gone by the time a reviewer checks). *Evidence:* `ls run-data/<arm>/` showing checkpoint + hook-log + state snapshots present, plus a clean secret-scan of that directory.

**Enforcement mechanics**
- **AC4 — D1 transcript positive control.** Given a real Claude Code session `transcript_path` fixture with K known `tool_use`/`tool_result` events, `readActionLogFromTranscript` returns a NON-EMPTY, judge-shaped action-log string containing those tool names/inputs in order, with extracted-count `== K`. *Fails if:* the returned string is empty or the count ≠ K (silent hollow-green: the gate fail-opens `allow-no-substrate` on every real session). *Evidence:* `node smoke` output on the fixture, quoting the extracted count and a sample of the ordered string.
- **AC5 — Chain-closure re-adds a dropped hop, and ONLY the authored chain.** When the compiled sheet names a chain root but omits a downstream hop, the gate still enforces the hop; conversely `closeEnforcedChain('onboard-vendor')` returns EXACTLY `{onboard-vendor, provision-account, grant-access}` and does NOT pull in `escalate-ticket` (every procedure has a `## Escalation` section — `followOnIds` matches only `## Follow-on procedures`). *Fails if:* a sheet omitting `grant-access` yields `enforced` without it (positive miss), OR the closure includes `escalate-ticket` or any id outside the authored chain (over-eager negative-control miss). *Evidence:* hook-log line `enforced` includes `grant-access` with `enforcedVia` ending `+chain` on a sheet whose text lacks that id; AND a `node smoke` assertion on `closeEnforcedChain('onboard-vendor')` returning the exact 3-id set.
- **AC6 — Gate is GPT-free and pinned.** No OpenAI request is issued from the shipped runtime; the gate runs `claude-sonnet-4-5`. *Fails if:* any `api.openai.com` request occurs, a non-`claude-*` `ADHERENCE_GATE_MODEL` routes anywhere but fail-open, or `callOpenAIJudge` is referenced anywhere in the shipped `lib.mjs` (dead-code residue is still a future-risk surface). *Evidence:* the run's per-proc verdict lines show `gateModel:"claude-sonnet-4-5"`; a grep of the shipped `lib.mjs` shows zero hits for both `api.openai.com` and `callOpenAIJudge`; setting a non-claude model logs `unsupported-gate-model` + allow.
- **AC7 — Bounded termination.** A proc induced to be unsatisfiable — authored so its sole mutating step can never get a matching tool action (e.g. it names an artifact the subject's toolset cannot produce) — hits the cap and releases, never loops. *Fails if:* blocks exceed the cap or the session wedges past the hook timeout. *Evidence:* hook-log shows exactly `cap` `block` lines then `allow-cap-hit`.
- **AC8 — Compile never blind.** When the Haiku compile call is throttled or returns empty, `UserPromptSubmit`'s stdout still carries the RAW retrieved candidate bodies plus a `[compile unavailable]` marker, and exits 0 so the turn proceeds. *Fails if:* stdout is empty or silent on a throttled/empty compile (a silent no-inject — the H1=0.00 failure mode). *Evidence:* an induced-throttle smoke run's hook stdout, quoted, showing the raw bodies + the marker string.
- **AC9 — Gate-live positive control.** On a covered vendor turn, `enforced` equals the 3-id chain AND the hook-log shows ≥1 per-proc verdict with `judgeOk=true` (a live Sonnet call fired) — catches a D2 bug that leaves `enforced` permanently empty, under which AC11/AC12/AC13 below would all pass vacuously on a plugin that never enforces anything. *Fails if:* `enforced` is empty on a covered turn, or every per-proc verdict shows `judgeOk=false`/absent. *Evidence:* hook-log lines for a covered vendor turn showing `enforced` = the 3 chain ids and ≥1 `judgeOk:true` verdict.
- **AC10 — No-nested-`claude`, single-fire compile (L4 invariant).** Neither hook ever spawns a `claude` subprocess — both model calls go through a direct `fetch` to `api.anthropic.com`; on an installed covered turn, `UserPromptSubmit` fires exactly ONCE per human turn. *Fails if:* a grep of the shipped `lib.mjs` for `claude -p` / `spawn.*claude` / `exec.*claude` returns any hit, or the hook-log shows more than one compile event for a single human prompt (a nested `claude` would inherit `CLAUDE_CONFIG_DIR` and re-fire this hook — infinite loop + Max-bucket burn). *Evidence:* grep output (zero hits) for the three patterns + a hook-log excerpt from one human turn showing exactly one compile event.

**Fail-open / edge surface**
- **AC11 — Fail-open is HONEST: never a masked pass.** Each of the seven `allow-*` decision paths fires its own distinct marker — never `allow-complete`/`allow-complete-after-retry` (the real-pass markers). The seven: (1) no readable action log → `allow-no-substrate` (hooks-lib.mjs:1223); (2) no OAuth cred / judge API-fail / judge parse-error → per-proc `judgeOk=false` → `allow-judge-partial` (1295-1303); (3) hook exception → `hook-error` + exit 0 (1451); (4) retry-counter file unwritable → `allow-counter-unwritable` (1368); (5) `ADHERENCE_GATE_MODEL` non-`claude-*` → `unsupported-gate-model`; (6) retry cap exceeded → `allow-cap-hit` (tested at AC7); (7) no procedure governs the turn → `allow-noop` (tested at AC12). This AC directly induces (1)-(5). *Fails if:* any of (1)-(5), when induced, yields `block` OR yields `allow-complete`/`allow-complete-after-retry` instead of its designated marker (the hollow-green direction — the gate silently claiming a real pass rather than admitting it couldn't judge). *Evidence:* five induced runs, each hook-log decision line showing its exact designated marker — never `block`, never `allow-complete*`.
- **AC12 — No-applicable ⇒ allow-noop.** A turn no procedure governs (compile names no id) does zero gate model calls and allows. *Fails if:* `enforced` non-empty or a block occurs on a non-covered turn. *Evidence:* hook-log `decision:"allow-noop"`, `enforced:[]`.
- **AC13 — Telemetry key-gated no-op.** With no ingestion key, the hook issues ZERO OTLP fetch and the turn is unaffected; the JSONL fallback line is still written. *Fails if:* any `/v1/traces` POST occurs without a key, or the missing key changes the gate decision. *Evidence:* `emitJudgeVerdict` returns `{emitted:false,reason:"no-ingestion-key"}`; the hook-log decision line is present regardless.
- **AC14 — Telemetry endpoint-down never slows the turn.** With an ingestion key SET but the telemetry endpoint hanging, `emitJudgeVerdict` aborts at its ~4s timeout and the turn completes normally; the JSONL decision line is still written. *Fails if:* the Stop hook takes materially longer than the timeout to return, or the decision line is missing/blocked pending the telemetry call. *Evidence:* a run against a hanging telemetry endpoint, wall-clock timing the Stop hook (bounded near the timeout) + the hook-log decision line present.

**Packaging / adoption**
- **AC15 — Corpus location resolves by precedence, proven by distinct id counts.** `ADHERENCE_CORPUS_DIR` env > project `.procedure-adherence/corpus` > bundled `${CLAUDE_PLUGIN_ROOT}/corpus`; the three fixture corpora carry DIFFERENT id counts (e.g. env=2, project=5, bundled=8) so the logged count alone proves which source won. *Fails if:* a higher-precedence corpus is present but the logged count matches a lower tier's count (wrong source won), or nothing loads with only the bundled corpus present. *Evidence:* three smoke runs, each logging the resolved corpus path + loaded id count, matching only the expected winning tier.
- **AC16 — Per-agent exemption.** An exempt session (`ADHERENCE_EXEMPT=1` or listed in `exempt`) does zero model calls and never blocks. *Fails if:* an exempt session emits a compile/gate model call or a block. *Evidence:* hook-log `decision:"exempt"` (or absent hook activity), zero OAuth calls.
- **AC17 — OAuth identity spoof present.** The Messages API call carries the Claude-Code identity first system block + `anthropic-beta: oauth-2025-04-20`. *Fails if:* the first system block is the task instruction (credential rejected 401/403). *Evidence:* a mocked-fetch smoke asserting request body `system[0].text === CLAUDE_CODE_SPOOF` and the beta header.
- **AC18 — Always-enforced ships safe, PROVEN behaviorally.** `ADHERENCE_ALWAYS_ENFORCED` defaults off — parsed `=== "1"` (opt-IN), never the inverted `!== "0"` idiom (default-ON, silently ships the §n regression enabled); the bundled corpus marks only satisfiable-by-construction metas; README documents the §n regression + the applicability bar. *Fails if:* a Stop-hook run with the env var UNSET on a real task turn shows `enforcedVia` containing `+always` (proves default-ON regardless of what a source grep suggests), or `prove`/`improvise-lookup` ship `always_enforced:true`, or the README lacks the bar. *Evidence:* a live Stop-hook run with the env var unset, quoting hook-log `enforcedVia` (no `+always`); frontmatter of bundled `prove.md`/`improvise-lookup.md` (no `always_enforced:true`); a README quote of the applicability bar. NEVER accept a source grep for `!== "0"` as evidence — that idiom reads default-ON as default-OFF and passes on the wrong port.
- **AC19 — Caveats documented.** README states, in bold, all three §5 caveats (exemption/identity, deterministic-guards-outside, literal-canary). *Fails if:* any of the three is absent. *Evidence:* three README quotes, one per caveat (exact load-bearing sentence, not a keyword).

---

## 12. Handoff
- ACs ready for ac-reviewer (see §11 AC draft above).
- Implementation → `coder` for the porting deltas D1–D3 + exemption/state (steps 3-6, 8, 9) and `fast-coder` for scaffolding + verbatim lib port + corpus/README (steps 1, 2, 7), per `~/.claude/references/model-selection.md`.
- Test bed = the sc#784 harness (`…/spike-784-adherence`); PR review-ready, NEVER merge (the user merges).
