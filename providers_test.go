package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// resultsJSON builds a search/recruiting-shaped CLI output with n entries in
// the given array key (results, trials, or sources).
func resultsJSON(t *testing.T, key string, n int) []byte {
	t.Helper()
	entries := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, map[string]any{
			"nct_id":     fmt.Sprintf("NCT%08d", i),
			"title":      fmt.Sprintf("Trial %d — a phase 2 study of drug X in condition Y", i),
			"phase":      "PHASE2",
			"status":     "RECRUITING",
			"enrollment": 120 + i,
		})
	}
	out, err := json.Marshal(map[string]any{
		"query": "diabetes",
		"count": n,
		key:     entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// decode unmarshals compacted output for assertions.
func decode(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("compacted output is not valid JSON: %v", err)
	}
	return obj
}

func entryCount(t *testing.T, obj map[string]json.RawMessage, key string) int {
	t.Helper()
	var list []json.RawMessage
	if err := json.Unmarshal(obj[key], &list); err != nil {
		t.Fatalf("%s not an array: %v", key, err)
	}
	return len(list)
}

func TestCompactForLLMTrimsLargeArrays(t *testing.T) {
	for _, key := range []string{"results", "trials", "sources"} {
		got := decode(t, compactForLLM(resultsJSON(t, key, 30)))
		if n := entryCount(t, got, key); n != maxEntriesForLLM {
			t.Errorf("%s: got %d entries, want %d", key, n, maxEntriesForLLM)
		}
		// The trim keeps the FIRST (most relevant) entries.
		var list []struct {
			NctID string `json:"nct_id"`
		}
		if err := json.Unmarshal(got[key], &list); err != nil {
			t.Fatal(err)
		}
		if list[0].NctID != "NCT00000000" || list[len(list)-1].NctID != fmt.Sprintf("NCT%08d", maxEntriesForLLM-1) {
			t.Errorf("%s: trim did not keep the first %d entries: first=%q last=%q", key, maxEntriesForLLM, list[0].NctID, list[len(list)-1].NctID)
		}
		// Unrelated fields survive.
		if _, ok := got["count"]; !ok {
			t.Errorf("%s: count dropped during compaction", key)
		}
	}
}

func TestCompactForLLMShortListUnchanged(t *testing.T) {
	raw := resultsJSON(t, "results", 10)
	if got := compactForLLM(raw); !bytes.Equal(got, raw) {
		t.Error("output with 10 results should pass through byte-identical (nothing to trim)")
	}
}

func TestCompactForLLMCompareNested(t *testing.T) {
	side := func(name string) json.RawMessage {
		out, err := json.Marshal(map[string]any{
			"name":   name,
			"trials": json.RawMessage(mustField(t, resultsJSON(t, "trials", 30), "trials")),
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	cmp, err := json.Marshal(map[string]any{
		"drug_a": side("aspirin"),
		"drug_b": side("ibuprofen"),
		"winner": "drug_a",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, compactForLLM(cmp))
	for _, k := range []string{"drug_a", "drug_b"} {
		sub := decode(t, got[k])
		if n := entryCount(t, sub, "trials"); n != maxEntriesForLLM {
			t.Errorf("%s.trials: got %d entries, want %d", k, n, maxEntriesForLLM)
		}
	}
	var winner string
	if err := json.Unmarshal(got["winner"], &winner); err != nil || winner != "drug_a" {
		t.Errorf("winner corrupted: %s err=%v", got["winner"], err)
	}
}

// mustField extracts one raw field from a JSON object.
func mustField(t *testing.T, raw []byte, key string) json.RawMessage {
	t.Helper()
	obj := decode(t, raw)
	f, ok := obj[key]
	if !ok {
		t.Fatalf("field %s missing", key)
	}
	return f
}

// TestCompactionFitsByteCap encodes the point of the trim: a huge results
// array (and a compare with two huge trials arrays) must fit under
// maxCLIJSONForPrompt after compaction, so the byte cap never cuts the JSON
// mid-array.
func TestCompactionFitsByteCap(t *testing.T) {
	if got := compactForLLM(resultsJSON(t, "results", 500)); len(got) >= maxCLIJSONForPrompt {
		t.Errorf("compacted results JSON is %d bytes, must stay under %d", len(got), maxCLIJSONForPrompt)
	}
	cmp, err := json.Marshal(map[string]any{
		"drug_a": json.RawMessage(resultsJSON(t, "trials", 500)),
		"drug_b": json.RawMessage(resultsJSON(t, "trials", 500)),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := compactForLLM(cmp)
	if len(got) >= maxCLIJSONForPrompt {
		t.Errorf("compacted compare JSON is %d bytes, must stay under %d", len(got), maxCLIJSONForPrompt)
	}
	t.Logf("compacted compare JSON: %d bytes (cap %d)", len(got), maxCLIJSONForPrompt)
}

func TestCompactForLLMNoLargeArraysUnchanged(t *testing.T) {
	// risk/forecast-shaped output: factors stay whole, nothing to trim.
	raw := []byte(`{"nct_id":"NCT01234567","score":0.42,"level":"medium","factors":[{"name":"single site"},{"name":"small N"}]}`)
	if got := compactForLLM(raw); !bytes.Equal(got, raw) {
		t.Errorf("JSON without large arrays must be returned byte-identical\ngot:  %s\nwant: %s", got, raw)
	}
}

func TestCompactForLLMInvalidInputsUnchanged(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`not json at all`),
		[]byte(`[1,2,3]`),                // top level not an object
		[]byte(`{"results":"oops"}`),     // results not an array
		[]byte(`{"drug_a":"not-object"}`), // compare key not an object
	} {
		if got := compactForLLM(raw); !bytes.Equal(got, raw) {
			t.Errorf("input %q must pass through unchanged, got %q", raw, got)
		}
	}
}

func TestSynthesisPromptUsesCompactedJSON(t *testing.T) {
	prompt := synthesisPrompt("search", []string{"diabetes"}, resultsJSON(t, "results", 30))
	if !strings.Contains(prompt, "INPUT 1: diabetes") {
		t.Error("prompt missing the user input")
	}
	if !strings.Contains(prompt, fmt.Sprintf("NCT%08d", maxEntriesForLLM-1)) {
		t.Errorf("prompt missing entry %d (last kept entry)", maxEntriesForLLM-1)
	}
	if strings.Contains(prompt, fmt.Sprintf("NCT%08d", maxEntriesForLLM)) {
		t.Errorf("prompt contains entry %d, which should be trimmed", maxEntriesForLLM)
	}
	if !strings.Contains(prompt, "NOT medical or clinical advice") {
		t.Error("prompt missing the not-medical-advice instruction")
	}
	if !strings.Contains(prompt, `"not_advice"`) {
		t.Error("prompt missing the output schema")
	}
}

func TestParseSynthesis(t *testing.T) {
	text := "Here you go:\n```json\n" +
		`{"summary":"There are 30 recruiting phase 2 trials.","key_points":["30 trials found","largest enrollment 149"],"caveats":["registry-only signal"],"not_advice":""}` +
		"\n```"
	syn, err := parseSynthesis(text)
	if err != nil {
		t.Fatal(err)
	}
	if syn.Summary == "" || len(syn.KeyPoints) != 2 || len(syn.Caveats) != 1 {
		t.Errorf("unexpected parse: %+v", syn)
	}
	if syn.NotAdvice != defaultNotAdvice {
		t.Errorf("empty not_advice must be replaced with the default, got %q", syn.NotAdvice)
	}
	if _, err := parseSynthesis("no json here"); err == nil {
		t.Error("want error for output without JSON")
	}
	if _, err := parseSynthesis(`{"summary":""}`); err == nil {
		t.Error("want error for empty summary")
	}
	// Runaway lists are capped.
	long := make([]string, 20)
	for i := range long {
		long[i] = fmt.Sprintf("point %d", i)
	}
	enc, _ := json.Marshal(map[string]any{"summary": "s", "key_points": long, "caveats": long})
	syn, err = parseSynthesis(string(enc))
	if err != nil {
		t.Fatal(err)
	}
	if len(syn.KeyPoints) > 5 || len(syn.Caveats) > 8 {
		t.Errorf("lists not capped: key_points=%d caveats=%d", len(syn.KeyPoints), len(syn.Caveats))
	}
}

// ---- synthesis post-validation (anti-hallucination) --------------------------

// groundingSource mimics the live recruiting output that produced the "Við
// Norway" hallucination: Israel is present, Norway is not; enrollment 10 and
// the 10-of-4004 counts are real numbers.
var groundingSource = []byte(`{"query":"heart disease","total_matching":4004,"returned":10,` +
	`"trials":[{"id":"NCT06115980","title":"Xtrac O.S. Study","countries":["Israel"],"enrollment":10},` +
	`{"id":"NCT07476703","title":"Heart Study","countries":["United States","China","Egypt"],"enrollment":120}]}`)

func groundedSyn(summary string, keyPoints, caveats []string) *llmSynthesis {
	return &llmSynthesis{Summary: summary, KeyPoints: keyPoints, Caveats: caveats, NotAdvice: defaultNotAdvice}
}

func TestGroundSynthesisDropsHallucinatedCountry(t *testing.T) {
	// The live failure verbatim: a caveat citing a nonexistent Norwegian trial.
	syn := groundedSyn("Ten trials are recruiting.",
		[]string{"NCT06115980 enrolls 10 participants."},
		[]string{"Many studies have small sample sizes (e.g., Við Norway enrolling 10 participants)."})
	groundSynthesis(syn, groundingSource)
	if len(syn.Caveats) != 0 {
		t.Errorf("hallucinated-country caveat must be dropped, got %v", syn.Caveats)
	}
	if len(syn.KeyPoints) != 1 {
		t.Errorf("grounded key point must be kept, got %v", syn.KeyPoints)
	}
	if syn.GroundingNote == "" || !strings.Contains(syn.GroundingNote, "Norway") {
		t.Errorf("grounding note must name the unverified token, got %q", syn.GroundingNote)
	}
}

func TestGroundSynthesisDropsUnknownNCTAndNumber(t *testing.T) {
	syn := groundedSyn("Ten trials are recruiting.",
		[]string{"NCT99999999 is the largest trial.", "One trial enrolls 98765 patients."},
		nil)
	groundSynthesis(syn, groundingSource)
	if len(syn.KeyPoints) != 0 {
		t.Errorf("unknown NCT / number key points must be dropped, got %v", syn.KeyPoints)
	}
	for _, tok := range []string{"NCT99999999", "98765"} {
		if !strings.Contains(syn.GroundingNote, tok) {
			t.Errorf("grounding note missing token %q: %q", tok, syn.GroundingNote)
		}
	}
}

func TestGroundSynthesisKeepsGroundedClaims(t *testing.T) {
	syn := groundedSyn("Showing 10 of 4,004 matching trials; sites include Israel and the United States.",
		[]string{"NCT07476703 (China, Egypt) enrolls 120 participants.", "About 50% lack phase data."},
		[]string{"Registry-only signal."})
	groundSynthesis(syn, groundingSource)
	if len(syn.KeyPoints) != 2 || len(syn.Caveats) != 1 {
		t.Errorf("grounded content must survive: key_points=%v caveats=%v", syn.KeyPoints, syn.Caveats)
	}
	if syn.GroundingNote != "" {
		t.Errorf("fully grounded synthesis must carry no note, got %q", syn.GroundingNote)
	}
}

func TestGroundSynthesisFlagsSummaryWithoutDroppingIt(t *testing.T) {
	syn := groundedSyn("The U.S., China, and Norway lead the activity.", nil, nil)
	groundSynthesis(syn, groundingSource)
	if syn.Summary == "" {
		t.Error("summary must be kept (flagged, not dropped)")
	}
	if !strings.Contains(syn.GroundingNote, "summary") || !strings.Contains(syn.GroundingNote, "Norway") {
		t.Errorf("summary with ungrounded reference must be flagged, note = %q", syn.GroundingNote)
	}
}

// ---- LLM trial-field whitelist -----------------------------------------------

func TestCompactStripsNoiseFieldsFromTrials(t *testing.T) {
	raw := []byte(`{"returned":1,"trials":[{"id":"NCT01234567","title":"T","status":"RECRUITING",` +
		`"sponsor_class":"INDUSTRY","last_update":"2026-01-01","source":"clinicaltrials.gov","enrollment":50}]}`)
	got := decode(t, compactForLLM(raw))
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(got["trials"], &list); err != nil {
		t.Fatal(err)
	}
	for _, noisy := range []string{"sponsor_class", "last_update", "source"} {
		if _, ok := list[0][noisy]; ok {
			t.Errorf("noise field %q must be stripped from the LLM copy", noisy)
		}
	}
	for _, keep := range []string{"id", "title", "status", "enrollment"} {
		if _, ok := list[0][keep]; !ok {
			t.Errorf("signal field %q must survive compaction", keep)
		}
	}
	// The client copy (input bytes) is untouched by design — compactForLLM
	// returns a new slice; the caller still holds the original.
	if !strings.Contains(string(raw), "sponsor_class") {
		t.Error("input mutated")
	}
}

func TestSanitizeLLMErrorRedactsKey(t *testing.T) {
	key := "sk-super-secret-key-123"
	out := sanitizeLLMError("provider said: invalid key "+key+"\nline2\x07", key)
	if strings.Contains(out, key) {
		t.Error("key leaked through sanitizeLLMError")
	}
	if strings.ContainsAny(out, "\n\x07") {
		t.Error("control bytes survived sanitizeLLMError")
	}
}

func TestValidateModel(t *testing.T) {
	if m, e := validateModel("  deepseek/deepseek-chat  "); m != "deepseek/deepseek-chat" || e != "" {
		t.Errorf("trim failed: %q %q", m, e)
	}
	if _, e := validateModel("bad model"); e == "" {
		t.Error("whitespace inside model must be rejected")
	}
	if _, e := validateModel(strings.Repeat("a", 129)); e == "" {
		t.Error("over-long model must be rejected")
	}
	if m, e := validateModel(""); m != "" || e != "" {
		t.Error("empty model must be accepted as no-override")
	}
}

func TestProviderRegistry(t *testing.T) {
	want := []string{"anthropic", "openai", "gemini", "groq", "mistral", "deepseek", "zai", "moonshot", "qwen", "minimax", "xai", "openrouter"}
	if len(providers) != len(want) {
		t.Errorf("registry has %d providers, want %d", len(providers), len(want))
	}
	for _, name := range want {
		spec, ok := providers[name]
		if !ok {
			t.Errorf("provider %s missing from registry", name)
			continue
		}
		if !strings.HasPrefix(spec.BaseURL, "https://") {
			t.Errorf("provider %s BaseURL is not HTTPS: %s", name, spec.BaseURL)
		}
		if spec.DefaultModel == "" {
			t.Errorf("provider %s has no default model", name)
		}
	}
	if providers["anthropic"].Style != styleAnthropic {
		t.Error("anthropic must use the Anthropic wire format")
	}
}
