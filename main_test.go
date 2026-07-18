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

// ---- phase_distribution normalization ----------------------------------------

// trialWithPhases builds a minimal trial row with the given phases (nil = the
// phases field is absent entirely, as in the live records that triggered the
// bug).
func trialWithPhases(id string, phases []string) string {
	if phases == nil {
		return `{"id":"` + id + `","title":"t","status":"RECRUITING"}`
	}
	enc, _ := json.Marshal(phases)
	return `{"id":"` + id + `","title":"t","status":"RECRUITING","phases":` + string(enc) + `}`
}

func phaseDist(t *testing.T, raw []byte) (entries []rankedEntry, sum int) {
	t.Helper()
	var out struct {
		PhaseDistribution []rankedEntry `json:"phase_distribution"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("normalized output is not valid JSON: %v", err)
	}
	for _, e := range out.PhaseDistribution {
		sum += e.Count
	}
	return out.PhaseDistribution, sum
}

// TestPhaseDistributionSumEqualsReturned reproduces the live bug: recruiting
// "heart disease" returned 10 trials but the CLI distribution summed to 5
// because 5 trials had no phases at all. After normalization the counts must
// sum to exactly the returned trial count, with the phaseless trials in an
// explicit "Not specified" bucket.
func TestPhaseDistributionSumEqualsReturned(t *testing.T) {
	trials := []string{
		trialWithPhases("NCT07476703", nil),
		trialWithPhases("NCT05719545", nil),
		trialWithPhases("NCT06434012", nil),
		trialWithPhases("NCT07154186", nil),
		trialWithPhases("NCT05180942", nil),
		trialWithPhases("NCT00000001", []string{"NA"}),
		trialWithPhases("NCT00000002", []string{"NA"}),
		trialWithPhases("NCT00000003", []string{"NA"}),
		trialWithPhases("NCT00000004", []string{"NA"}),
		trialWithPhases("NCT00000005", []string{"PHASE4"}),
	}
	raw := []byte(`{"query":"heart disease","total_matching":4004,"returned":10,` +
		`"phase_distribution":[{"label":"N/A","count":4},{"label":"Phase 4","count":1}],` +
		`"trials":[` + strings.Join(trials, ",") + `]}`)
	if !json.Valid(raw) {
		t.Fatal("fixture invalid")
	}
	entries, sum := phaseDist(t, normalizePhaseDistribution(raw))
	if sum != 10 {
		t.Errorf("phase_distribution sums to %d, want 10 (== returned)", sum)
	}
	want := map[string]int{"Not specified": 5, "N/A": 4, "Phase 4": 1}
	for _, e := range entries {
		if want[e.Label] != e.Count {
			t.Errorf("label %q count = %d, want %d", e.Label, e.Count, want[e.Label])
		}
		delete(want, e.Label)
	}
	for l := range want {
		t.Errorf("label %q missing from normalized distribution", l)
	}
}

func TestPhaseDistributionMultiPhaseCountsOnce(t *testing.T) {
	raw := []byte(`{"returned":2,"phase_distribution":[{"label":"Phase 1","count":1},{"label":"Phase 2","count":1},{"label":"Phase 3","count":1}],` +
		`"trials":[` + trialWithPhases("NCT00000001", []string{"PHASE1", "PHASE2"}) + `,` +
		trialWithPhases("NCT00000002", []string{"PHASE3"}) + `]}`)
	entries, sum := phaseDist(t, normalizePhaseDistribution(raw))
	if sum != 2 {
		t.Errorf("sum = %d, want 2 (multi-phase trial must count once)", sum)
	}
	found := false
	for _, e := range entries {
		if e.Label == "Phase 1/Phase 2" && e.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("want joined label \"Phase 1/Phase 2\", got %+v", entries)
	}
}

func TestPhaseDistributionNestedCompare(t *testing.T) {
	side := `{"name":"aspirin","phase_distribution":[{"label":"N/A","count":1}],` +
		`"trials":[` + trialWithPhases("NCT00000001", nil) + `,` + trialWithPhases("NCT00000002", []string{"PHASE2"}) + `]}`
	raw := []byte(`{"drug_a":` + side + `,"drug_b":` + side + `}`)
	var out struct {
		DrugA json.RawMessage `json:"drug_a"`
	}
	if err := json.Unmarshal(normalizePhaseDistribution(raw), &out); err != nil {
		t.Fatal(err)
	}
	_, sum := phaseDist(t, out.DrugA)
	if sum != 2 {
		t.Errorf("nested drug_a distribution sums to %d, want 2", sum)
	}
}

func TestPhaseDistributionPassthrough(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`not json`),
		[]byte(`[1,2]`),
		[]byte(`{"returned":3,"trials":[{"id":"x"}]}`),                     // no phase_distribution
		[]byte(`{"phase_distribution":[{"label":"N/A","count":1}]}`),      // no trial list
		[]byte(`{"phase_distribution":"oops","trials":"also-oops"}`),      // wrong shapes
		[]byte(`{"score":0.4,"level":"medium","factors":[{"name":"x"}]}`), // risk shape
	} {
		if got := normalizePhaseDistribution(raw); !bytes.Equal(got, raw) {
			t.Errorf("input %q must pass through byte-identical, got %q", raw, got)
		}
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
