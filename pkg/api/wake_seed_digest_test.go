package api

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The digest exists because the wake turn was handed a verdict and a list of file
// names and nothing else, and answered with invented figures in 10/10 runs on
// harness-bench 041 (score 1.00 — the child writes the deliverable, so the oracle
// never sees the answer). These tests pin the two properties the fix depends on:
// the headline scalars are quotable, and the block cannot blow up the turn.

func TestBuildArtifactDigest_JSONTopLevelScalars(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "capacity_plan.json", `{
      "total_hourly_cost_usd": 0.552,
      "services": [{"service": "checkout-api", "selected_instance_type": "c7g.xlarge", "on_demand_count": 3}],
      "assumptions": ["a", "b"]
    }`)

	got, _ := buildArtifactDigest(dir, []string{"capacity_plan.json"}, 4000)

	for _, want := range []string{
		"### capacity_plan.json",
		"total_hourly_cost_usd: 0.552", // the exact figure an answer must quote
		"services: [1 item(s)]",        // shape, not a dump
		"selected_instance_type=c7g.xlarge",
		"assumptions: [2 item(s)]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest must contain %q; got:\n%s", want, got)
		}
	}
	// A nested object is described, not expanded: expanding it would be the file.
	if strings.Contains(got, `"service": "checkout-api"`) {
		t.Errorf("digest should not dump the nested object verbatim:\n%s", got)
	}
}

func TestBuildArtifactDigest_CSVHeaderAndRows(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "risk_tradeoffs.csv", "service,tradeoff,risk\n"+
		"checkout-api,Budget,tight\npricing-worker,Spot,preempt\nc1,x,y\nc2,x,y\n")

	got, _ := buildArtifactDigest(dir, []string{"risk_tradeoffs.csv"}, 4000)

	if !strings.Contains(got, "service,tradeoff,risk") {
		t.Errorf("header row missing:\n%s", got)
	}
	if !strings.Contains(got, "checkout-api,Budget,tight") {
		t.Errorf("first data row missing:\n%s", got)
	}
	// The excerpt must say it is an excerpt; a short file and a truncated one read
	// the same otherwise (iron law 220).
	if !strings.Contains(got, "more row(s)") {
		t.Errorf("a truncated table must state how many rows were dropped:\n%s", got)
	}
}

func TestBuildArtifactDigest_RejectsPathEscape(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, outer, "secret.txt", "TOP-SECRET-VALUE")

	got, skipped := buildArtifactDigest(root, []string{"../secret.txt"}, 4000)

	if strings.Contains(got, "TOP-SECRET-VALUE") {
		t.Fatalf("digest read a file outside the workspace:\n%s", got)
	}
	// Declared names come from model output, so a silent skip would hide an
	// attempted escape; it has to be reported.
	if !strings.Contains(strings.Join(skipped, ", "), "outside the workspace") {
		t.Errorf("escape attempt must be reported to the caller; skipped=%v", skipped)
	}
}

func TestBuildArtifactDigest_SkipsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Symlink needs either developer mode or SeCreateSymbolicLinkPrivilege
		// on Windows, and Git Bash's `ln -s` produces a copy — so this can only be
		// verified on POSIX. Recorded as a skip rather than a pass (iron law: an
		// unmeasured gate is not a passing gate).
		t.Skip("symlink creation requires privileges on Windows")
	}
	outer := t.TempDir()
	root := filepath.Join(outer, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, outer, "secret.txt", "TOP-SECRET-VALUE")
	if err := os.Symlink(filepath.Join(outer, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("cannot create symlink here: %v", err)
	}

	got, skipped := buildArtifactDigest(root, []string{"link.txt"}, 4000)

	if strings.Contains(got, "TOP-SECRET-VALUE") {
		t.Fatalf("digest followed a symlink out of the workspace:\n%s", got)
	}
	if !strings.Contains(strings.Join(skipped, ", "), "symlink") {
		t.Errorf("a skipped symlink must be named; skipped=%v", skipped)
	}
}

func TestBuildArtifactDigest_BudgetIsRespected(t *testing.T) {
	dir := t.TempDir()
	var names []string
	for i := 0; i < 8; i++ {
		name := "notes" + string(rune('a'+i)) + ".md"
		writeFile(t, dir, name, strings.Repeat("x", wakeDigestFileChars))
		names = append(names, name)
	}

	got, _ := buildArtifactDigest(dir, names, 2000)

	if len(got) > 2600 { // budget + header/footer slack
		t.Errorf("digest exceeded its budget: %d chars for budget 2000", len(got))
	}
	if !strings.Contains(got, "budget exhausted") && !strings.Contains(got, "truncated") {
		t.Errorf("truncation must be stated, not silent:\n%.200s", got)
	}
}

