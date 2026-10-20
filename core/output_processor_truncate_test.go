package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// buildPaymentRequestRecords mimics the shape that exposed the defect: a flat
// array of records where every individual field is short, but the array as a
// whole is long. AppWorld's venmo endpoint returns 20 of these (~1.5K chars each,
// ~29K total) when asked for page_limit=20, against an 8000-char budget.
//
// No field here exceeds the per-field budget (maxChars/10), which is exactly why
// a field-shrinking strategy could not reduce it by a single byte.
func buildPaymentRequestRecords(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"payment_request_id": 1600 + i,
			"amount":             float64(i) + 12.34,
			"description":        fmt.Sprintf("Dinner split %d", i),
			"created_at":         "2026-07-01T19:42:11",
			"updated_at":         "2026-07-01T19:42:11",
			"approved_at":        nil,
			"denied_at":          nil,
			"private":            false,
			"status":             "pending",
			"sender":             map[string]any{"name": "Ada Lovelace", "email": "ada@example.com"},
			"receiver":           map[string]any{"name": "Grace Hopper", "email": "grace@example.com"},
			"note":               strings.Repeat("x", 120),
		})
	}
	return out
}

// TestTruncateJSONFields_LongArrayBecomesParseableYAML is the regression for the
// defect: a long array of short-field records used to fall through to the
// plain-text head+tail path, which cut the JSON mid-token and left the model with
// something it could not parse.
func TestTruncateJSONFields_LongArrayBecomesParseableYAML(t *testing.T) {
	const budget = 8000
	records := buildPaymentRequestRecords(20)

	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	if len(raw) <= budget {
		t.Fatalf("fixture must exceed the budget to exercise truncation: %d <= %d", len(raw), budget)
	}

	got, ok := truncateJSONFields(string(raw), budget)
	if !ok {
		t.Fatal("a syntactically valid JSON document must never fall through to the head+tail path")
	}
	if len(got) > budget {
		t.Fatalf("output %d chars exceeds budget %d", len(got), budget)
	}
	if strings.Contains(got, "... [TRUNCATED") {
		t.Fatal("output must not use the plain-text truncation marker")
	}
	if !strings.HasPrefix(got, "# This tool returned JSON; it is shown as YAML") {
		t.Fatalf("output must announce the re-encoding; got head: %.160s", got)
	}

	var parsed []map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output must parse as YAML: %v\nhead: %.200s", err, got)
	}
	if len(parsed) == 0 {
		t.Fatal("expected at least one record")
	}
	// Every element must be a real record. An earlier design appended a sentinel
	// object into the array, which a caller iterating records would have had to
	// special-case; the header now carries that information instead.
	for i, r := range parsed {
		if _, ok := r["payment_request_id"]; !ok {
			t.Fatalf("element %d is not a real record: %v", i, r)
		}
	}
}

// TestTruncateJSONFields_ReencodingIsLossless checks that the YAML path carries
// every field through unchanged.
//
// Compression ratio is deliberately NOT asserted. Measured on AppWorld-shaped
// records (12 short fields each) YAML saves only a few percent, because one
// `key: value` line plus indentation costs about as much as one `"key":` entry
// plus its comma. The win is larger for string-heavy payloads, and larger still
// for payloads that would otherwise be cut -- but it is not a fixed percentage,
// and a test that assumed one would be asserting a number nobody measured.
func TestTruncateJSONFields_ReencodingIsLossless(t *testing.T) {
	records := buildPaymentRequestRecords(10)
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	// Budget comfortably above both encodings so nothing is dropped.
	got, ok := truncateJSONFields(string(raw), len(raw)*2)
	if !ok {
		t.Fatal("expected YAML re-encoding to succeed")
	}
	if strings.Contains(got, "TRUNCATED") {
		t.Fatalf("nothing should be dropped at this budget:\n%.300s", got)
	}

	var parsed []map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output must parse as YAML: %v", err)
	}
	if len(parsed) != 10 {
		t.Fatalf("expected all 10 records, got %d", len(parsed))
	}
	for i, r := range parsed {
		if !scalarEq(r["payment_request_id"], fmt.Sprint(1600+i)) {
			t.Fatalf("record %d: id mismatch, got %v", i, r["payment_request_id"])
		}
		if r["description"] != fmt.Sprintf("Dinner split %d", i) {
			t.Fatalf("record %d: description mismatch, got %v", i, r["description"])
		}
		if r["status"] != "pending" {
			t.Fatalf("record %d: status mismatch, got %v", i, r["status"])
		}
	}
}

