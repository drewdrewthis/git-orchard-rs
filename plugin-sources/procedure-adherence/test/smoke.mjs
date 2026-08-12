/**
 * Offline smoke — 0 bucket, no scenario.run. Validates the plugin's pure functions
 * + the D1 transcript action-log extraction + a mocked-fetch gate-judge round-trip.
 * De-risks before the bucket-spending DoD harness run. Run: `node test/smoke.mjs`.
 */
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";

import {
  readActionLogFromTranscript,
  loadCorpus,
  closeEnforcedChain,
  compiledIdsFromSheet,
  parseFrontmatter,
  parseProcedureSteps,
  stepIsMutating,
  alwaysEnforcedIds,
  callHaiku,
  buildPerProcJudgeSystem,
  buildPerProcJudgeUser,
  parsePerProcVerdict,
  isExempt,
  emitJudgeVerdict,
  resolveGateAuth,
  scrubForEgress,
} from "../hooks/lib.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const CORPUS = join(HERE, "..", "corpus");
let pass = 0, fail = 0;
const ok = (name, cond, detail = "") => { if (cond) { pass++; console.log(`  ok   ${name}`); } else { fail++; console.log(`  FAIL ${name} ${detail}`); } };

// ---- AC4: D1 transcript positive control (extracted count == K) ----
{
  const tmp = mkdtempSync(join(tmpdir(), "pa-smoke-"));
  const tp = join(tmp, "transcript.jsonl");
  const K = 3;
  const rows = [
    { type: "user", message: { role: "user", content: "onboard the vendor" } },
    { type: "assistant", message: { role: "assistant", content: [{ type: "tool_use", name: "Read", input: { file_path: "state/intake.json" } }] } },
    { type: "user", message: { role: "user", content: [{ type: "tool_result", content: "{...}" }] } },
    { type: "assistant", message: { role: "assistant", content: [{ type: "tool_use", name: "Write", input: { file_path: "state/accounts.jsonl" } }] } },
    { type: "user", message: { role: "user", content: [{ type: "tool_result", content: "ok" }] } },
    { type: "assistant", message: { role: "assistant", content: [{ type: "tool_use", name: "Edit", input: { file_path: "state/ledger.jsonl" } }] } },
    { type: "user", message: { role: "user", content: [{ type: "tool_result", content: "ok" }] } },
  ];
  writeFileSync(tp, rows.map((r) => JSON.stringify(r)).join("\n"));
  const res = readActionLogFromTranscript(tp);
  ok("D1: non-empty action log", res && res.log.length > 0);
  ok(`D1: extracted toolUses == K(${K})`, res && res.toolUses === K, `got ${res && res.toolUses}`);
  ok("D1: log carries tool names in order", res && /Read[\s\S]*Write[\s\S]*Edit/.test(res.log));
  ok("D1: missing transcript → null (fail-open)", readActionLogFromTranscript(join(tmp, "nope.jsonl")) === null);
  // Review fix (principles#1): a readable but shape-drifted/empty transcript (no
  // message-shaped line parsed) → null (no-substrate → fail-OPEN), NOT a truthy
  // {log:""} object that would fall through to the judge and block-churn to cap.
  const driftTp = join(tmp, "drift.jsonl");
  writeFileSync(driftTp, '{"foo":"bar"}\n{"note":"no message field"}\n');
  ok("review: shape-drift transcript → null (fail-open, not block-churn)", readActionLogFromTranscript(driftTp) === null);
}

// ---- corpus + chain closure (AC5 incl. escalate-ticket negative control) ----
{
  const corpus = loadCorpus(CORPUS);
  const ids = corpus.map((c) => c.id).sort();
  ok("corpus loads bundled procs", ids.includes("onboard-vendor") && ids.includes("grant-access"), ids.join(","));
  const byId = new Map(corpus.map((c) => [c.id, c]));
  const allIds = corpus.map((c) => c.id);
  // A sheet that names ONLY the chain root:
  const sheet = "Follow procedure `onboard-vendor`.";
  const seed = compiledIdsFromSheet(sheet, corpus);
  const closed = closeEnforcedChain(seed, allIds, byId).sort();
  ok("AC5: chain-closure re-adds dropped hops", JSON.stringify(closed) === JSON.stringify(["grant-access", "onboard-vendor", "provision-account"]), closed.join(","));
  ok("AC5: chain-closure does NOT pull escalate-ticket", !closed.includes("escalate-ticket"));
  // D2 governor-scoping (review fix): header path, no-header fallback = FIRST backtick
  // (never union — else escalate-ticket re-leaks), and "none".
  ok("D2: GOVERNING PROCEDURE header → governor alone", JSON.stringify(compiledIdsFromSheet("GOVERNING PROCEDURE: onboard-vendor\nsteps", corpus)) === JSON.stringify(["onboard-vendor"]));
  ok("D2: no-header fallback = first backtick, NOT +escalate-ticket", JSON.stringify(compiledIdsFromSheet("Follow `onboard-vendor`. If blocked, escalate via `escalate-ticket`.", corpus)) === JSON.stringify(["onboard-vendor"]));
  ok("D2: GOVERNING PROCEDURE: none → []", compiledIdsFromSheet("GOVERNING PROCEDURE: none", corpus).length === 0);
  // Review fix (principles#2): ids are captured case-insensitively (/i) but the
  // byId map is lowercase-keyed — a capitalized governor must normalize, not miss.
  ok("review: capitalized governor normalizes to lowercase id (no silent miss)", JSON.stringify(compiledIdsFromSheet("GOVERNING PROCEDURE: Onboard-Vendor\nsteps", corpus)) === JSON.stringify(["onboard-vendor"]));
}

