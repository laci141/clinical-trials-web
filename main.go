// Command clinical-trials-web is a thin standalone HTTP wrapper around the
// clinical-trials CLI. It exposes ONE endpoint per command group (6 total) and
// routes to the ~30 CLI subcommands, assembling each command's real positional
// arguments (verified from `--help`, NOT the guessed flag names). A per-request
// BYOK key is injected only into the child process environment.
//
// SECURITY: the X-LLM-Key header lives only in memory for one request/child;
// buildChildEnv strips every known provider key from the inherited environment
// before adding the single per-request key; nothing is logged or persisted.
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

// providerEnvVar maps a BYOK provider to the env var the CLI would read. Setting
// exactly one (and stripping the rest) keeps the caller's choice deterministic.
var providerEnvVar = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"openai":    "OPENAI_API_KEY",
	"gemini":    "GEMINI_API_KEY",
	"groq":      "GROQ_API_KEY",
	"mistral":   "MISTRAL_API_KEY",
}

// request carries every field any group body might send. Each command's spec
// picks the ones it needs as ordered positional arguments.
type request struct {
	Command   string `json:"command"`
	Provider  string `json:"provider"`
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

func handleGroup(w http.ResponseWriter, r *http.Request, group string) {
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

	b, ok := extractBYOK(w, r, req.Provider)
	if !ok {
		return
	}
	runCLI(w, r, b, group, cmd, args)
}

// ---- BYOK + exec (shared) ---------------------------------------------------

type byok struct {
	envKeyVar string
	key       string
	source    string
}

func extractBYOK(w http.ResponseWriter, r *http.Request, bodyProvider string) (byok, bool) {
	key := strings.TrimSpace(r.Header.Get("X-LLM-Key"))
	if key == "" {
		return byok{source: "keyless"}, true
	}
	provider := strings.ToLower(firstNonEmpty(bodyProvider, r.Header.Get("X-LLM-Provider")))
	if provider == "" {
		writeError(w, http.StatusBadRequest, "X-LLM-Key supplied but no provider; set \"provider\" in body or X-LLM-Provider header")
		return byok{}, false
	}
	v, ok := providerEnvVar[provider]
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown provider; supported: anthropic, openai, gemini, groq, mistral")
		return byok{}, false
	}
	return byok{envKeyVar: v, key: key, source: "llm:" + provider}, true
}

func runCLI(w http.ResponseWriter, r *http.Request, b byok, group, cmd string, args []string) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	// #nosec G204 -- args are a fixed subcommand plus user text as discrete argv
	// elements (no shell); the key goes only into the child's environment.
	c := exec.CommandContext(ctx, cliBinaryPath(), args...)
	c.Env = buildChildEnv(b.envKeyVar, b.key)

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
	} else {
		// Some commands (version, help) print plain text; wrap it so the
		// response stays valid JSON instead of erroring.
		result, _ = json.Marshal(map[string]string{"output": string(raw)})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"group":   group,
		"command": cmd,
		"source":  b.source,
		"result":  result,
	})
}

func buildChildEnv(keyVar, keyVal string) []string {
	strip := map[string]struct{}{}
	for _, v := range providerEnvVar {
		strip[v] = struct{}{}
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if _, drop := strip[name]; drop {
			continue
		}
		out = append(out, kv)
	}
	if keyVar != "" && keyVal != "" {
		out = append(out, keyVar+"="+keyVal)
	}
	return out
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