// TestTruncateJSONFields_DroppedRecordsAreAnnounced covers the case YAML alone
// cannot save: the header must say how many records were shown, so the model asks
// for the next page instead of concluding the data set is that small.
func TestTruncateJSONFields_DroppedRecordsAreAnnounced(t *testing.T) {
	const budget = 4000
	records := buildPaymentRequestRecords(20)
	raw, _ := json.Marshal(records)

	got, ok := truncateJSONFields(string(raw), budget)
	if !ok {
		t.Fatal("expected success")
	}
	if len(got) > budget {
		t.Fatalf("output %d chars exceeds budget %d", len(got), budget)
	}
	if !strings.Contains(got, "TRUNCATED: showing the first") || !strings.Contains(got, "of 20 records") {
		t.Fatalf("dropped records must be announced in the header:\n%.400s", got)
	}

	var parsed []map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output must parse as YAML: %v", err)
	}
	if len(parsed) >= 20 {
		t.Fatalf("expected fewer than 20 records at a %d budget, got %d", budget, len(parsed))
	}
	if len(parsed) == 0 {
		t.Fatal("expected at least one record")
	}
}

// TestTruncateJSONFields_LongSingleFieldStillShrinksField keeps the behaviour
// for the shape field-shrinking was built for: one or two oversized values.
func TestTruncateJSONFields_LongSingleFieldStillShrinksField(t *testing.T) {
	doc := map[string]any{
		"path":    "notes.txt",
		"content": strings.Repeat("A", 50000),
	}
	raw, _ := json.Marshal(doc)

	got, ok := truncateJSONFields(string(raw), 4000)
	if !ok {
		t.Fatal("expected a successful truncation")
	}
	if len(got) > 4000 {
		t.Fatalf("output %d chars exceeds budget 4000", len(got))
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output must parse as YAML: %v", err)
	}
	if _, ok := parsed["path"]; !ok {
		t.Fatal("small fields must survive truncation")
	}
	content, _ := parsed["content"].(string)
	if len(content) >= 50000 {
		t.Fatal("oversized field should have been shrunk")
	}
	if len(parsed) != 2 {
		t.Fatalf("field-level truncation must not drop keys, got %d keys", len(parsed))
	}
}

// TestTruncateJSONFields_NonJSONDefersToCaller confirms the boolean still means
// what the caller thinks it means: false == "not JSON, use the text fallback".
func TestTruncateJSONFields_NonJSONDefersToCaller(t *testing.T) {
	cases := map[string]string{
		"cli stdout":  strings.Repeat("go: downloading module\n", 500),
		"stack trace": strings.Repeat("panic: runtime error\n", 500),
		"malformed":   `{"a": 1, "b": `,
		"empty":       "",
	}
	for name, in := range cases {
		if out, ok := truncateJSONFields(in, 100); ok {
			t.Errorf("%s: expected ok=false so the caller uses head+tail, got %d chars", name, len(out))
		}
	}
}

// TestTruncate_JSONAlwaysReturnsParseableOutput is the end-to-end invariant the
// model depends on: whatever truncate returns for a JSON payload, it can be read.
func TestTruncate_JSONAlwaysReturnsParseableOutput(t *testing.T) {
	p := &OutputProcessor{MaxBodyChars: 8000}

	shapes := map[string]any{
		"many small records": buildPaymentRequestRecords(20),
		"nested wrapper":     map[string]any{"content": []any{map[string]any{"type": "text", "text": string(mustJSON(buildPaymentRequestRecords(20)))}}},
		"single long field":  map[string]any{"body": strings.Repeat("z", 40000)},
		"wide object":        wideObject(400),
		"list of scalars":    longScalarList(5000),
	}
	for name, shape := range shapes {
		raw, err := json.Marshal(shape)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if len(raw) <= p.MaxBodyChars {
			continue // nothing to truncate
		}
		out := p.truncate(string(raw))
		if len(out) > p.MaxBodyChars {
			t.Errorf("%s: output %d chars exceeds budget %d", name, len(out), p.MaxBodyChars)
		}
		if strings.Contains(out, "... [TRUNCATED") {
			t.Errorf("%s: JSON fell through to the plain-text path", name)
		}
		var v any
		if err := yaml.Unmarshal([]byte(out), &v); err != nil {
			t.Errorf("%s: output does not parse as YAML: %v\nhead: %.200s", name, err, out)
		}
	}
}

