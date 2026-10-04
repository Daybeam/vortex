package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestWriteIfAbsent_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "test.json")
	data := []byte(`{"id":"test"}`)

	wrote, err := writeIfAbsent(path, data)
	if err != nil {
		t.Fatalf("writeIfAbsent: %v", err)
	}
	if !wrote {
		t.Error("expected wrote=true for new file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content mismatch: got %q want %q", got, data)
	}
}

func TestWriteIfAbsent_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	existing := []byte(`{"user":"edited"}`)
	if err := os.WriteFile(path, existing, 0644); err != nil {
		t.Fatal(err)
	}

	wrote, err := writeIfAbsent(path, []byte(`{"seed":"data"}`))
	if err != nil {
		t.Fatalf("writeIfAbsent: %v", err)
	}
	if wrote {
		t.Error("expected wrote=false for existing file")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(existing) {
		t.Error("existing file was overwritten")
	}
}

func TestSeedBuiltins_CreatesAbsentFiles(t *testing.T) {
	rolesDir := t.TempDir()
	sopsDir := t.TempDir()
	skillsDir := t.TempDir()

	r := &Registry{Roles: make(map[string]*Role), SOPs: make(map[string]*schemas.SOP), Skills: make(map[string]*Skill)}
	if err := r.SeedBuiltins(rolesDir, sopsDir, skillsDir); err != nil {
		t.Fatalf("SeedBuiltins: %v", err)
	}
	for _, role := range builtinRoles {
		path := filepath.Join(rolesDir, role.ID+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("role file %s not created: %v", role.ID, err)
		}
	}
	for _, sop := range builtinSOPs {
		path := filepath.Join(sopsDir, sop.ID+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("sop file %s not created: %v", sop.ID, err)
		}
	}
	for _, skill := range builtinSkills {
		path := filepath.Join(skillsDir, skill.ID+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("skill file %s not created: %v", skill.ID, err)
		}
	}
}

func TestSeedBuiltins_SkipsExistingFiles(t *testing.T) {
	rolesDir := t.TempDir()
	sopsDir := t.TempDir()
	skillsDir := t.TempDir()

	userData := []byte(`{"id":"user_custom","name":"My Role"}`)
	rolePath := filepath.Join(rolesDir, "sop_generator.json")
	os.WriteFile(rolePath, userData, 0644)

	r := &Registry{Roles: make(map[string]*Role), SOPs: make(map[string]*schemas.SOP), Skills: make(map[string]*Skill)}
	if err := r.SeedBuiltins(rolesDir, sopsDir, skillsDir); err != nil {
		t.Fatalf("SeedBuiltins: %v", err)
	}
	got, _ := os.ReadFile(rolePath)
	if string(got) != string(userData) {
		t.Error("existing role file was overwritten")
	}
}

func TestSeedBuiltins_Idempotent(t *testing.T) {
	rolesDir := t.TempDir()
	sopsDir := t.TempDir()
	skillsDir := t.TempDir()

	r := &Registry{Roles: make(map[string]*Role), SOPs: make(map[string]*schemas.SOP), Skills: make(map[string]*Skill)}
	if err := r.SeedBuiltins(rolesDir, sopsDir, skillsDir); err != nil {
		t.Fatalf("first SeedBuiltins: %v", err)
	}
	roleData1, _ := os.ReadFile(filepath.Join(rolesDir, "sop_generator.json"))
	if err := r.SeedBuiltins(rolesDir, sopsDir, skillsDir); err != nil {
		t.Fatalf("second SeedBuiltins: %v", err)
	}
	roleData2, _ := os.ReadFile(filepath.Join(rolesDir, "sop_generator.json"))
	if string(roleData1) != string(roleData2) {
		t.Error("second run modified the file")
	}
}

func TestSeedBuiltins_PartialExist(t *testing.T) {
	rolesDir := t.TempDir()
	sopsDir := t.TempDir()
	skillsDir := t.TempDir()

	os.WriteFile(filepath.Join(rolesDir, "sop_generator.json"), []byte(`{"id":"existing"}`), 0644)

	r := &Registry{Roles: make(map[string]*Role), SOPs: make(map[string]*schemas.SOP), Skills: make(map[string]*Skill)}
	if err := r.SeedBuiltins(rolesDir, sopsDir, skillsDir); err != nil {
		t.Fatalf("SeedBuiltins: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sopsDir, "sop_auto_generate.json")); err != nil {
		t.Error("missing SOP was not created")
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "sop_writing.json")); err != nil {
		t.Error("missing skill was not created")
	}
}

func TestSeedBuiltins_DAGValidation(t *testing.T) {
	for _, sop := range builtinSOPs {
		for stepID, step := range sop.Steps {
			for _, dep := range step.DependsOn {
				if _, ok := sop.Steps[dep]; !ok {
					t.Errorf("SOP %s step %q depends on non-existent step %q", sop.ID, stepID, dep)
				}
			}
		}
	}
}
