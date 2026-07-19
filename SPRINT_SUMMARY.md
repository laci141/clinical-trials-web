# clinical-trials-web Sprint Summary — July 9–16, 2026

Internal recap for future reference. Not a marketing doc — the numbers and caveats below are
the real ones. (Git-history note for honesty: the repo was born 2026-07-11 with the initial
6-group UI; the entire LLM/gate/UI sprint below landed on 2026-07-16 in one long session.)

## 1. The Problem (starting point)

clinical-trials-web existed and looked functional, but the BYOK LLM key was **decorative**:
the server injected `X-LLM-Key` into the child CLI's environment, and the clinical-trials CLI
is 100% heuristic — it never reads a provider key and never calls an LLM. The key did nothing.
On top of that, the CLI's broad FTS search returned noisy results: a "diabetes" query came back
with Preterm Birth, Parkinson and Hepatitis C trials mixed in, and several commands opened with
empty fields that produced instant "required"/"unknown resource" errors.

## 2. Solution delivered

- **In-process BYOK LLM synthesis** (pattern ported from scientific-consensus-web):
  `providers.go` with 12 providers — openai, anthropic, gemini, groq, mistral, deepseek, zai,
  moonshot, qwen, minimax, xai, openrouter. Key lives in memory for one request and one
  outbound HTTPS header; 60s `llmTimeout`; graceful degradation — the CLI JSON is always
  returned verbatim, an LLM failure only adds a redacted `llm_error`. New generic schema:
  `{summary, key_points, caveats, not_advice}` with the disclaimer enforced server-side.
- **Conservative relevance gate** (`main.go`, search only): query tokenized (lowercase,
  accent-folded, 3-char floor, stopword set), matched as substring against `conditions[]`,
  title, and `interventions[]`. In live use it removed 8–87 off-topic trials per search.
  Dropped trials move to `filtered_out` + a 🚫 details panel — transparent, never silent.
  Zero content tokens (empty/short query) disables the gate entirely; any parse doubt passes
  the JSON through unchanged.
- **Glass-panel UI overhaul**: body::before radial gradients raised ~2.5× (alphas .12–.24 →
  .40–.55, spreads to 55–60%) so the frosted blur finally shows color; transparent hero;
  search-box hover affordance; `-webkit-backdrop-filter` paired everywhere (Safari); mobile
  blur(10px) + reduced padding.
- **Model auto-fill**: provider defaults (deepseek-chat, openrouter/free, …) arrive as real
  editable values via a `knownDefaults` set — a user-custom model is never clobbered on
  provider switch or reload.
- **NCT guardrail**: client-side `/^NCT\d{8}$/` — strict for risk/forecast/enrollment-check/
  similar/timeline/export-fhir; lenient for evidence, whose "NCT ID or term" field accepts
  free text (semaglutide must pass), blocking only malformed NCT-looking attempts.
- **Dead-end elimination**: every field opens pre-filled (Compare: aspirin/ibuprofen;
  Data Management Resource: a `studies`-only dropdown — the CLI's sole valid value; all NCT
  examples switched to the live-verified NCT07437157). sync/import/tail hidden from the UI
  (ephemeral disk / endless polling); server still routes them.
- **Side-card readability**: `.rcard.compare-side` at 4× background opacity so the compare
  cards stay legible over the brighter glass — scoped, other `.rcard` uses untouched.

## 3. Commits (8 total, 1 on 07-11 + 7 on 07-16)

| Hash | What |
|---|---|
| `89032b9` (07-11) | initial 6-group UI + BYOK-env + Docker |
| `7b4dd2b` | LLM synthesis layer, 12 providers, providers_test.go |
| `6ade972` | Docker fix — build the whole package (providers.go was missing from COPY) |
| `5e8a36a` | relevance gate (⚠️ main_test.go missed — see §4) |
| `36c2812` | hero glass visible + model auto-fill |
| `8f4ce30` | glass 2.5×, dead-end fixes, NCT guardrail |
| `e813e21` | compare side-card readability |
| `d1ea658` | README.md |
| *(pending)* | SPRINT_SUMMARY.md + **main_test.go** |

