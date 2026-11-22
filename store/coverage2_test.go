package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestAddJITCandidate_Defaults(t *testing.T) {
	es := &ExperienceStore{
		JITCandidates: make(map[string]*JITCandidate),
	}
	c := &JITCandidate{ID: "j1"}
	if err := es.AddJITCandidate(context.Background(), c); err != nil {
		t.Fatalf("AddJITCandidate failed: %v", err)
	}
	if c.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
	if c.LastSeen.IsZero() {
		t.Error("expected LastSeen to be set")
	}
	if c.Status != "pending" {
		t.Errorf("Status = %q, want pending", c.Status)
	}
}

func TestAddJITCandidate_PreservesExistingValues(t *testing.T) {
	es := &ExperienceStore{
		JITCandidates: make(map[string]*JITCandidate),
	}
	now := time.Now().Add(-1 * time.Hour)
	c := &JITCandidate{ID: "j1", Status: "audited", CreatedAt: now, LastSeen: now}
	if err := es.AddJITCandidate(context.Background(), c); err != nil {
		t.Fatalf("AddJITCandidate failed: %v", err)
	}
	if c.Status != "audited" {
		t.Errorf("Status = %q, want audited", c.Status)
	}
	if !c.CreatedAt.Equal(now) {
		t.Error("expected CreatedAt to be preserved")
	}
}