// TestTruncate_NonJSONUsesHeadTail keeps the CLI behaviour intact -- the fix must
// not regress the case head+tail was designed for.
func TestTruncate_NonJSONUsesHeadTail(t *testing.T) {
	p := &OutputProcessor{MaxBodyChars: 1000}
	in := "HEAD-MARKER\n" + strings.Repeat("log line\n", 500) + "TAIL-MARKER"

	out := p.truncate(in)
	if !strings.Contains(out, "HEAD-MARKER") || !strings.Contains(out, "TAIL-MARKER") {
		t.Fatalf("plain text truncation must keep both ends:\n%.300s", out)
	}
	if !strings.Contains(out, "TRUNCATED") {
		t.Fatal("plain text truncation must be labelled")
	}
}

// TestTruncateJSONFields_ObjectDropsKeysDeterministically covers the object
// shape: drop keys in a stable order and say how many remain.
func TestTruncateJSONFields_ObjectDropsKeysDeterministically(t *testing.T) {
	obj := wideObject(300)
	raw, _ := json.Marshal(obj)

	got, ok := truncateJSONFields(string(raw), 4000)
	if !ok {
		t.Fatal("object shape must be truncatable, not fall through")
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output must parse as YAML: %v", err)
	}
	if len(parsed) == 0 {
		t.Fatal("expected at least one key")
	}
	if !strings.Contains(got, "TRUNCATED: showing the first") {
		t.Fatalf("dropped keys must be announced:\n%.300s", got)
	}

	// Determinism: map iteration order is randomised, so the same input must still
	// produce byte-identical output or batch comparisons drift.
	again, _ := truncateJSONFields(string(raw), 4000)
	if again != got {
		t.Fatal("object truncation must be deterministic across calls")
	}
}

func wideObject(n int) map[string]any {
	m := make(map[string]any, n)
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("field_%03d", i)] = strings.Repeat("v", 100)
	}
	return m
}

func longScalarList(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("item-%05d", i))
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// buildJSONL renders n records one per line, sized so that the whole stream is
// several times the budget.
func buildJSONL(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		rec := map[string]any{
			"event_id":   fmt.Sprintf("evt-%05d", i),
			"ts":         "2026-07-01T19:42:11Z",
			"level":      "info",
			"message":    strings.Repeat("m", 400),
			"request_id": fmt.Sprintf("req-%05d", i),
		}
		b.Write(mustJSON(rec))
		b.WriteString("\n")
	}
	return b.String()
}

// TestTruncate_JSONLKeepsWholeRecords pins the shape of JSONL truncation: cut on
// line boundaries, never mid-record, and state how many records were kept.
//
// The old plain-text fallback sliced at byte maxChars/2, so the last retained
// line and the first tail line were both invalid JSON, and nothing in the output
// told the model that records were missing.
func TestTruncate_JSONLKeepsWholeRecords(t *testing.T) {
	const budget = 8000
	stream := buildJSONL(40)
	if len(stream) <= budget {
		t.Fatalf("fixture must exceed the budget, got %d", len(stream))
	}

	p := &OutputProcessor{MaxBodyChars: budget}
	out := p.truncate(stream)

	if !strings.HasPrefix(out, "# This tool returned newline-delimited JSON") {
		t.Fatalf("JSONL output must announce the re-encoding, got: %.160s", out)
	}
	if len(out) > budget {
		t.Fatalf("output %d chars exceeds budget %d", len(out), budget)
	}

	// Every non-comment line must be a complete, individually-valid record.
	kept := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d is not valid JSON: %.120s", kept, line)
		}
		kept++
	}
	if kept == 0 || kept >= 40 {
		t.Fatalf("expected 1..39 complete records kept, got %d", kept)
	}

	// The count in the header must match what is actually there, otherwise the
	// model is told a number it cannot rely on.
	want := fmt.Sprintf("showing the first %d of 40 records", kept)
	if !strings.Contains(out, want) {
		t.Fatalf("header must state %q, got: %.240s", want, out)
	}
}

// TestTruncate_JSONLDoesNotStealPrettyPrintedJSON guards the detection rule: a
// pretty-printed single document also spans many lines, and its inner lines are
// neither objects nor arrays. If JSONL detection were looser, such documents
// would be cut as if they were line-based and lose their structure.
func TestTruncate_JSONLDoesNotStealPrettyPrintedJSON(t *testing.T) {
	const budget = 8000
	records := buildPaymentRequestRecords(20)
	pretty, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(pretty) <= budget {
		t.Fatalf("fixture must exceed the budget, got %d", len(pretty))
	}

	p := &OutputProcessor{MaxBodyChars: budget}
	out := p.truncate(string(pretty))

	if strings.Contains(out, "newline-delimited JSON") {
		t.Fatal("pretty-printed JSON must not be treated as JSONL")
	}
	if !strings.Contains(out, "shown as YAML") && !strings.Contains(out, "TRUNCATED") {
		t.Fatalf("expected the single-document path, got: %.200s", out)
	}
}
