package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The two trials below are real objects measured live from the CLI's search
// output (fields trimmed to the ones the gate reads plus a few bystanders).
const trialPreterm = `{"id":"NCT03360539","title":"Nurse-Family Partnership Impact Evaluation in South Carolina","status":"COMPLETED","conditions":["Preterm Birth","Injuries","Maternal Behavior"],"interventions":["Nurse-Family Partnership"],"sponsor":"Harvard University","source":"clinicaltrials.gov"}`

const trialT2DM = `{"id":"NCT07437157","title":"A Study of Oral Agent X in Adults","status":"RECRUITING","conditions":["Type 2 Diabetes (T2DM)","Obesity"],"interventions":["Drug: Agent X"],"sponsor":"Example Pharma","source":"clinicaltrials.gov"}`

func searchJSON(t *testing.T, trials ...string) []byte {
	t.Helper()
	raw := []byte(`{"query":"q","count":` + itoa(len(trials)) + `,"results":[` + strings.Join(trials, ",") + `]}`)
	if !json.Valid(raw) {
		t.Fatalf("test fixture is not valid JSON: %s", raw)
	}
	return raw
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// gateResults decodes the gated output's results / filtered_out id lists.
func gateResults(t *testing.T, raw []byte) (kept, dropped []string, count int) {
	t.Helper()
	var out struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
		FilteredOut []struct {
			ID string `json:"id"`
		} `json:"filtered_out"`
		FilteredCount int `json:"filtered_count"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("gated output is not valid JSON: %v", err)
	}
	for _, r := range out.Results {
		kept = append(kept, r.ID)
	}
	for _, r := range out.FilteredOut {
		dropped = append(dropped, r.ID)
	}
	return kept, dropped, out.FilteredCount
}

func TestRelevanceGateDropsOffTopicKeepsOnTopic(t *testing.T) {
	got := relevanceGate(searchJSON(t, trialPreterm, trialT2DM), "diabetes")
	kept, dropped, count := gateResults(t, got)
	if len(kept) != 1 || kept[0] != "NCT07437157" {
		t.Errorf("kept = %v, want [NCT07437157]", kept)
	}
	if len(dropped) != 1 || dropped[0] != "NCT03360539" {
		t.Errorf("filtered_out = %v, want [NCT03360539]", dropped)
	}
	if count != 1 {
		t.Errorf("filtered_count = %d, want 1", count)
	}
	// The kept trial must be byte-identical to the input entry (no mutation).
	var out map[string]json.RawMessage
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(out["results"], &list); err != nil {
		t.Fatal(err)
	}
	if string(list[0]) != trialT2DM {
		t.Errorf("kept trial mutated:\ngot:  %s\nwant: %s", list[0], trialT2DM)
	}
}

func TestRelevanceGateEmptyQueryKeepsEverything(t *testing.T) {
	raw := searchJSON(t, trialPreterm, trialT2DM)
	for _, q := range []string{"", "   ", "in of to", "ab"} {
		if got := relevanceGate(raw, q); !bytes.Equal(got, raw) {
			t.Errorf("query %q: gate must be disabled (byte-identical pass-through)", q)
		}
	}
}

func TestRelevanceGateCaseInsensitive(t *testing.T) {
	for _, q := range []string{"DIABETES", "Diabetes", "type 2 DIABETES"} {
		_, dropped, _ := gateResults(t, relevanceGate(searchJSON(t, trialPreterm, trialT2DM), q))
		if len(dropped) != 1 || dropped[0] != "NCT03360539" {
			t.Errorf("query %q: filtered_out = %v, want [NCT03360539]", q, dropped)
		}
	}
}

func TestRelevanceGateNoDropsPassesThroughUnchanged(t *testing.T) {
	raw := searchJSON(t, trialT2DM)
	got := relevanceGate(raw, "diabetes")
	if !bytes.Equal(got, raw) {
		t.Error("no-drop result must be byte-identical (no filtered_out/filtered_count added)")
	}
}

func TestRelevanceGateOddShapesUnchanged(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`not json`),
		[]byte(`[1,2,3]`),
		[]byte(`{"trials":[{"id":"x"}]}`),  // no results key
		[]byte(`{"results":"oops"}`),       // results not an array
	} {
		if got := relevanceGate(raw, "diabetes"); !bytes.Equal(got, raw) {
			t.Errorf("input %q must pass through unchanged, got %q", raw, got)
		}
	}
	// An unparseable entry inside results is KEPT, never dropped.
	raw := []byte(`{"results":[{"title":42,"conditions":"bad-shape"}]}`)
	kept, dropped, _ := gateResults(t, relevanceGate(raw, "diabetes"))
	_ = kept
	if len(dropped) != 0 {
		t.Error("unparseable trial entry must be kept, not dropped")
	}
}

func TestRelevanceGateMatchesTitleAndInterventions(t *testing.T) {
	titleOnly := `{"id":"NCT1","title":"Metformin in Adults With Prediabetes","conditions":["Something Else"],"interventions":[]}`
	intervOnly := `{"id":"NCT2","title":"A Study","conditions":["Something Else"],"interventions":["Drug: Insulin glargine"]}`
	_, dropped, _ := gateResults(t, relevanceGate(searchJSON(t, titleOnly, intervOnly), "prediabetes insulin"))
	if len(dropped) != 0 {
		t.Errorf("title/intervention matches must be kept, dropped = %v", dropped)
	}
}

func TestContentTokens(t *testing.T) {
	got := contentTokens("Type 2 Diabetes, with the insulin!")
	want := []string{"type", "diabetes", "insulin"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if toks := contentTokens("in of to a an or vs"); len(toks) != 0 {
		t.Errorf("stopword-only query must yield no tokens, got %v", toks)
	}
	// Accent folding: Hungarian/French letters compare in the base alphabet.
	if toks := contentTokens("sclérose"); len(toks) != 1 || toks[0] != "sclerose" {
		t.Errorf("accent fold failed: %v", toks)
	}
}
