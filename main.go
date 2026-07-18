// Command clinical-trials-web is a thin standalone HTTP wrapper around the
// clinical-trials CLI. It exposes ONE endpoint per command group (6 total) and
// routes to the ~30 CLI subcommands, assembling each command's real positional
// arguments (verified from `--help`, NOT the guessed flag names). The CLI is
// always keyless/heuristic — it never calls an LLM. When the caller supplied a
// BYOK (bring-your-own-key) key, this server makes ONE in-process
// chat-completions call (providers.go) to synthesize the CLI's JSON into a
// plain-language summary. Each caller uses their own LLM key; the server never
// holds a key of its own.
//
// SECURITY MODEL (enforced below and in providers.go, do not weaken):
//   - The BYOK key arrives in the X-LLM-Key request header and lives in memory
//     only for the duration of one request and one outbound HTTPS call.
//   - The key is NEVER logged, printed, persisted, written to the server's own
//     process environment, or passed to the child CLI. buildChildEnv() strips
//     every known provider key out of os.Environ(), so a key set in the
//     server's own environment can never leak into the child either.
//   - Any CLI stderr or LLM provider diagnostic surfaced to the client passes
//     through redact()/sanitizeLLMError(), which remove the key substring so a
//     key echoed in an error can never escape.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
)

func cliBinaryPath() string {
	if p := strings.TrimSpace(os.Getenv("CLI_BIN")); p != "" {
		return p
	}
	name := "clinical-trials-pp-cli"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join("bin", name)
}

// allProviderEnvVars is every provider key env var a CLI might conceivably
// read. buildChildEnv strips ALL of them from the inherited environment so the
// child never sees any provider key — the child is always keyless; LLM calls
// happen in-process (providers.go).
var allProviderEnvVars = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"GROQ_API_KEY",
	"MISTRAL_API_KEY",
}

