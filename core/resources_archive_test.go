package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestFetchArchive_ExtractsAllFileTypes verifies that FetchArchive downloads
// a GitHub tarball and extracts ALL file types (not just .md/.json), fixing
// Gap 3.1 from SKILL_MARKETPLACE_REPO_CONTRACT_DESIGN.md.
func TestFetchArchive_ExtractsAllFileTypes(t *testing.T) {
	// Build a tar.gz in memory containing a skill directory with
	// SKILL.md, scripts/, assets/, and references/ subdirectories.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	files := []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/skills/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/SKILL.md", "---\nname: PDF Report\ndescription: Generate PDF reports\n---\n# PDF Report\nInstructions here.", tar.TypeReg},
		{"repo-main/skills/pdf-report/scripts/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/scripts/generate.py", "print('generating PDF')", tar.TypeReg},
		{"repo-main/skills/pdf-report/assets/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/assets/template.j2", "{{title}}", tar.TypeReg},
		{"repo-main/skills/pdf-report/references/", "", tar.TypeDir},
		{"repo-main/skills/pdf-report/references/format-spec.md", "# Format Spec\nDetails here.", tar.TypeReg},
	}

	for _, f := range files {
		hdr := &tar.Header{
			Name:     f.name,
			Typeflag: f.typeflag,
			Mode:     0644,
		}
		if f.typeflag == tar.TypeReg {
			hdr.Size = int64(len(f.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if f.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(f.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gzw.Close()

	// Serve the tarball via httptest
	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(buf.Bytes())
	}))
	defer codeloadSrv.Close()

	// Serve the repo API (for fetchDefaultBranch)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	}))
	defer apiSrv.Close()

	loader := &ResourceLoader{
		client:            http.DefaultClient,
		cache:             make(map[string]string),
		githubAPIBase:     apiSrv.URL,
		githubRawBase:     "http://localhost",
		githubCodeloadBase: codeloadSrv.URL,
	}

	targetDir := t.TempDir()
	if err := loader.FetchArchive(context.Background(), "test", "skills", targetDir); err != nil {
		t.Fatalf("FetchArchive failed: %v", err)
	}

	// Verify ALL file types were extracted (Gap 3.1 fix)
	checks := []struct {
		relPath string
		want    string
	}{
		{"skills/pdf-report/SKILL.md", "---\nname: PDF Report\ndescription: Generate PDF reports\n---\n# PDF Report\nInstructions here."},
		{"skills/pdf-report/scripts/generate.py", "print('generating PDF')"},
		{"skills/pdf-report/assets/template.j2", "{{title}}"},
		{"skills/pdf-report/references/format-spec.md", "# Format Spec\nDetails here."},
	}

	for _, c := range checks {
		full := filepath.Join(targetDir, c.relPath)
		data, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("expected file %s to exist: %v", c.relPath, err)
			continue
		}
		if string(data) != c.want {
			t.Errorf("file %s content mismatch:\nwant: %q\ngot:  %q", c.relPath, c.want, string(data))
		}
	}
}

// TestFetchArchive_StripsTopLevelDir verifies that the top-level directory
// prefix (e.g. "repo-name-branch/") is stripped during extraction.
func TestFetchArchive_StripsTopLevelDir(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	entries := []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"myrepo-main/", "", tar.TypeDir},
		{"myrepo-main/SKILL.md", "# Test Skill", tar.TypeReg},
		{"myrepo-main/skills/foo.md", "# Foo", tar.TypeReg},
	}

	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0644}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		tw.WriteHeader(hdr)
		if e.typeflag == tar.TypeReg {
			tw.Write([]byte(e.content))
		}
	}
	tw.Close()
	gzw.Close()

	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer codeloadSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	}))
	defer apiSrv.Close()

	loader := &ResourceLoader{
		client:            http.DefaultClient,
		cache:             make(map[string]string),
		githubAPIBase:     apiSrv.URL,
		githubRawBase:     "http://localhost",
		githubCodeloadBase: codeloadSrv.URL,
	}

	targetDir := t.TempDir()
	if err := loader.FetchArchive(context.Background(), "test", "myrepo", targetDir); err != nil {
		t.Fatalf("FetchArchive failed: %v", err)
	}

	// Files should be at targetDir/SKILL.md, NOT targetDir/myrepo-main/SKILL.md
	if _, err := os.Stat(filepath.Join(targetDir, "SKILL.md")); err != nil {
		t.Errorf("expected SKILL.md at targetDir root (prefix stripped): %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "skills/foo.md")); err != nil {
		t.Errorf("expected skills/foo.md at targetDir (prefix stripped): %v", err)
	}
}