func TestBuildArtifactDigest_NothingReadableReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, skipped := buildArtifactDigest(dir, []string{"missing.json"}, 4000)
	if got != "" {
		// A heading with nothing under it invites the model to fill the gap from
		// priors — that is the defect this block exists to remove, not to restate.
		t.Errorf("a digest with no readable artifact must be empty, not a stub; got:\n%s", got)
	}
	if len(skipped) == 0 {
		t.Error("a missing artifact must be reported to the caller, not dropped silently")
	}
	if got, _ := buildArtifactDigest(dir, nil, 4000); got != "" {
		t.Errorf("nil manifest must produce no digest; got:\n%s", got)
	}
}

func TestBuildArtifactDigest_SkipsBinary(t *testing.T) {
	dir := t.TempDir()
	// Task 111 declares state/hris_payroll.db, and the fallback branch used to
	// paste a prefix of SQLite pages into the prompt. NUL in the first 8 KiB is
	// what makes a file binary for git and for file(1); same rule here.
	bin := append([]byte("SQLite format 3\x00"), bytes.Repeat([]byte{0x00, 0x01, 0x02, 0xff}, 4096)...)
	if err := os.WriteFile(filepath.Join(dir, "hris_payroll.db"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "summary.json", `{"rows": 42}`)

	got, skipped := buildArtifactDigest(dir, []string{"hris_payroll.db", "summary.json"}, 4000)

	if strings.ContainsRune(got, 0) {
		t.Fatalf("digest carried NUL bytes into the prompt:\n%q", got)
	}
	if strings.Contains(got, "SQLite format 3") {
		t.Errorf("binary artifact was rendered as text:\n%s", got)
	}
	if !strings.Contains(strings.Join(skipped, ", "), "binary") {
		t.Errorf("a skipped binary must be named; skipped=%v", skipped)
	}
	// And the readable sibling still renders: one binary file must not cost the
	// rest of the digest.
	if !strings.Contains(got, "rows: 42") {
		t.Errorf("a binary artifact suppressed the readable one:\n%s", got)
	}
}

func TestBuildArtifactDigest_CapsTheReadOfAVeryLargeFile(t *testing.T) {
	dir := t.TempDir()
	// 4 MiB of text, with a sentinel only at the end. Reading the whole file to
	// show 1600 chars is the thing the cap exists to prevent.
	body := strings.Repeat("a", 4<<20) + "TAIL-SENTINEL"
	if err := os.WriteFile(filepath.Join(dir, "app.log"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, _ := buildArtifactDigest(dir, []string{"app.log"}, 4000)

	if strings.Contains(got, "TAIL-SENTINEL") {
		t.Error("digest read past the read cap")
	}
	if len(got) > 2600 {
		t.Errorf("digest exceeded its character budget: %d", len(got))
	}
	// The truncation must be attributed to the read cap, not left to look like a
	// short file.
	if !strings.Contains(got, "only the first") {
		t.Errorf("the read cap must be stated in the text:\n%.300s", got)
	}
}

func TestBuildArtifactDigest_InvalidUTF8IsReplaced(t *testing.T) {
	dir := t.TempDir()
	// Latin-1 "café" — text, no NUL, but not valid UTF-8. It reaches the provider
	// inside a JSON body, so it cannot be carried through as raw bytes.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte{'c', 'a', 'f', 0xe9, '\n'}, 0o644); err != nil {
		t.Fatal(err)
	}

	got, _ := buildArtifactDigest(dir, []string{"notes.txt"}, 4000)

	if !strings.HasPrefix(got, "\n\n## Artifact digest") {
		t.Fatalf("notes.txt was not rendered at all:\n%s", got)
	}
	if !strings.Contains(got, "caf\uFFFD") {
		t.Errorf("invalid byte was not replaced with U+FFFD:\n%q", got)
	}
}

func TestWakeSeedDigestMode_DefaultsOn(t *testing.T) {
	t.Setenv("VORTEX_WAKE_SEED_DIGEST", "")
	if !wakeSeedDigestMode() {
		t.Fatal("digest must be on by default (Fisher p=1.1e-05, harness-bench 041)")
	}
	t.Setenv("VORTEX_WAKE_SEED_DIGEST", "on")
	if !wakeSeedDigestMode() {
		t.Fatal("VORTEX_WAKE_SEED_DIGEST=on must enable the digest")
	}
	t.Setenv("VORTEX_WAKE_SEED_DIGEST", "off")
	if wakeSeedDigestMode() {
		t.Fatal("VORTEX_WAKE_SEED_DIGEST=off must disable the digest")
	}
}

// TestFollowUpTrigger_CarriesTheArtifactDigest is the end-to-end half: the block
// has to reach the prompt the provider sees, not merely be buildable.
func TestFollowUpTrigger_CarriesTheArtifactDigest(t *testing.T) {
	t.Setenv("VORTEX_WAKE_SEED_DIGEST", "on")
	h, store, provider := newFollowUpHandler(t, "noted")
	store.GetOrCreate("s_digest")

	root := t.TempDir()
	writeFile(t, root, "capacity_plan.json",
		`{"total_hourly_cost_usd":0.552,"selected_instance_type":"c7g.xlarge"}`)
	outcome := taskOutcome{
		Desc: "x", ID: "task-abc", Status: "completed",
		WorkspaceRoot: root, Declared: []string{"capacity_plan.json"},
	}

	digest := wakeArtifactDigest(outcome)
	if !strings.Contains(digest, "total_hourly_cost_usd: 0.552") {
		t.Fatalf("digest did not render the headline scalar:\n%s", digest)
	}
	if !h.startFollowUpTurnWithDigest("s_digest", "task-abc", outcome.summary(), digest, "") {
		t.Fatal("follow-up turn was not started for an idle session")
	}
	h.wg.Wait()

	if provider.calls == 0 {
		t.Fatal("provider was never called")
	}
	if !strings.Contains(provider.lastIn, "total_hourly_cost_usd: 0.552") {
		t.Errorf("the digest never reached the wake turn; prompt was:\n%s", provider.lastIn)
	}
	// The standing rule must survive next to the digest.
	if !strings.Contains(provider.lastIn, "do not rewrite the artifacts") {
		t.Errorf("the design rule was displaced by the digest:\n%s", provider.lastIn)
	}
}

// TestWakeGateOff_SendsTheSamePromptAsBeforeTheDigest turns the control arm into
// a checked property. The Dockerfile says "v91 with the gate off behaves like v90"
// and that is an assertion; a score comparison against an unverified control is
// not a score comparison (iron law 167). Two independent handlers, same session id,
// same task id: the only difference is whether the digest argument is the gate's
// output ("" when off) or nothing at all. The provider must see the same bytes.
func TestWakeGateOff_SendsTheSamePromptAsBeforeTheDigest(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "capacity_plan.json", `{"total_hourly_cost_usd": 0.552}`)
	outcome := taskOutcome{
		Desc: "x", ID: "task-abc", Status: "completed",
		WorkspaceRoot: root, Declared: []string{"capacity_plan.json"},
	}

	t.Setenv("VORTEX_WAKE_SEED_DIGEST", "on")
	if on := wakeArtifactDigest(outcome); !strings.Contains(on, "total_hourly_cost_usd: 0.552") {
		t.Fatalf("with the gate on the digest must carry the headline; got:\n%s", on)
	}

	t.Setenv("VORTEX_WAKE_SEED_DIGEST", "off")
	if off := wakeArtifactDigest(outcome); off != "" {
		t.Fatalf("with the gate off the digest must be empty; got:\n%s", off)
	}
	gateOff := wakeArtifactDigest(outcome)

	h1, s1, p1 := newFollowUpHandler(t, "noted")
	s1.GetOrCreate("s_gate")
	if !h1.startFollowUpTurn("s_gate", "task-abc", outcome.summary()) {
		t.Fatal("summary-only follow-up did not start")
	}
	h1.wg.Wait()

	h2, s2, p2 := newFollowUpHandler(t, "noted")
	s2.GetOrCreate("s_gate")
	if !h2.startFollowUpTurnWithDigest("s_gate", "task-abc", outcome.summary(), gateOff, "") {
		t.Fatal("gate-off follow-up did not start")
	}
	h2.wg.Wait()

	if p1.calls == 0 || p2.calls == 0 {
		t.Fatalf("provider was not called (calls=%d/%d)", p1.calls, p2.calls)
	}
	if p1.lastIn != p2.lastIn {
		t.Errorf("gate-off prompt differs from the pre-digest prompt:\n--- before ---\n%s\n--- after ---\n%s",
			p1.lastIn, p2.lastIn)
	}
	if strings.Contains(p2.lastIn, "Artifact digest") {
		t.Errorf("gate-off prompt carried a digest:\n%s", p2.lastIn)
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