// ---- per-agent exemption: the pre-install brick-a-worker guard (Step 2) ----
// isExempt is the FIRST line of both runCompile and runGate (before any model
// call / block), so a proven-exempt session is untouched by the gate.
{
  const tmp = mkdtempSync(join(tmpdir(), "pa-exempt-"));
  const exemptFile = join(tmp, "exempt");
  writeFileSync(exemptFile, "sess-exempt-1\nsess-exempt-2\n");
  const env = { exemptFile };
  delete process.env.ADHERENCE_EXEMPT;
  // (a) global env escape hatch exempts ANY session
  process.env.ADHERENCE_EXEMPT = "1";
  ok("exempt: ADHERENCE_EXEMPT=1 → exempt (any session)", isExempt({ session_id: "whatever" }, env) === true);
  delete process.env.ADHERENCE_EXEMPT;
  // (b) session id listed in the exempt file → exempt
  ok("exempt: session_id listed in exempt file → exempt", isExempt({ session_id: "sess-exempt-2" }, env) === true);
  // (c) unlisted session, no env → NOT exempt (the gate runs)
  ok("exempt: unlisted session + no env → NOT exempt (gate active)", isExempt({ session_id: "sess-other" }, env) === false);
  // (d) no exempt file + no env → NOT exempt (default is enforce, not skip)
  ok("exempt: no exempt file + no env → NOT exempt", isExempt({ session_id: "x" }, { exemptFile: join(tmp, "nope") }) === false);
}

// ---- D3 telemetry: key-gated fire-and-forget judge-verdict emit ----
{
  const emit = {
    decision: "block", gateModel: "claude-sonnet-4-5", blocked: true, blockedProcs: ["grant-access"],
    enforcedVia: "sheet+chain", followedCount: 2, applicableCount: 3, adherenceRate: 2 / 3,
    perProc: [{ id: "grant-access", followed: false, reasoning: "expiry step absent" }],
  };
  // (a) mocked-fetch emit path: POSTs the renamed span with the Bearer key
  let captured = null;
  const mockFetch = async (url, init) => {
    captured = { url, init };
    return { ok: true, status: 200, text: async () => JSON.stringify({ partialSuccess: {} }) };
  };
  const res = await emitJudgeVerdict(emit, {
    key: "ik-lw-test", fetchImpl: mockFetch, genTraceId: () => "trace123", genSpanId: () => "span456",
  });
  ok("D3: emit returns {emitted:true, traceId} on 200 + zero rejectedSpans", res.emitted === true && res.traceId === "trace123");
  ok("D3: POSTs /v1/traces with Authorization: Bearer <key>", !!captured && captured.url.endsWith("/v1/traces") && captured.init.headers.Authorization === "Bearer ik-lw-test");
  const span = JSON.parse(captured.init.body).resourceSpans[0].scopeSpans[0];
  ok("D3: scope+span renamed sc784.* → procedure_adherence.*", span.scope.name === "procedure_adherence.judge" && span.spans[0].name === "procedure_adherence.judge.verdict");
  const attrs = Object.fromEntries(span.spans[0].attributes.map((a) => [a.key, a.value]));
  ok("D3: span carries adherence.rate + blocked + per_procedure", "adherence.rate" in attrs && attrs["adherence.blocked"].boolValue === true && "adherence.per_procedure" in attrs);
  // (b) no key → {emitted:false, no-ingestion-key} with ZERO fetch (fail-open, never slows the turn)
  let fetchedNoKey = false;
  const res2 = await emitJudgeVerdict(emit, {
    loadKey: () => undefined,
    fetchImpl: async () => { fetchedNoKey = true; return { ok: true, status: 200, text: async () => "{}" }; },
  });
  ok("D3: no key → {emitted:false, no-ingestion-key} + ZERO fetch", res2.emitted === false && res2.reason === "no-ingestion-key" && fetchedNoKey === false);
}

