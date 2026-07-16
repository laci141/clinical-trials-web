# 🔬 Clinical Trials Intelligence

A thin Go web wrapper around the [clinical-trials CLI](https://github.com/mvanhorn/printing-press-library): search, analyze, compare and monitor clinical trials across ClinicalTrials.gov, PubMed, OpenAlex and the FAERS safety database — with optional bring-your-own-key AI synthesis on top.

**Live instance:** [clinical-trials-web-1.onrender.com](https://clinical-trials-web-1.onrender.com)

## Features

- **6 command groups, 30+ commands** — search & discovery, trial analysis, comparison, recruiting & watch, data management, system
- **BYOK AI synthesis** — 12 LLM providers; your key, one HTTPS call, never stored
- **Live data sources** — ClinicalTrials.gov, PubMed, OpenAlex, FDA FAERS
- **In-process synthesis** — plain-language summary, key points, caveats over every result
- **Off-topic filter** — a conservative web-side relevance gate hides trials that don't match your query, transparently

## Architecture

- [`main.go`](main.go) — HTTP server, one endpoint per command group (`/api/search-discovery`, `/api/trial-analysis`, `/api/comparison`, `/api/recruiting-watch`, `/api/data-management`, `/api/system`), CLI exec via `runCLI`, relevance gate, CORS
- [`providers.go`](providers.go) — the 12-provider BYOK registry (OpenAI + Anthropic wire formats), prompt building, JSON compaction, error redaction
- [`index.html`](index.html) — single-file glass-panel UI (Bento grid, modals, AI synthesis panel)
- [`vendor-cli.sh`](vendor-cli.sh) — vendors the CLI source; prebuilt Linux binary in `bin/` ships in the image
- [`Dockerfile`](Dockerfile) — multi-stage build; [`render.yaml`](render.yaml) — [Render](https://render.com) deploy

The CLI itself is purely heuristic and keyless — **all** LLM work happens in the web layer as a post-processing step over the CLI's JSON.

## Quick start

```bash
# Local (serves on 127.0.0.1:8091; override with ADDR or PORT)
go build ./... && go run .

# Docker
docker build -t ctw . && docker run -p 8091:8091 ctw

# Render: push to main — auto-deploys
```

## Usage examples

| In the UI | What happens |
|---|---|
| `search` → *diabetes* | FTS search, off-topic trials filtered + listed, AI synthesis with a key |
| `risk` → *NCT07437157* | heuristic risk read (score, level, factors) |
| `compare` → *aspirin* vs *ibuprofen* | side-by-side cards (2,185 vs 1,099 trials) + LLM comparison |
| `recruiting` → *heart disease*, limit 20 | 1,955+ matches, top 20, synthesized |
| `health` | data-source status for all four backends |
| BYOK model field | pre-filled per provider (`deepseek-chat`, `openrouter/free`, …), fully editable |

## What's new this sprint

- **In-process BYOK LLM synthesis** — 12 providers: openai, anthropic, gemini, groq, mistral, deepseek, zai, moonshot, qwen, minimax, xai, openrouter. 60s timeout, redacted errors, graceful degradation: the CLI JSON is always returned verbatim; an LLM failure yields the heuristic result plus `llm_error`, never a 500.
- **Conservative relevance gate** — the query is tokenized (lowercase, accent-folded, stopwords dropped) and matched against each trial's `conditions[]`, title and `interventions[]`. Off-topic trials move to a 🚫 details panel instead of vanishing; an empty query disables the gate entirely.
- **Glass-panel UI** — 2.5× stronger `body::before` radial gradients so the frosted blur actually shows color, hero hover affordance, `-webkit-backdrop-filter` everywhere, mobile blur(10px) with reduced padding.
- **Model auto-fill** — provider defaults arrive as real editable values; a custom model is never clobbered when switching providers.
- **NCT guardrail** — `/^NCT\d{8}$/` checked client-side: strict for risk/forecast/enrollment-check/similar/timeline/export-fhir, lenient for evidence (free-text terms welcome).
- **No empty-field dead-ends** — every field opens pre-filled (Drug A *aspirin* / Drug B *ibuprofen*, Resource locked to the CLI's only valid value `studies`, all NCT examples `NCT07437157`, verified live).
- **Compare readability** — `.rcard.compare-side` gets a 4× stronger background so the side-by-side cards stay readable over the brighter glass.

## Security

The BYOK key travels in the `X-LLM-Key` request header, lives in memory for one request, and goes into exactly **one** outbound HTTPS call to the provider you chose. It is never logged, never persisted, never written to any environment — `buildChildEnv` strips every provider env var, so the child CLI is always keyless. Any error string that could echo the key passes through `redact()` / `sanitizeLLMError()` before reaching a client.

## Known limits

- `openrouter/free` is rate-limited (~50 requests/day, then HTTP 429) — switch model or provider
- OpenAlex can be slow at peak; use a lower limit (e.g. 12) if requests time out
- Early-phase trials often have small sample sizes — the synthesis flags this in caveats, but read the raw data
- Informational only — **not medical advice**

## Development

- Git: single `main` branch; Render auto-deploys on push
- Tests: `go test ./...` — 18 tests, no network (7 relevance-gate tests in [`main_test.go`](main_test.go), 11 LLM-layer tests in [`providers_test.go`](providers_test.go))
- `go build ./...` and `go vet ./...` clean; do not run `gofmt -w` on Windows checkouts (CRLF)

## Future

Phase 3 multi-model consensus · abstracts for gaps/evidence · Patent CLI · Grants CLI

## Attribution

Built on the [Printing Press Library](https://github.com/mvanhorn/printing-press-library) CLI. Web layer: [github.com/laci141/clinical-trials-web](https://github.com/laci141/clinical-trials-web).
