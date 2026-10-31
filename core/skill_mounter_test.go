package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestActivate_InjectsManifest(t *testing.T) {
	skill := &config.Skill{
		ID:          "ext_test",
		Name:        "Test Skill",
		Description: "A test skill for unit testing",
		SkillDir:    "/tmp/skills/test",
		Manifest: &config.SkillManifest{
			References: []string{"references/guide.md", "references/scorecard.md"},
			Scripts:    []string{"scripts/run.sh"},
			Assets:     []string{"assets/diagram.png"},
		},
	}

	m := &SkillMounter{}
	result := m.Activate(skill)

	if m.ActiveSkillID != "ext_test" {
		t.Errorf("expected ActiveSkillID=ext_test, got %s", m.ActiveSkillID)
	}
	if m.ActiveSkillDir != "/tmp/skills/test" {
		t.Errorf("expected ActiveSkillDir=/tmp/skills/test, got %s", m.ActiveSkillDir)
	}
	if !strings.Contains(result, "A test skill for unit testing") {
		t.Error("result should contain skill description")
	}
	if !strings.Contains(result, "Available Resources") {
		t.Error("result should contain manifest section")
	}
	if !strings.Contains(result, "references/guide.md") {
		t.Error("result should list reference files")
	}
	if !strings.Contains(result, "scripts/run.sh") {
		t.Error("result should list script files")
	}
	if !strings.Contains(result, "assets/diagram.png") {
		t.Error("result should list asset files")
	}
	if !strings.Contains(result, "relative paths") {
		t.Error("result should mention relative paths usage")
	}
}

func TestActivate_NoManifest_StillWorks(t *testing.T) {
	skill := &config.Skill{
		ID:          "ext_simple",
		Name:        "Simple Skill",
		Description: "A skill without a manifest",
		SkillDir:    "/tmp/skills/simple",
	}

	m := &SkillMounter{}
	result := m.Activate(skill)

	if !strings.Contains(result, "A skill without a manifest") {
		t.Error("result should contain skill description")
	}
	if strings.Contains(result, "Available Resources") {
		t.Error("result should NOT contain manifest section when manifest is nil")
	}
}

func TestActivate_EmptyManifest_StillWorks(t *testing.T) {
	skill := &config.Skill{
		ID:          "ext_empty",
		Name:        "Empty Manifest Skill",
		Description: "A skill with an empty manifest",
		SkillDir:    "/tmp/skills/empty",
		Manifest:    &config.SkillManifest{}, // all empty slices
	}

	m := &SkillMounter{}
	result := m.Activate(skill)

	if !strings.Contains(result, "A skill with an empty manifest") {
		t.Error("result should contain skill description")
	}
	if strings.Contains(result, "Available Resources") {
		t.Error("result should NOT contain manifest section when manifest is empty")
	}
}