## 4. Test coverage — with one open action item

- Locally: **18 Go tests green** (7 relevance-gate in `main_test.go` + 11 LLM-layer in
  `providers_test.go`), all network-free; `go build`, `go vet`, `go test` clean on every step.
- JS syntax verified on every index.html change (extracted-script check), plus a DOM-stub vm
  harness for the model auto-fill logic (9/9) — harness lived in the session scratchpad, not
  the repo.
- ⚠️ **Action item:** `5e8a36a` committed only `index.html` + `main.go`; `main_test.go` is
  still untracked. A fresh clone runs 11 tests, and the README's "18 tests" claim is only
  true locally until `main_test.go` ships — include it in the next commit with this file.

## 5. Live validation (Render, manual)

[pubvera-trialvera.onrender.com](https://pubvera-trialvera.onrender.com):
search *diabetes* → 87 off-topic filtered, clean synthesis, 🚫 panel transparent;
recruiting *heart disease* → 3,997 matches, top 10 synthesized; compare aspirin/ibuprofen →
readable side-by-side with trial counts/phases/top sponsors; risk/forecast/timeline correct
keyless; health → 6/7 sources ok (OpenAlex occasionally down).

## 6. Architecture decisions (why this way)

- **Web-side gate**, not a CLI patch: immediate, no monorepo PR, no regeneration risk, and
  deliberately conservative — unparseable trials are kept, doubt = pass-through verbatim.
- **In-process LLM**, not a sidecar: no extra service, keys never logged/persisted/env'd,
  and degradation is trivially correct because the CLI result never depends on the LLM.
- **Scoped UI fixes** (`.rcard.compare-side`): the readability problem was local to compare;
  a global change would have restyled search/recruiting/risk cards that already looked right.
- **Lenient evidence guardrail**: its backend accepts terms; a strict check would have created
  a new dead-end — the exact thing the sprint was removing.

## 7. Known limits & next steps

- `openrouter/free`: ~50 requests/day, then 429 until reset — switch provider or add credits.
- OpenAlex slow at peak → use limit ≈12; CLI fetch time dominates the LLM call.
- Multi-model consensus (Phase 3): not built — needs parallel LLM calls + merge logic; deferred.
- Gaps/Evidence-style abstracts + gate for more commands: not urgent, pattern exists in sciweb.
- Patent / Grants CLI wrappers: planned, lower priority.

## 8. Code-quality conventions held

No `gofmt -w` (CRLF on Windows); explicit-path staging only, never `git add -A` (no `.claude/`
or backup leakage); all repo tests network-free; live verification happens on Render after push.

## 9. What worked

Porting the proven sciweb pattern nearly wholesale (providers.go, redact, extractBYOK shape);
the empty-query fallback making the gate regression-proof by construction; scoped CSS fixes;
Fable 5 driving the long multi-file turns with review checkpoints before every push; the
Git Bash + Claude Code loop (user pushes, model stops at the diff) — zero conflicts all sprint.

## 10. What would change next time

- Have the glass-panel skill on disk from day one — it was referenced before it existed and
  had to be reconstructed from prompt rules mid-sprint.
- Verify NCT examples against the live API **before** hardcoding them (NCT07104383 shipped
  dead in the initial build).
- Know the openrouter/free rate limit upfront and pick the default provider accordingly.
- Decide the web-visible command set (import/sync/tail out) at design time, not mid-sprint.
- Commit test files in the same commit as the code they cover — the untracked `main_test.go`
  is exactly the failure mode explicit-path staging invites. Check `git status` before push.

## 11. Lessons learned

- A relevance gate is safe to ship when "no signal" means "do nothing": tokenize, and make the
  empty case a byte-identical pass-through.
- LLM synthesis belongs in the web layer: iteration is a redeploy, not a CLI regeneration, and
  failures degrade instead of breaking the data path.
- Glass needs light: blur over a flat dark background is invisible; the 2.5× alpha raise was
  the difference between "flat box" and "glass".
- Defaults are UX: pre-filling every field removed an entire class of first-click errors.