// request carries every field any group body might send. Each command's spec
// picks the ones it needs as ordered positional arguments.
type request struct {
	Command   string `json:"command"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Query     string `json:"query"`
	Nctid     string `json:"nctid"`
	Condition string `json:"condition"`
	Drug1     string `json:"drug1"`
	Drug2     string `json:"drug2"`
	Area      string `json:"area"`
	Term      string `json:"term"`
	Drug      string `json:"drug"`
	Resource  string `json:"resource"`
	ID        string `json:"id"`
	File      string `json:"file"`
	Format    string `json:"format"`
	Interface string `json:"interface"`
	Text      string `json:"text"`
	Limit     int    `json:"limit"`
}

// firstNonEmpty returns the first non-blank of the given values (trimmed).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// cmdSpec describes how to turn a request into a CLI invocation for one command.
//   build   -> ordered positional args (after the subcommand); error if a
//              required input is missing.
//   limit   -> append --limit <n> when the caller supplied one.
//   format  -> append --format <v> when the caller supplied one (only for the
//              commands that actually accept it).
type cmdSpec struct {
	build  func(r request) ([]string, error)
	limit  bool
	format bool
}

// target resolves the single free-text/identifier positional shared by the
// analysis commands (nct-id, condition, drug, or query — whatever was sent).
func target(r request) string {
	return firstNonEmpty(r.Nctid, r.Condition, r.Query, r.Drug, r.Term, r.Area)
}

func req1(name, val string) ([]string, error) {
	if strings.TrimSpace(val) == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	return []string{strings.TrimSpace(val)}, nil
}

// groups maps each group endpoint to the commands it may run. This is also the
// allow-list: /api/system can never invoke `search`.
var groups = map[string]map[string]cmdSpec{
	"search-discovery": {
		"search":   {build: func(r request) ([]string, error) { return req1("query", r.Query) }, limit: true},
		"digest":   {build: func(r request) ([]string, error) { return req1("query", firstNonEmpty(r.Query, r.Term)) }, limit: true},
		"similar":  {build: func(r request) ([]string, error) { return req1("nct-id", firstNonEmpty(r.Nctid, r.Query)) }, limit: true},
		"hotspots": {build: func(r request) ([]string, error) { return req1("condition", firstNonEmpty(r.Condition, r.Query)) }},
		"phase3":   {build: func(r request) ([]string, error) { return req1("condition", firstNonEmpty(r.Condition, r.Query)) }, limit: true},
	},
	"trial-analysis": {
		"risk":             {build: func(r request) ([]string, error) { return req1("nct-id", target(r)) }},
		"forecast":         {build: func(r request) ([]string, error) { return req1("nct-id", target(r)) }},
		"enrollment-check": {build: func(r request) ([]string, error) { return req1("nct-id", target(r)) }},
		"evidence":         {build: func(r request) ([]string, error) { return req1("nct-id-or-term", target(r)) }, limit: true},
		"safety":           {build: func(r request) ([]string, error) { return req1("drug", firstNonEmpty(r.Drug, r.Condition, r.Query, r.Nctid)) }, limit: true},
		"timeline":         {build: func(r request) ([]string, error) { return req1("nct-id", target(r)) }},
	},
	"comparison": {
		"compare": {build: func(r request) ([]string, error) {
			a, b := strings.TrimSpace(r.Drug1), strings.TrimSpace(r.Drug2)
			if a == "" || b == "" {
				return nil, errors.New("drug1 and drug2 are required")
			}
			return []string{a, b}, nil
		}},
		"sponsors": {build: func(r request) ([]string, error) { return req1("area", firstNonEmpty(r.Area, r.Query, r.Condition)) }, limit: true},
		"velocity": {build: func(r request) ([]string, error) { return req1("area", firstNonEmpty(r.Area, r.Query, r.Condition)) }},
	},
	"recruiting-watch": {
		"recruiting": {build: func(r request) ([]string, error) { return req1("condition", firstNonEmpty(r.Condition, r.Query)) }, limit: true},
		"watch":      {build: func(r request) ([]string, error) { return req1("term", firstNonEmpty(r.Term, r.Condition, r.Query)) }},
		// tail streams by polling; over request/response it runs until the
		// server's context timeout. Routed for completeness — resource optional.
		"tail": {build: func(r request) ([]string, error) {
			if s := firstNonEmpty(r.Resource, r.Query); s != "" {
				return []string{s}, nil
			}
			return []string{}, nil
		}},
	},
	"data-management": {
		// sync/import/export mutate the local SQLite DB or read server-side
		// files; they are routable but intended for local/admin use.
		"sync": {build: func(r request) ([]string, error) {
			if s := firstNonEmpty(r.Query, r.Term, r.Condition); s != "" {
				return []string{s}, nil
			}
			return []string{}, nil
		}},
		"export":      {build: func(r request) ([]string, error) { return req1("resource", r.Resource) }, limit: true, format: true},
		"export-fhir": {build: func(r request) ([]string, error) { return req1("nct-id", target(r)) }, format: true},
		"import":      {build: func(r request) ([]string, error) { return req1("resource", r.Resource) }},
	},
	"system": {
		"health":                     {build: noArgs},
		"doctor":                     {build: noArgs},
		"analytics":                  {build: noArgs},
		"agent-context":              {build: noArgs},
		"version":                    {build: noArgs},
		"clinicaltrials-gov-version": {build: noArgs},
		"api":                        {build: func(r request) ([]string, error) { return optArg(r.Interface), nil }},
		"feedback":                   {build: func(r request) ([]string, error) { return optArg(r.Text), nil }},
	},
}

func noArgs(request) ([]string, error) { return []string{}, nil }
func optArg(v string) []string {
	if s := strings.TrimSpace(v); s != "" {
		return []string{s}
	}
	return []string{}
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	for group := range groups {
		g := group
		mux.HandleFunc("/api/"+g, func(w http.ResponseWriter, r *http.Request) { handleGroup(w, r, g) })
	}

	addr := "127.0.0.1:8091"
	if a := strings.TrimSpace(os.Getenv("ADDR")); a != "" {
		addr = a
	} else if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		addr = "0.0.0.0:" + p
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("clinical-trials-web listening on %s (CLI: %s)", addr, cliBinaryPath())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "/index.html":
		if data, err := os.ReadFile("index.html"); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		fallthrough
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	default:
		http.NotFound(w, r)
	}
}

// setCORS adds the CORS headers every /api response needs. Browsers refuse
// fetch() responses without these ("Failed to fetch") even same-origin in some
// embed/proxy setups, and error responses need them too or the browser hides
// the JSON error body.
func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-LLM-Key, X-LLM-Provider")
}

// preflight handles the CORS preflight OPTIONS request. Returns true when the
// request was a preflight and has been fully answered.
func preflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	w.WriteHeader(http.StatusOK)
	return true
}

// noLLMGroups / noLLMCmds mark where the LLM synthesis is skipped even with a
// key: data-management (sync/import/export/export-fhir) moves data around and
// tail polls a stream — a summary adds nothing over the raw output there.
var noLLMGroups = map[string]bool{"data-management": true}
var noLLMCmds = map[string]bool{"tail": true}

func handleGroup(w http.ResponseWriter, r *http.Request, group string) {
	setCORS(w)
	if preflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	cmds := groups[group]

	var req request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	cmd := strings.TrimSpace(req.Command)
	spec, ok := cmds[cmd]
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown command %q for group %q; allowed: %s",
			cmd, group, strings.Join(sortedKeys(cmds), ", ")))
		return
	}

	pos, err := spec.build(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Assemble argv: <command> <positional...> --json [--limit N] [--format F].
	args := append([]string{cmd}, pos...)
	args = append(args, "--json")
	if spec.limit && req.Limit > 0 {
		if req.Limit > 200 {
			req.Limit = 200
		}
		args = append(args, "--limit", fmt.Sprintf("%d", req.Limit))
	}
	if spec.format && strings.TrimSpace(req.Format) != "" {
		args = append(args, "--format", strings.TrimSpace(req.Format))
	}

	b, ok := extractBYOK(w, r, req.Provider, req.Model)
	if !ok {
		return
	}
	runCLI(w, r, b, group, cmd, pos, args)
}

// ---- BYOK + exec (shared) ---------------------------------------------------

// byok holds the per-request BYOK decision: the validated provider name, the
// key, and the (validated, possibly empty) model override. A zero byok means
// keyless — the CLI's heuristic output is returned as-is.
type byok struct {
	provider string
	key      string
	model    string
}

// extractBYOK reads the X-LLM-Key header, resolves the provider (from the
// bodyProvider argument, falling back to the X-LLM-Provider header), and
// validates the optional model override. It returns the byok decision and true
// on success; on a client error it writes the response and returns false so
// the caller stops. When no key is supplied it succeeds with a keyless
// decision. The key is used ONLY by the in-process LLM call (providers.go) —
// it never reaches the child CLI in any form.
func extractBYOK(w http.ResponseWriter, r *http.Request, bodyProvider, bodyModel string) (byok, bool) {
	// The model override is validated even on keyless requests so a malformed
	// value fails the same way regardless of key presence. Its value is never
	// echoed back or logged.
	model, errMsg := validateModel(bodyModel)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return byok{}, false
	}
	// Key from header only (never from body — a key in a JSON body is easier to
	// accidentally log).
	key := strings.TrimSpace(r.Header.Get("X-LLM-Key"))
	if key == "" {
		return byok{}, true
	}
	provider := strings.ToLower(firstNonEmpty(bodyProvider, r.Header.Get("X-LLM-Provider")))
	if provider == "" {
		writeError(w, http.StatusBadRequest, "X-LLM-Key supplied but no provider; set \"provider\" in body or X-LLM-Provider header")
		return byok{}, false
	}
	if _, ok := providers[provider]; !ok {
		writeError(w, http.StatusBadRequest, "unknown provider "+quoteToken(provider)+"; supported: "+supportedProviders)
		return byok{}, false
	}
	return byok{provider: provider, key: key, model: model}, true
}

// runCLI executes the CLI (always keyless), then — when a BYOK key was
// supplied and the command benefits from it — performs the in-process LLM
// synthesis over the CLI's JSON output plus the user's free-text inputs and
// merges it into the response. The CLI result is always returned verbatim; an
// LLM failure never fails the request — the response degrades to
// llm_source:"keyless" plus a redacted llm_error.
func runCLI(w http.ResponseWriter, r *http.Request, b byok, group, cmd string, inputs, args []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// #nosec G204 -- args are a fixed subcommand plus user text as discrete argv
	// elements (no shell); the child env carries no keys at all.
	c := exec.CommandContext(ctx, cliBinaryPath(), args...)
	c.Env = buildChildEnv()

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	if err := c.Run(); err != nil {
		msg := redact(strings.TrimSpace(stderr.String()), b.key)
		if msg == "" {
			msg = redact(err.Error(), b.key)
		}
		writeError(w, http.StatusBadGateway, "CLI failed: "+msg)
		return
	}

	raw := bytes.TrimSpace(stdout.Bytes())
	var result json.RawMessage
	if json.Valid(raw) {
		result = raw
		// Web-side relevance gate (search only): partition off-topic trials
		// out of result.results BEFORE both display and the LLM prompt below.
		// Best-effort — any doubt passes the JSON through unchanged.
		if group == "search-discovery" && relevanceGatedCmds[cmd] && len(inputs) > 0 {
			result = relevanceGate(result, inputs[0])
		}
		// Rebuild phase_distribution so its counts always sum to the number of
		// trials returned (the CLI skips phaseless trials and double-counts
		// multi-phase ones). Pass-through when the shape doesn't apply.
		result = normalizePhaseDistribution(result)
	} else {
		// Some commands (version, help) print plain text; wrap it so the
		// response stays valid JSON instead of erroring.
		result, _ = json.Marshal(map[string]string{"output": string(raw)})
	}

	resp := map[string]any{
		"group":      group,
		"command":    cmd,
		"llm_source": "keyless",
		"result":     result,
	}
	if b.key != "" && !noLLMGroups[group] && !noLLMCmds[cmd] {
		syn, err := llmSynthesize(ctx, b.provider, b.key, b.model, cmd, inputs, result)
		if err != nil {
			// Already sanitized/redacted by providers.go; safe for client + log-free.
			resp["llm_error"] = err.Error()
		} else {
			resp["llm_synthesis"] = syn
			resp["llm_source"] = "llm:" + b.provider
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---- relevance gate (search results) -----------------------------------------
//
// The clinical-trials CLI matches by broad FTS full-text search with no
// condition filter, so a "diabetes" search can return Preterm Birth or
// Parkinson trials. This web-side gate partitions result.results[] into
// on-topic and off-topic BEFORE display and BEFORE the LLM prompt. It is
// deliberately conservative: any parse doubt keeps the trial (or passes the
// whole JSON through unchanged), and nothing is silently lost — dropped trials
// move to result.filtered_out with result.filtered_count alongside.

// relevanceGatedCmds names the commands whose output goes through the gate.
// Only search: digest's JSON is {term,total,recruiting,newest[],…} with no
// results[]/conditions[] to judge (verified in cli-src/internal/cli/digest.go).
var relevanceGatedCmds = map[string]bool{"search": true}

// gateStopwords are English filler words dropped from the query before
// matching. Most are under the 3-char length floor anyway; listed in full so
// the rule reads explicitly.
var gateStopwords = map[string]struct{}{
	"in": {}, "of": {}, "the": {}, "and": {}, "for": {}, "with": {},
	"to": {}, "on": {}, "a": {}, "an": {}, "or": {}, "vs": {},
}

// foldAccents maps accented Latin letters to their base letter so "sclérose"
// matches "sclerose". Dependency-free (no x/text): Latin-1 Supplement plus the
// Hungarian long vowels from Latin Extended-A. Unlisted runes pass through.
var foldAccents = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ő': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u', 'ű': 'u',
	'ç': 'c', 'ñ': 'n', 'ý': 'y', 'ÿ': 'y',
}

// normalizeGateText lowercases and folds accents; applied identically to the
// query tokens and to every trial haystack so both sides compare in the same
// alphabet.
func normalizeGateText(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		if b, ok := foldAccents[r]; ok {
			return b
		}
		return r
	}, s)
}

// contentTokens tokenizes the user's query for the relevance gate: lowercase,
// accents folded, split on non-alphanumeric runes, then tokens shorter than 3
// chars and stopwords dropped. An empty return DISABLES the gate — an empty or
// too-short query must never filter anything.
func contentTokens(query string) []string {
	fields := strings.FieldsFunc(normalizeGateText(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 3 {
			continue
		}
		if _, stop := gateStopwords[f]; stop {
			continue
		}
		out = append(out, f)
	}
	return out
}

// trialRelevant reports whether ANY content token appears (case-insensitive,
// accent-folded substring) in ANY of the trial's conditions[], title, or
// interventions[]. A trial whose shape can't be parsed is kept — the gate must
// never drop what it can't judge.
func trialRelevant(entry json.RawMessage, tokens []string) bool {
	var t struct {
		Title         string   `json:"title"`
		Conditions    []string `json:"conditions"`
		Interventions []string `json:"interventions"`
	}
	if err := json.Unmarshal(entry, &t); err != nil {
		return true
	}
	parts := make([]string, 0, 1+len(t.Conditions)+len(t.Interventions))
	parts = append(parts, t.Title)
	parts = append(parts, t.Conditions...)
	parts = append(parts, t.Interventions...)
	hay := normalizeGateText(strings.Join(parts, "\n"))
	for _, tok := range tokens {
		if strings.Contains(hay, tok) {
			return true
		}
	}
	return false
}

// relevanceGate applies the gate to one search response. Kept trials are
// passed through byte-identical (json.RawMessage, never re-marshaled
// individually); dropped trials move to filtered_out. On ANY doubt — no
// content tokens, unparseable JSON, missing/odd results shape, marshal
// failure — the input is returned unchanged. When nothing is dropped the
// input is returned byte-identical (no filtered_out/filtered_count added).
func relevanceGate(raw []byte, query string) []byte {
	tokens := contentTokens(query)
	if len(tokens) == 0 {
		return raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	rawList, ok := obj["results"]
	if !ok {
		return raw
	}
	var list []json.RawMessage
	if err := json.Unmarshal(rawList, &list); err != nil {
		return raw
	}
	kept := make([]json.RawMessage, 0, len(list))
	dropped := make([]json.RawMessage, 0)
	for _, entry := range list {
		if trialRelevant(entry, tokens) {
			kept = append(kept, entry)
		} else {
			dropped = append(dropped, entry)
		}
	}
	if len(dropped) == 0 {
		return raw
	}
	keptJSON, err := json.Marshal(kept)
	if err != nil {
		return raw
	}
	droppedJSON, err := json.Marshal(dropped)
	if err != nil {
		return raw
	}
	countJSON, err := json.Marshal(len(dropped))
	if err != nil {
		return raw
	}
	obj["results"] = keptJSON
	obj["filtered_out"] = droppedJSON
	obj["filtered_count"] = countJSON
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// ---- aggregate normalization (phase_distribution) ------------------------------
//
// The CLI's buildTrialListView (cli-src/internal/cli/recruiting.go) counts one
// distribution entry PER PHASE VALUE: a trial whose registry record has no
// phases contributes nothing, and a Phase 1/Phase 2 trial is counted twice.
// Observed live: recruiting "heart disease" returned 10 trials but the
// phase_distribution summed to 5 — the 5 phaseless trials silently vanished
// from the breakdown. The vendored CLI is generated code, so the fix lives
// here: whenever a result carries BOTH a phase_distribution and a trial list,
// the distribution is rebuilt one-label-per-trial ("Not specified" when the
// trial has no phases; multi-phase trials get a joined "Phase 1/Phase 2"
// label), which makes the counts sum to exactly the number of trials shown.

// webPhaseLabel mirrors the CLI's phaseLabel mapping (cli-src intel.go) so the
// rebuilt labels match what the CLI would print for a present phase.
func webPhaseLabel(phase string) string {
	switch phase {
	case "EARLY_PHASE1":
		return "Early Phase 1"
	case "PHASE1":
		return "Phase 1"
	case "PHASE2":
		return "Phase 2"
	case "PHASE3":
		return "Phase 3"
	case "PHASE4":
		return "Phase 4"
	case "NA", "":
		return "N/A"
	default:
		return phase
	}
}

// notSpecifiedLabel is the bucket for trials whose registry record carries no
// phase information at all (distinct from an explicit "NA" phase).
const notSpecifiedLabel = "Not specified"

// rankedEntry matches the CLI's {label,count} distribution entry shape.
type rankedEntry struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// trialListKeys are the result keys that may hold the trial list a
// phase_distribution was computed from.
var trialListKeys = []string{"trials", "results"}

// normalizePhaseDistribution rebuilds obj.phase_distribution from the trial
// list (top level and compare's drug_a/drug_b sub-objects). On ANY doubt —
// no phase_distribution key, no trial list, unparseable JSON — the input is
// returned unchanged, and when nothing changes the bytes pass through
// untouched.
func normalizePhaseDistribution(raw []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	changed := normalizePhaseObject(obj)
	for _, k := range []string{"drug_a", "drug_b"} {
		sub, ok := obj[k]
		if !ok {
			continue
		}
		var subObj map[string]json.RawMessage
		if err := json.Unmarshal(sub, &subObj); err != nil {
			continue
		}
		if normalizePhaseObject(subObj) {
			if enc, err := json.Marshal(subObj); err == nil {
				obj[k] = enc
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// normalizePhaseObject rebuilds phase_distribution inside one object in place.
// Returns true when the distribution was replaced.
func normalizePhaseObject(obj map[string]json.RawMessage) bool {
	if _, ok := obj["phase_distribution"]; !ok {
		return false
	}
	var list []struct {
		Phases []string `json:"phases"`
	}
	found := false
	for _, k := range trialListKeys {
		rawList, ok := obj[k]
		if !ok {
			continue
		}
		if err := json.Unmarshal(rawList, &list); err == nil {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	counts := map[string]int{}
	order := make([]string, 0, 8)
	for _, t := range list {
		label := notSpecifiedLabel
		if len(t.Phases) > 0 {
			labels := make([]string, 0, len(t.Phases))
			for _, p := range t.Phases {
				labels = append(labels, webPhaseLabel(p))
			}
			label = strings.Join(labels, "/")
		}
		if counts[label] == 0 {
			order = append(order, label)
		}
		counts[label]++
	}
	entries := make([]rankedEntry, 0, len(order))
	for _, l := range order {
		entries = append(entries, rankedEntry{Label: l, Count: counts[l]})
	}
	// stable insertion sort by count desc (ties keep first-seen order); the
	// list is tiny and main.go deliberately avoids importing sort.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j-1].Count < entries[j].Count; j-- {
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}
	enc, err := json.Marshal(entries)
	if err != nil {
		return false
	}
	obj["phase_distribution"] = enc
	return true
}

// buildChildEnv returns the environment for the child CLI process: the
// server's own environment with EVERY provider key removed. The child is
// always keyless — BYOK keys are used only for the in-process LLM call and
// must never reach a subprocess.
func buildChildEnv() []string {
	strip := make(map[string]struct{}, len(allProviderEnvVars))
	for _, v := range allProviderEnvVars {
		strip[v] = struct{}{}
	}
	base := os.Environ()
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if _, drop := strip[name]; drop {
			continue // never inherit the server's own provider keys
		}
		out = append(out, kv)
	}
	return out
}

// quoteToken quotes a short untrusted token for safe inclusion in an error
// message, stripping control bytes so it can't echo terminal escapes.
func quoteToken(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	return "\"" + strings.Map(func(r rune) rune {
		if r < 0x20 {
			return '?'
		}
		return r
	}, s) + "\""
}

func redact(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[REDACTED]")
}

func sortedKeys(m map[string]cmdSpec) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// simple insertion sort (small maps) to avoid importing sort for one use
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
