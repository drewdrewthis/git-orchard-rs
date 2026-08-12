# BUILD-NOTES — procedure-adherence plugin (live-validation progress)

Status: **built + live-wiring proven; DoD-tested end-to-end (see "DoD RESULT" below).** Commits: scaffold+port
`df7eae5`, dead-code prune `8514d06`, dispatch-guard fix `eb244ef`.

## What is proven
- **Offline smoke** (`test/smoke.mjs`, 0 bucket) — 15/15: D1 `readActionLogFromTranscript`
  count==K positive control; `closeEnforcedChain` re-adds dropped hops AND excludes
  `escalate-ticket` (`## Escalation` ≠ `## Follow-on`); frontmatter/step-classify; mocked
  gate-judge round-trip.
- **Live-hook smoke** (`test/live-hook-smoke.sh`) — invokes the REAL `compile.sh` + `gate.sh`
  with fake stdin + a fake transcript (no `claude -p`, no `scenario.run`). Proven: compile
  Haiku-compiles via OAuth (haiku 200), writes+prints the binding sheet naming `onboard-vendor`;
  gate fires a `decision:"block"` on the Sonnet path (`gate=claude-sonnet-4-5`,
  `enforcedVia=sheet+chain`). D1 transcript parse, D2 sheet-derived enforced, chain-closure,
  and the OAuth compile+gate all fire end-to-end.

## Findings from the live-hook smoke (2 real, both load-bearing)
1. **[FIXED `eb244ef`] Dispatch-guard silent no-op (critical).** The verbatim port kept
   `invokedDirectly = process.argv[1].endsWith("hooks-lib.mjs")`, but the plugin file is
   `lib.mjs` → `main()` never ran → every hook exited 0 doing NOTHING (silent fail-open =
   un-enforced green). The import-based offline smoke could not catch this (it never exercises
   the CLI entry). Changed to `endsWith("lib.mjs")`. **Lesson:** a plugin needs a CLI-entry
   smoke, not only an import smoke.
2. **[FIXED] D2 over-scoping — `compiledIdsFromSheet` extracted INCIDENTAL corpus-id mentions.**
   Fix: return the compile's DESIGNATED governor (`GOVERNING PROCEDURE: <id>`) ALONE, then let
   chain-closure over `## Follow-on` supply the required hand-offs — which excludes conditional
   `## Escalation` procs (escalate-ticket) and incidental prose (audit-log). Verified via the
   live-hook smoke: `compiledIds=['onboard-vendor']` → `enforced=['onboard-vendor',
   'provision-account','grant-access']` (the clean vendor chain, matching the harness's
   applicable-scoped set). Original problem for the record:
   On the vendor turn the enforced set came out `[audit, escalate-ticket, onboard-vendor,
   provision-account, grant-access]` — `audit` + `escalate-ticket` leaked in because they are
   MENTIONED in the sheet body (an audit-log step, an escalation note), not because the compile
   chose them as governing. In the proven harness this was masked by `enforced = env.applicable
   ∩ sheet`; D2 dropped the authored applicable-set, so incidental mentions now over-broaden
   enforcement → over-block/cap-hit risk (a §n-adjacent regression, via the compile rather than
   the always-enforced tier). **Fix before the DoD run:** constrain `compiledIdsFromSheet` to
   the GOVERNING id (the `GOVERNING PROCEDURE: <id>` line) + explicit `## Follow-on` hand-offs
   only — NOT every corpus id that appears anywhere in the sheet text; optionally drop
   always_enforced metas from the sheet-derived set when the tier is off.

## ✅ DoD RESULT — the plugin-installed subject reproduces the enforcement (PROVEN)

