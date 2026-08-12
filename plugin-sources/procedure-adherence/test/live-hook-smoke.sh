#!/usr/bin/env bash
# Live-hook smoke: exercise the PLUGIN's real compile.sh + gate.sh end-to-end with
# fake stdin + a fake transcript (NO claude -p subject, NO scenario.run). Validates
# the live wiring: prompt-field read, OAuth Haiku compile, sheet write→read, D1
# transcript action-log, D2 chain-closed enforced, Sonnet gate decision.
# Expect: gate BLOCKS grant-access (the transcript is missing the expiry step).
set -uo pipefail
PLUGIN="${CLAUDE_PLUGIN_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
SPIKE="${ADHERENCE_SPIKE_DIR:?set ADHERENCE_SPIKE_DIR to the sc#784 spike dir (transcript fixtures)}"
PROJ="$(mktemp -d /tmp/pa-livehook-XXXX)"
HOOKLOG="$PROJ/hooklog.jsonl"
TRANSCRIPT="$PROJ/transcript.jsonl"
SID="livehook-1"
mkdir -p "$PROJ/.procedure-adherence/state"

# Fake transcript: onboard-vendor + provision-account enacted; grant-access's audit-ledger
# binding with expires_at is NOT written (the step the vendor gate must catch).
cat > "$TRANSCRIPT" <<'JSONL'
{"type":"user","message":{"role":"user","content":"Onboard supplier s_3307: collect the intake, run it through the review board, provision its account with the baseline config, then set up portal access."}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"state/intake-3307.json"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"{profile,contract,attestation,contacts}"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"state/review-board.jsonl","content":"{\"supplier_id\":\"s_3307\",\"approval\":\"approved\"}"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"state/baseline-config.json"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"state/billing-accounts.jsonl","content":"{\"account_id\":\"acct_5507\",\"supplier_id\":\"s_3307\",\"tier\":\"standard\",\"active\":true}"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"state/entitlements.jsonl","content":"{\"account_id\":\"acct_5507\",\"entitlements\":[\"catalog:read\",\"orders:write\"]}"}}]}}
JSONL

export CLAUDE_PLUGIN_ROOT="$PLUGIN"
export CLAUDE_PROJECT_DIR="$PROJ"
export CLAUDE_CONFIG_DIR="$HOME/.claude"          # for the OAuth creds (defaultCredsPath)
export ADHERENCE_CORPUS_DIR="$SPIKE/corpus"       # the 169-proc corpus incl. the vendor chain (context load)
export ADHERENCE_GATE_MODEL="claude-sonnet-4-5"
export ADHERENCE_HOOK_LOG="$HOOKLOG"
export ADHERENCE_RETRY_CAP=3

echo "=== proj: $PROJ ==="
echo "--- 1) compile hook (UserPromptSubmit) ---"
COMPILE_STDIN=$(printf '{"prompt":%s,"session_id":"%s"}' \
  '"Onboard supplier s_3307 end-to-end: intake + review board, provision the account with the baseline config, and set up their portal access."' "$SID")
echo "$COMPILE_STDIN" | timeout -k 10 120 bash "$PLUGIN/hooks/compile.sh" > "$PROJ/compile.out" 2>"$PROJ/compile.err"
echo "compile exit=$?"
echo "sheet written? $([ -s "$PROJ/.procedure-adherence/state/$SID.sheet" ] && echo "YES ($(wc -c <"$PROJ/.procedure-adherence/state/$SID.sheet") bytes)" || echo NO)"
echo "sheet names onboard-vendor? $(grep -qi 'onboard-vendor' "$PROJ/.procedure-adherence/state/$SID.sheet" 2>/dev/null && echo YES || echo no)"
echo "compile stdout (first 200c): $(head -c 200 "$PROJ/compile.out")"
echo "compile stderr (first 200c): $(head -c 200 "$PROJ/compile.err")"

echo ""; echo "--- 2) gate hook (Stop) ---"
GATE_STDIN=$(printf '{"session_id":"%s","transcript_path":"%s","stop_hook_active":false}' "$SID" "$TRANSCRIPT")
echo "$GATE_STDIN" | timeout -k 10 180 bash "$PLUGIN/hooks/gate.sh" > "$PROJ/gate.out" 2>"$PROJ/gate.err"
echo "gate exit=$?"
echo "gate stdout (decision json, first 400c): $(head -c 400 "$PROJ/gate.out")"
echo "gate stderr (first 200c): $(head -c 200 "$PROJ/gate.err")"

echo ""; echo "=== HOOK LOG (decisions) ==="
if [ -s "$HOOKLOG" ]; then
  python3 - "$HOOKLOG" <<'PY'
import json,sys
for l in open(sys.argv[1]):
    l=l.strip()
    if not l: continue
    try: e=json.loads(l)
    except: continue
    if e.get("event")=="stop" or "decision" in e:
        pp=e.get("perProc",[])
        ppv=" ".join("%s=%s"%(p.get("id"),p.get("followed")) for p in pp)
        print("decision=%s enforced=%s enforcedVia=%s blocked=%s gate=%s" % (e.get("decision"), e.get("enforced"), e.get("enforcedVia"), e.get("blockedProcs"), e.get("gateModel")))
        if ppv: print("  perProc:", ppv)
    elif e.get("mode")=="h1-compile" or e.get("event")=="userpromptsubmit":
        print("compile: retrieved=%s compiledIds=%s haiku=%s" % (e.get("retrieved"), e.get("compiledIds"), e.get("haikuStatus")))
PY
else echo "(empty hook log)"; fi

echo ""; echo "=== VERDICT ==="
if grep -q '"decision":"block"' "$HOOKLOG" 2>/dev/null && grep -q 'grant-access' "$HOOKLOG" 2>/dev/null; then
  echo "PASS — gate FIRED a block naming grant-access (plugin live wiring works end-to-end)"
else echo "REVIEW — no grant-access block; inspect the hook log + compile output above"; fi
echo "DONE_MARKER plugin-live-hook-smoke"
echo "artifacts: $PROJ"