func TestGetJITCandidates(t *testing.T) {
	es := &ExperienceStore{
		JITCandidates: map[string]*JITCandidate{
			"j1": {ID: "j1"},
			"j2": {ID: "j2"},
		},
	}
	candidates := es.GetJITCandidates()
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates["j1"] == nil || candidates["j2"] == nil {
		t.Error("expected both candidates")
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`{"failure_mode": "timeout"}`, `{"failure_mode": "timeout"}`},
		{`text before {"key": "val"} text after`, `{"key": "val"}`},
		{`no json here`, `no json here`},
		{`{}`, `{}`},
		{`}{`, `}{`},
	}
	for _, tt := range tests {
		got := extractJSON(tt.input)
		if got != tt.want {
			t.Errorf("extractJSON(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRunFMCBatch_NoPendingNodes(t *testing.T) {
	es := &ExperienceStore{
		Nodes: make(map[string]*ExperienceNode),
	}
	count, err := es.RunFMCBatch(context.Background(), nil, "model1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 updates, got %d", count)
	}
}

func TestGetFailureModeProfile(t *testing.T) {
	es := &ExperienceStore{
		Nodes: map[string]*ExperienceNode{
			"n1": {NodeID: "n1", ModelID: "m1", Outcome: "failure", FailureMode: "timeout_exceeded"},
			"n2": {NodeID: "n2", ModelID: "m1", Outcome: "failure", FailureMode: "timeout_exceeded"},
			"n3": {NodeID: "n3", ModelID: "m1", Outcome: "failure", FailureMode: "logic_error"},
			"n4": {NodeID: "n4", ModelID: "m2", Outcome: "failure", FailureMode: "timeout_exceeded"},
			"n5": {NodeID: "n5", ModelID: "m1", Outcome: "success", FailureMode: ""},
		},
	}
	profile := es.GetFailureModeProfile("m1")
	if profile == nil {
		t.Fatal("expected non-nil profile")
	}
	if profile.ModelID != "m1" {
		t.Errorf("ModelID = %q, want m1", profile.ModelID)
	}
	if profile.Counts["timeout_exceeded"] != 2 {
		t.Errorf("timeout_exceeded count = %d, want 2", profile.Counts["timeout_exceeded"])
	}
	if profile.Counts["logic_error"] != 1 {
		t.Errorf("logic_error count = %d, want 1", profile.Counts["logic_error"])
	}
}

func TestGetFailureModeProfile_NoMatches(t *testing.T) {
	es := &ExperienceStore{
		Nodes: map[string]*ExperienceNode{},
	}
	profile := es.GetFailureModeProfile("nonexistent")
	if profile == nil {
		t.Fatal("expected non-nil profile even with no matches")
	}
	if len(profile.Counts) != 0 {
		t.Errorf("expected empty counts, got %d", len(profile.Counts))
	}
}

func TestGetStatePotential(t *testing.T) {
	es := &ExperienceStore{
		StatePotentials: map[string]*StatePotential{
			"hash1": {StateHash: "hash1", Potential: 0.75},
		},
	}
	if p := es.GetStatePotential("hash1"); p != 0.75 {
		t.Errorf("GetStatePotential(hash1) = %f, want 0.75", p)
	}
	if p := es.GetStatePotential("nonexistent"); p != 1.0 {
		t.Errorf("GetStatePotential(nonexistent) = %f, want 1.0 (optimistic default)", p)
	}
}

func TestRecordEnvironmentIssue_New(t *testing.T) {
	es := &ExperienceStore{
		EnvironmentIssues: make(map[string]*EnvironmentIssue),
	}
	if err := es.RecordEnvironmentIssue(context.Background(), "npm install", "missing dep"); err != nil {
		t.Fatalf("RecordEnvironmentIssue failed: %v", err)
	}
	issue := es.EnvironmentIssues["npm install"]
	if issue == nil {
		t.Fatal("expected issue to be created")
	}
	if issue.Message != "missing dep" {
		t.Errorf("Message = %q, want missing dep", issue.Message)
	}
	if issue.Count != 1 {
		t.Errorf("Count = %d, want 1", issue.Count)
	}
	if issue.IsResolved {
		t.Error("expected IsResolved to be false for new issue")
	}
}

func TestRecordEnvironmentIssue_Update(t *testing.T) {
	es := &ExperienceStore{
		EnvironmentIssues: map[string]*EnvironmentIssue{
			"npm install": {Command: "npm install", Count: 3, IsResolved: true},
		},
	}
	if err := es.RecordEnvironmentIssue(context.Background(), "npm install", "new error"); err != nil {
		t.Fatalf("RecordEnvironmentIssue failed: %v", err)
	}
	issue := es.EnvironmentIssues["npm install"]
	if issue.Count != 4 {
		t.Errorf("Count = %d, want 4", issue.Count)
	}
	if issue.IsResolved {
		t.Error("expected IsResolved to be reset to false")
	}
	if issue.Message != "new error" {
		t.Errorf("Message = %q, want new error", issue.Message)
	}
}

func TestReindex_NoPatterns(t *testing.T) {
	dir := t.TempDir()
	es, err := NewExperienceStore(dir, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore failed: %v", err)
	}
	if err := es.Reindex(context.Background(), nil, "model1"); err != nil {
		t.Fatalf("Reindex failed: %v", err)
	}
}

func TestMemoryBankStore_SetEmbeddingClient(t *testing.T) {
	s := NewMemoryBankStore(t.TempDir(), nil)
	s.SetEmbeddingClient(nil)
}

func TestTaskStore_GetByRef(t *testing.T) {
	backend := &mockTaskBackend{
		data: map[string][]byte{
			"task1:step1": []byte(`{"data":"result"}`),
		},
	}
	ts := &TaskStore{backend: backend}
	result, err := ts.GetByRef(context.Background(), "task1:step1")
	if err != nil {
		t.Fatalf("GetByRef failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestTaskStore_GetByRef_InvalidFormat(t *testing.T) {
	ts := &TaskStore{backend: &mockTaskBackend{}}
	result, err := ts.GetByRef(context.Background(), "invalid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil result for invalid ref format")
	}
}

func TestTaskStore_Claim(t *testing.T) {
	backend := &mockTaskBackend{claimResult: true}
	ts := &TaskStore{backend: backend}
	ok, err := ts.Claim(context.Background(), "task1", "step1")
	if err != nil {
		t.Fatalf("Claim failed: %v", err)
	}
	if !ok {
		t.Error("expected Claim to return true")
	}
}

func TestStorageCompactor_Start_CancelledContext(t *testing.T) {
	c := NewStorageCompactor(nil, RetentionConfig{AutoVacuumIntervalDays: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Start(ctx)
}

func TestAutoDepositDecisionRecovery(t *testing.T) {
	s := NewAntiPatternStore(nil, nil)
	s.AutoDepositDecisionRecovery(context.Background(), "routing", "timeout_exceeded", "slow_api", "hangs on large input", "add timeout", nil)
	if s.PrecedentCount() != 1 {
		t.Errorf("expected 1 precedent, got %d", s.PrecedentCount())
	}
}

func TestBestSiblingScoreLocked(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: map[string]*GeneratedSkill{
			"gs1": {ID: "gs1", Capability: "coding"},
			"gs2": {ID: "gs2", Capability: "coding"},
			"gs3": {ID: "gs3", Capability: "testing"},
		},
		SkillAffinities: map[string]*SkillAffinity{
			"coding:gs2": {AddedSkill: "gs2", ConfidenceDelta: 0.15, SampleCount: 5},
		},
	}
	best, found := es.bestSiblingScoreLocked("coding", "gs1", 3)
	if !found {
		t.Fatal("expected to find a sibling score")
	}
	if best != 0.15 {
		t.Errorf("best = %f, want 0.15", best)
	}
}

func TestBestSiblingScoreLocked_NoMatches(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: map[string]*GeneratedSkill{
			"gs1": {ID: "gs1", Capability: "coding"},
		},
		SkillAffinities: map[string]*SkillAffinity{},
	}
	_, found := es.bestSiblingScoreLocked("coding", "gs1", 3)
	if found {
		t.Error("expected no matches")
	}
}

func TestFileExperienceBackend_GraphNoOps(t *testing.T) {
	b := &FileExperienceBackend{dir: t.TempDir()}
	ctx := context.Background()

	result, err := b.GetCompoundCandidates(ctx, 1, 0.5)
	if err != nil || result != nil {
		t.Errorf("GetCompoundCandidates: result=%v, err=%v", result, err)
	}

	result, err = b.GetCooccurrencePartners(ctx, "toolA", 1)
	if err != nil || result != nil {
		t.Errorf("GetCooccurrencePartners: result=%v, err=%v", result, err)
	}

	if err := b.SaveExperienceNode(ctx, &ExperienceNode{NodeID: "n1"}); err != nil {
		t.Errorf("SaveExperienceNode: %v", err)
	}

	if err := b.SaveExperienceEdge(ctx, ExperienceEdge{SourceID: "n1", TargetID: "n2"}); err != nil {
		t.Errorf("SaveExperienceEdge: %v", err)
	}

	if err := b.SavePromotionAuditLog(ctx, &PromotionAuditLog{CandidateID: "j1"}); err != nil {
		t.Errorf("SavePromotionAuditLog: %v", err)
	}
}

func TestCooccurrenceEntry_SuccessRate(t *testing.T) {
	c := &CooccurrenceEntry{CoCount: 10, SuccessCount: 7}
	if r := c.SuccessRate(); r != 0.7 {
		t.Errorf("SuccessRate = %f, want 0.7", r)
	}
	c = &CooccurrenceEntry{CoCount: 0}
	if r := c.SuccessRate(); r != 0 {
		t.Errorf("SuccessRate = %f, want 0", r)
	}
}

func TestLoadRolesFromDisk_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	roles, err := loadRolesFromDisk(dir)
	if err != nil {
		t.Fatalf("loadRolesFromDisk failed: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("expected 0 roles, got %d", len(roles))
	}
}

func TestLoadRolesFromDisk_ValidFile(t *testing.T) {
	dir := t.TempDir()
	role := config.Role{ID: "r1", Name: "Role 1"}
	data, _ := json.Marshal(role)
	os.WriteFile(filepath.Join(dir, "r1.json"), data, 0644)

	roles, err := loadRolesFromDisk(dir)
	if err != nil {
		t.Fatalf("loadRolesFromDisk failed: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("expected 1 role, got %d", len(roles))
	}
	if roles["r1"] == nil {
		t.Fatal("expected r1 in roles")
	}
	if roles["r1"].Name != "Role 1" {
		t.Errorf("Name = %q, want Role 1", roles["r1"].Name)
	}
}

func TestLoadRolesFromDisk_DeriveIDFromFilename(t *testing.T) {
	dir := t.TempDir()
	role := config.Role{Name: "No ID Role"}
	data, _ := json.Marshal(role)
	os.WriteFile(filepath.Join(dir, "auto_role.json"), data, 0644)

	roles, err := loadRolesFromDisk(dir)
	if err != nil {
		t.Fatalf("loadRolesFromDisk failed: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("expected 1 role, got %d", len(roles))
	}
	if roles["auto_role"] == nil {
		t.Fatal("expected auto_role in roles")
	}
}

func TestLoadRolesFromDisk_MalformedFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{invalid json`), 0644)

	roles, err := loadRolesFromDisk(dir)
	if err != nil {
		t.Fatalf("loadRolesFromDisk failed: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("expected 0 roles from malformed file, got %d", len(roles))
	}
}

func TestBootstrapConfigSync_EmptyDir_EmptyDB(t *testing.T) {
	dir := t.TempDir()
	repo := &mockConfigRepo{}
	roles, err := BootstrapConfigSync(repo, dir)
	if err != nil {
		t.Fatalf("BootstrapConfigSync failed: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("expected 0 roles, got %d", len(roles))
	}
}

func TestBootstrapConfigSync_FileFirst(t *testing.T) {
	dir := t.TempDir()
	role := config.Role{ID: "r1", Name: "From File"}
	data, _ := json.Marshal(role)
	os.WriteFile(filepath.Join(dir, "r1.json"), data, 0644)

	repo := &mockConfigRepo{}
	roles, err := BootstrapConfigSync(repo, dir)
	if err != nil {
		t.Fatalf("BootstrapConfigSync failed: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("expected 1 role, got %d", len(roles))
	}
	if roles["r1"] == nil {
		t.Fatal("expected r1 in roles")
	}
	if !repo.syncCalled {
		t.Error("expected SyncFileToDB to be called")
	}
}

func TestBootstrapConfigSync_FallbackToDB(t *testing.T) {
	dir := t.TempDir()
	repo := &mockConfigRepo{
		dbRoles: map[string]*config.Role{
			"db1": {ID: "db1", Name: "From DB"},
		},
	}
	roles, err := BootstrapConfigSync(repo, dir)
	if err != nil {
		t.Fatalf("BootstrapConfigSync failed: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("expected 1 role, got %d", len(roles))
	}
	if roles["db1"] == nil {
		t.Fatal("expected db1 in roles")
	}
	if repo.syncCalled {
		t.Error("expected SyncFileToDB NOT to be called when falling back to DB")
	}
}

type mockTaskBackend struct {
	data        map[string][]byte
	claimResult bool
}

func (m *mockTaskBackend) Save(ctx context.Context, taskID, stepID string, data []byte) error {
	return nil
}
func (m *mockTaskBackend) Load(ctx context.Context, taskID, stepID string) ([]byte, error) {
	key := taskID + ":" + stepID
	if d, ok := m.data[key]; ok {
		return d, nil
	}
	return nil, nil
}
func (m *mockTaskBackend) Delete(ctx context.Context, taskID string) (int, error) {
	return 0, nil
}
func (m *mockTaskBackend) Claim(ctx context.Context, taskID, stepID string) (bool, error) {
	return m.claimResult, nil
}

type mockConfigRepo struct {
	dbRoles    map[string]*config.Role
	syncCalled bool
}

func (m *mockConfigRepo) SaveRoleAndPersistFile(role *config.Role, targetPath string) error {
	return nil
}
func (m *mockConfigRepo) SyncFileToDB(roles map[string]*config.Role) error {
	m.syncCalled = true
	return nil
}
func (m *mockConfigRepo) SyncSOPsToDB(sops map[string]*schemas.SOP) error {
	return nil
}
func (m *mockConfigRepo) LoadRolesFromDB() (map[string]*config.Role, error) {
	return m.dbRoles, nil
}
func (m *mockConfigRepo) DeleteRole(roleID, jsonFilePath string) error {
	return nil
}