func TestResolvePath_RelativeJoinsSkillDir(t *testing.T) {
	m := &SkillMounter{
		ActiveSkillDir: "/tmp/skills/test",
	}

	result := m.ResolvePath("references/guide.md")
	expected := filepath.Join("/tmp/skills/test", "references/guide.md")
	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestResolvePath_AbsolutePassthrough(t *testing.T) {
	m := &SkillMounter{
		ActiveSkillDir: "/tmp/skills/test",
	}

	// Use a platform-aware absolute path
	absPath := filepath.Join(os.TempDir(), "test_file.md")
	result := m.ResolvePath(absPath)
	if result != absPath {
		t.Errorf("absolute path should pass through, expected %s, got %s", absPath, result)
	}
}

func TestResolvePath_NoActiveDir_Passthrough(t *testing.T) {
	m := &SkillMounter{} // ActiveSkillDir is empty

	result := m.ResolvePath("references/guide.md")
	if result != "references/guide.md" {
		t.Errorf("with no ActiveSkillDir, path should pass through, got %s", result)
	}
}

func TestActivateTopSkills_FiltersAndActivates(t *testing.T) {
	reg := &config.Registry{
		Skills: map[string]*config.Skill{
			"ext_a": {
				ID:          "ext_a",
				Name:        "Skill A",
				Description: "Description A",
				SkillDir:    "/tmp/a",
				Manifest:    &config.SkillManifest{References: []string{"references/a.md"}},
			},
			"ext_b": {
				ID:          "ext_b",
				Name:        "Skill B",
				Description: "Description B",
				SkillDir:    "/tmp/b",
			},
		},
	}

	candidates := []DiscoveryCandidate{
		{ID: "ext_a", Type: CandidateSkill, Confidence: 0.9},
		{ID: "ext_b", Type: CandidateSkill, Confidence: 0.8},
		{ID: "ext_c", Type: CandidateRole, Confidence: 0.95}, // not a skill
	}

	result, skillDir := ActivateTopSkills(reg, candidates, 3)

	if !strings.Contains(result, "Active Skills") {
		t.Error("result should contain 'Active Skills' header")
	}
	if !strings.Contains(result, "Description A") {
		t.Error("result should contain skill A's description")
	}
	if !strings.Contains(result, "Description B") {
		t.Error("result should contain skill B's description")
	}
	if !strings.Contains(result, "references/a.md") {
		t.Error("result should contain skill A's manifest")
	}
	if skillDir != "/tmp/a" {
		t.Errorf("expected primary skill dir /tmp/a, got %q", skillDir)
	}
}

func TestActivateTopSkills_RespectsTopN(t *testing.T) {
	reg := &config.Registry{
		Skills: map[string]*config.Skill{
			"ext_a": {ID: "ext_a", Name: "A", Description: "Desc A", SkillDir: "/tmp/a"},
			"ext_b": {ID: "ext_b", Name: "B", Description: "Desc B", SkillDir: "/tmp/b"},
			"ext_c": {ID: "ext_c", Name: "C", Description: "Desc C", SkillDir: "/tmp/c"},
		},
	}

	candidates := []DiscoveryCandidate{
		{ID: "ext_a", Type: CandidateSkill, Confidence: 0.9},
		{ID: "ext_b", Type: CandidateSkill, Confidence: 0.8},
		{ID: "ext_c", Type: CandidateSkill, Confidence: 0.7},
	}

	result, _ := ActivateTopSkills(reg, candidates, 2)

	// Should contain A and B but not C
	if !strings.Contains(result, "Desc A") {
		t.Error("should contain skill A")
	}
	if !strings.Contains(result, "Desc B") {
		t.Error("should contain skill B")
	}
	if strings.Contains(result, "Desc C") {
		t.Error("should NOT contain skill C (topN=2)")
	}
}

func TestActivateTopSkills_NoSkills_ReturnsEmpty(t *testing.T) {
	reg := &config.Registry{
		Skills: map[string]*config.Skill{},
	}

	candidates := []DiscoveryCandidate{
		{ID: "ext_x", Type: CandidateRole, Confidence: 0.9},
	}

	result, skillDir := ActivateTopSkills(reg, candidates, 3)
	if result != "" {
		t.Errorf("expected empty string for no skill candidates, got %q", result)
	}
	if skillDir != "" {
		t.Errorf("expected empty skill dir for no skill candidates, got %q", skillDir)
	}
}

// ─── L2 body injection + standard fields (AGENTIC_SKILL_EXPLORATION_DESIGN.md G1/G2/G3) ──

// TestActivate_InjectsBodyAndStandardFields verifies that activating a
// directory-mode skill loads the SKILL.md body into the prompt (agentskills.io
// progressive disclosure L2) and surfaces compatibility + allowed-tools hints.
func TestActivate_InjectsBodyAndStandardFields(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "doc-writer")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: doc-writer\ndescription: Writes docs\n---\nWrite the doc carefully."
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	skill := &config.Skill{
		ID:            "ext_doc_writer",
		Name:          "doc-writer",
		Description:   "Writes docs",
		ExecutionMode: config.ExecutionModeDirectory,
		SkillDir:      skillDir,
		Compatibility: "Requires Python 3.14+ and uv",
		AllowedTools:  []string{"read_file", "write_file"},
	}

	m := &SkillMounter{}
	result := m.Activate(skill)

	if !strings.Contains(result, "Write the doc carefully.") {
		t.Error("result should contain the SKILL.md body (L2 injection)")
	}
	if !strings.Contains(result, "Skill Instructions") {
		t.Error("result should contain the Skill Instructions header")
	}
	if !strings.Contains(result, "Compatibility Requirements") {
		t.Error("result should contain the compatibility warning section")
	}
	if !strings.Contains(result, "Requires Python 3.14+ and uv") {
		t.Error("result should contain the compatibility text")
	}
	if !strings.Contains(result, "Pre-approved Tools") {
		t.Error("result should contain the pre-approved tools section")
	}
	if !strings.Contains(result, "read_file write_file") {
		t.Error("result should list the allowed tools")
	}
	// Frontmatter must be stripped from the injected body.
	if strings.Contains(result, "name: doc-writer") {
		t.Error("frontmatter leaked into the prompt body")
	}
}

// TestActivate_BodyTruncatedAtCap verifies the G1 overflow guard: a body
// larger than skillBodyPromptCap is truncated with a pointer to the full file.
func TestActivate_BodyTruncatedAtCap(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "big-skill")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("line-of-instructions\n", 1000) // ~21KB > cap
	content := "---\nname: big-skill\ndescription: d\n---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	skill := &config.Skill{
		ID:            "ext_big_skill",
		Description:   "d",
		ExecutionMode: config.ExecutionModeDirectory,
		SkillDir:      skillDir,
	}

	result := (&SkillMounter{}).Activate(skill)
	if !strings.Contains(result, "line-of-instructions") {
		t.Error("truncated body prefix should be present")
	}
	if !strings.Contains(result, "truncated") {
		t.Error("truncation notice should be present")
	}
	if got := strings.Count(result, "line-of-instructions"); got >= 1000 {
		t.Errorf("full body leaked: %d occurrences", got)
	}
}

// TestActivate_MissingSkillFile_DegradesToDescription verifies graceful
// degradation when the SkillDir has no readable SKILL.md.
func TestActivate_MissingSkillFile_DegradesToDescription(t *testing.T) {
	skill := &config.Skill{
		ID:            "ext_gone",
		Description:   "still here",
		ExecutionMode: config.ExecutionModeDirectory,
		SkillDir:      filepath.Join(t.TempDir(), "gone"),
	}

	result := (&SkillMounter{}).Activate(skill)
	if !strings.Contains(result, "still here") {
		t.Error("description should survive SKILL.md read failure")
	}
	if strings.Contains(result, "Skill Instructions") {
		t.Error("no instructions section expected when SKILL.md is unreadable")
	}
}