// ---- design#2 fix: auth resolves from the env surface, not OAuth-only ----
{
  const saved = { k: process.env.ANTHROPIC_API_KEY, t: process.env.ANTHROPIC_AUTH_TOKEN };
  delete process.env.ANTHROPIC_API_KEY;
  delete process.env.ANTHROPIC_AUTH_TOKEN;
  const savedBase = process.env.ANTHROPIC_BASE_URL;
  process.env.ANTHROPIC_BASE_URL = "https://gw.example.com";
  // (a) ANTHROPIC_API_KEY → x-api-key, no oauth beta / spoof, and HONORS ANTHROPIC_BASE_URL
  process.env.ANTHROPIC_API_KEY = "sk-ant-test";
  const a = resolveGateAuth("/nonexistent");
  ok("auth: ANTHROPIC_API_KEY → x-api-key, no oauth-beta, spoof=false", a.headers["x-api-key"] === "sk-ant-test" && !a.headers["anthropic-beta"] && a.spoof === false);
  ok("auth: x-api-key branch honors ANTHROPIC_BASE_URL", a.baseUrl === "https://gw.example.com");
  delete process.env.ANTHROPIC_API_KEY;
  // (b) ANTHROPIC_AUTH_TOKEN → Bearer + oauth beta + spoof, PINNED to canonical (ignores BASE_URL)
  process.env.ANTHROPIC_AUTH_TOKEN = "tok-login";
  const b = resolveGateAuth("/nonexistent");
  ok("auth: ANTHROPIC_AUTH_TOKEN → Bearer + oauth-beta, spoof=true", b.headers.Authorization === "Bearer tok-login" && b.headers["anthropic-beta"] === "oauth-2025-04-20" && b.spoof === true);
  ok("auth [security]: Bearer branch PINNED to api.anthropic.com, ignores ANTHROPIC_BASE_URL", b.baseUrl === "https://api.anthropic.com");
  delete process.env.ANTHROPIC_AUTH_TOKEN;
  // (c) no env → OAuth creds-file fallback; missing file → {error} (fail-open path preserved)
  const c = resolveGateAuth("/nonexistent-creds.json");
  ok("auth: no env + no creds file → {error}, OAuth path preserved", typeof c.error === "string" && !c.headers);
  if (savedBase === undefined) delete process.env.ANTHROPIC_BASE_URL;
  else process.env.ANTHROPIC_BASE_URL = savedBase;
  if (saved.k) process.env.ANTHROPIC_API_KEY = saved.k;
  if (saved.t) process.env.ANTHROPIC_AUTH_TOKEN = saved.t;
}

// ---- review [security, Medium]: judge reasoning is scrubbed before egress ----
{
  const s = scrubForEgress("granted access; token sk-lw-abcdef1234567890 for alice@example.com");
  ok("scrub: redacts provider key + email before egress", s.includes("[REDACTED]") && !s.includes("sk-lw-abcdef1234567890") && !s.includes("alice@example.com"));
  ok("scrub: caps length (defense-in-depth)", scrubForEgress("x".repeat(900)).length <= 520);
}

// ---- frontmatter + steps + always_enforced tier ----
{
  const corpus = loadCorpus(CORPUS);
  const onboard = corpus.find((c) => c.id === "onboard-vendor");
  const steps = parseProcedureSteps(onboard.body);
  ok("parseProcedureSteps finds numbered steps", steps.length >= 1, `n=${steps.length}`);
  ok("stepIsMutating classifies a write step", steps.some((s) => stepIsMutating(s.text)) || steps.length === 0);
  const always = alwaysEnforcedIds(corpus);
  ok("always_enforced tier = only satisfiable metas (done), not prove/improvise-lookup", !always.includes("prove") && !always.includes("improvise-lookup"), always.join(","));
}

// ---- mocked-fetch gate-judge round-trip (no bucket) ----
{
  const realFetch = global.fetch;
  global.fetch = async () => ({ ok: true, status: 200, headers: { get: () => null }, json: async () => ({ content: [{ type: "text", text: '{"followed":false,"missingSteps":["set expires_at"],"reasoning":"no ledger write with expiry"}' }] }) });
  // Dummy creds file so the offline path never depends on host credentials.
  const credsDir = mkdtempSync(join(tmpdir(), "pa-smoke-creds-"));
  const credsPath = join(credsDir, ".credentials.json");
  writeFileSync(credsPath, JSON.stringify({ claudeAiOauth: { accessToken: "smoke-test-token" } }));
  try {
    const res = await callHaiku(buildPerProcJudgeSystem(), buildPerProcJudgeUser("grant-access", "1. write the binding with expires_at", "#1 tool_use Read"), { model: "claude-sonnet-4-5", credentialsPath: credsPath });
    ok("gate judge: callHaiku returns ok on mocked 200", res.ok === true, JSON.stringify(res).slice(0, 80));
    const v = res.ok ? parsePerProcVerdict(res.text) : null;
    ok("gate judge: parsePerProcVerdict → followed=false", v && v.followed === false);
  } finally { global.fetch = realFetch; }
}

console.log(`\nSMOKE: ${pass} passed, ${fail} failed`);
process.exit(fail === 0 ? 0 : 1);