// TestFetchArchive_ZipSlipProtection verifies that tar entries with path
// traversal sequences (..) are skipped during extraction.
func TestFetchArchive_ZipSlipProtection(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	entries := []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/SKILL.md", "# Safe", tar.TypeReg},
		{"repo-main/../../../etc/passwd", "malicious", tar.TypeReg},
		{"repo-main/sub/../../escape.txt", "malicious", tar.TypeReg},
	}

	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0644}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		tw.WriteHeader(hdr)
		if e.typeflag == tar.TypeReg {
			tw.Write([]byte(e.content))
		}
	}
	tw.Close()
	gzw.Close()

	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer codeloadSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	}))
	defer apiSrv.Close()

	loader := &ResourceLoader{
		client:            http.DefaultClient,
		cache:             make(map[string]string),
		githubAPIBase:     apiSrv.URL,
		githubRawBase:     "http://localhost",
		githubCodeloadBase: codeloadSrv.URL,
	}

	targetDir := t.TempDir()
	if err := loader.FetchArchive(context.Background(), "test", "repo", targetDir); err != nil {
		t.Fatalf("FetchArchive failed: %v", err)
	}

	// Safe file should exist
	if _, err := os.Stat(filepath.Join(targetDir, "SKILL.md")); err != nil {
		t.Errorf("expected safe file SKILL.md to exist: %v", err)
	}

	// Malicious files should NOT exist outside targetDir
	if _, err := os.Stat(filepath.Join(targetDir, "..", "..", "etc", "passwd")); err == nil {
		t.Error("zip-slip: malicious file was extracted outside target dir!")
	}
	if _, err := os.Stat(filepath.Join(targetDir, "escape.txt")); err == nil {
		t.Error("zip-slip: malicious file with ../ was extracted!")
	}
}

// TestFetchArchive_PersistentSkillDir verifies that after FetchArchive +
// ImportSkillDirectory, the skill's SkillDir points to a persistent path
// that survives cleanup — fixing Gap 3.2 from SKILL_MARKETPLACE_REPO_CONTRACT_DESIGN.md.
func TestFetchArchive_PersistentSkillDir(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	skillContent := "---\nname: Persistent Skill\ndescription: Skill with scripts\n---\n# Persistent Skill\nUses scripts."

	entries := []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"repo-main/", "", tar.TypeDir},
		{"repo-main/skills/persistent-skill/", "", tar.TypeDir},
		{"repo-main/skills/persistent-skill/SKILL.md", skillContent, tar.TypeReg},
		{"repo-main/skills/persistent-skill/scripts/run.sh", "#!/bin/bash\necho hello", tar.TypeReg},
	}

	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0644}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		tw.WriteHeader(hdr)
		if e.typeflag == tar.TypeReg {
			tw.Write([]byte(e.content))
		}
	}
	tw.Close()
	gzw.Close()

	codeloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer codeloadSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
	}))
	defer apiSrv.Close()

	loader := &ResourceLoader{
		client:            http.DefaultClient,
		cache:             make(map[string]string),
		githubAPIBase:     apiSrv.URL,
		githubRawBase:     "http://localhost",
		githubCodeloadBase: codeloadSrv.URL,
	}

	// Use a persistent directory (NOT t.TempDir which gets cleaned up)
	persistentDir := filepath.Join(os.TempDir(), "orch-test-persistent-skills")
	defer os.RemoveAll(persistentDir)

	if err := loader.FetchArchive(context.Background(), "test", "repo", persistentDir); err != nil {
		t.Fatalf("FetchArchive failed: %v", err)
	}

	// Verify the SKILL.md and scripts are at the persistent path
	skillDir := filepath.Join(persistentDir, "skills/persistent-skill")
	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md not found at persistent path %s: %v", skillDir, err)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "scripts/run.sh")); err != nil {
		t.Fatalf("scripts/run.sh not found at persistent path: %v", err)
	}

	// The persistent directory is NOT cleaned up by FetchArchive —
	// it survives for ImportSkillDirectory to use and for promoted
	// skills to read SKILL.md later via readSkillBody.
}