Ran `run-plugin-dod.ts` (in the sc#784 spike dir): a real `claude -p` subject (claude-haiku-4-5)
with the SHIPPED plugin hooks installed as its `settings.json` hooks (h3's discarded), on the
`context-load-vendor` scenario, referee gpt-5.1. Run-data salvaged to
`.../spike-784-adherence/run-data/plugin-dod/<runId>/` BEFORE teardown.

**Enactability-immune enforcement proof (AC1a), from the RAW salvaged hook-log** — the vendor
(chain-closed) turn:
```
block  enforcedVia=sheet+chain blocked=[grant-access]  perProc: onboard=True provision=True grant-access=False
block  enforcedVia=sheet+chain blocked=[grant-access]  perProc: onboard=True provision=True grant-access=False
allow-complete enforcedVia=sheet+chain blocked=[]      perProc: onboard=True provision=True grant-access=True
```
The gate BLOCKED `grant-access` while it was `followed=False` (absent), the subject — forced by
the block — then made it `followed=True`, and only THEN did the gate allow. grant-access was done
AFTER the block, not before → **enforcement, not enactability**. Referee gpt-5.1: **3/3 = 1.00**;
the forced `grant_s_3307_confirm` binding with `expires_at:2027-…` is on disk. The plugin's compile
correctly named the governor (turn-4 `compiledIds=[onboard-vendor]`, the D2 fix live), and fired
+ gated correctly on the two DISTRACTOR task turns too (cert / archive) while allow-noop'ing the
no-procedure turn. **Open question answered:** the plugin's native `input.transcript_path` read DOES
see the subject's live actions under `claude -p` (the gate judged onboard/provision done +
grant-access not-done from the real action log). Simulation query-back confirmed via the MCP
(`platform_list_simulation_runs` setId spike-784-adherence, batch scenariobatch_3GNTuR…, 470.6s).

**Findings (honest — the DoD is proven; these are refinements):**
- **[cosmetic] Runner false-negative verdict.** `run-plugin-dod.ts`'s `forcedCompletionAfterBlock`
  check looked for the label `allow-complete-after-retry`, but the plugin logs `allow-complete`;
  its printed "DoD NOT reproduced" is wrong — the RAW hook-log (above) proves reproduction. Align
  the plugin's decision labels with the harness's, or the runner's check.
- **[FIXED] Retry counter not incrementing → cap defeated.** The two vendor-turn blocks both logged
  `priorBlocks=0`: the counter was keyed on `countHumanTurns(transcript_path)`, which a Stop-block
  re-injection perturbs → the counter reset to 0 every block → the retry cap (bounded-termination,
  AC5/AC7) never fired. Fix: the **compile hook resets** `state/<sid>.retry` to 0 (UserPromptSubmit
  fires exactly once per human turn — verified: `compile fires == human turns`, never on a Stop
  continuation), and the **gate only increments** it. Verified by a cap-test (compile-reset → gate
  4× on an unsatisfiable transcript): counter `0→1→2→3`, decisions `[block, block, block,
  allow-cap-hit]` — the cap terminates, no infinite loop. Offline smoke 15/15 green.
- **[FOLLOW-UP] Judge-span query-back needs `LANGWATCH_INGESTION_KEY`.** The DoD wrapper set only
  `LANGWATCH_API_KEY` (simulation → MCP-readable Vqci project); the referee `emitJudgeVerdict`
  OTEL span used `loadIngestionKey()` → the box key → landed in voice-bugbash (`get_trace` 404 from
  the MCP). Set `LANGWATCH_INGESTION_KEY` to the same MCP-project key for judge-span query-back.

## ✅ Code review (principles-reviewer) — folded

An independent correctness review (post-DoD) positively confirmed the load-bearing invariants
(D1 transcript parse: 432 tool_use extracted / 0 dropped on a real 2499-line session JSONL;
GPT-free + no-nested-`claude`: only `fetch api.anthropic.com`, zero `openai`/`claude -p`/`spawn`;
always-enforced default correctly `=== "1"`; exemption checked first; no path blocks blind). It
found **4 issues beyond the 3 already-fixed bugs — all now fixed + regression-guarded:**
1. **[Must, fixed] Stale-sheet cross-turn mis-enforcement.** A throttled compile did not clear the
   session sheet; since D2 made the sheet the sole scope, a stale prior-turn sheet would be enforced
   against the current turn. Fix: the throttle branch now clears the sheet (→ `allow-noop`).
2. **[Must, fixed] Incomplete D2 fix (escalate-ticket re-leak).** `compiledIdsFromSheet` preferred a
   `GOVERNING PROCEDURE:` header the compile prompt never mandated; when Haiku omitted it, the
   backtick fallback unioned EVERY id (re-leaking `escalate-ticket`). Fix: `buildCompileSystem` now
   REQUIRES `GOVERNING PROCEDURE: <id>` (or `none`) as the first line, and the fallback takes only the
   FIRST backtick id as governor. Regression-guarded in `test/smoke.mjs` (header / no-header / none).
3. **[Should, fixed] Harness-specific block text.** The block directive hardcoded the sc#784 `state/`
   layout; generalized to "the relevant project files".
4. **[Should, fixed] Dead code / config trap.** Removed `countHumanTurns`, `loadOpenAIKeyFromEnvFile`
   + its stale OpenAI comment, and the never-read `hookEnv` fields (`sheetFile`/`applicable`/
   `transcriptDir`/`judgeModel`/`openaiKey`) — the last were a silent-no-op trap for adopters who set
   the harness's `ADHERENCE_APPLICABLE`/`ADHERENCE_JUDGE_MODEL`.

Out-of-scope follow-ups the review noted (unchanged): whole-session action-log scope (D1 follow-up,
benign fail-open direction); D3 telemetry deferred (reconcile DESIGN §6 / ACs 13-14 wording); the
inherited `allow-complete-after-retry`-on-judge-error marker (faithful to the proven reference).

## Remaining (post-DoD)
- **DoD harness test** (`test bed = sc#784 harness`): wire the plugin's `compile.sh`/`gate.sh`
  into the vendor-scenario subject's `settings.json` (CLAUDE_PLUGIN_ROOT/CLAUDE_PROJECT_DIR/
  ADHERENCE_CORPUS_DIR=spike corpus/gate-model), run the vendor scenario; AC1a (gate fires
  block on grant-access, absent pre-block / present post-retry → 3/3) + AC1b (baseline flaky —
  already established §l) + DATA-FIRST run-data + LW query-back.
- Inline LangWatch telemetry (D3 — deferred; JSONL hook-log is the current run-data).
- PR review-ready, NEVER merge.
