package store

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestToSet(t *testing.T) {
	s := toSet([]string{"a", "b", "a"})
	if len(s) != 2 {
		t.Fatalf("expected 2 unique items, got %d", len(s))
	}
	if !s["a"] || !s["b"] {
		t.Error("expected a and b in set")
	}
}

func TestIntersectionSize(t *testing.T) {
	a := map[string]bool{"x": true, "y": true, "z": true}
	b := map[string]bool{"y": true, "z": true, "w": true}
	if n := intersectionSize(a, b); n != 2 {
		t.Errorf("intersectionSize = %d, want 2", n)
	}
}

func TestUnionSize(t *testing.T) {
	a := map[string]bool{"x": true, "y": true}
	b := map[string]bool{"y": true, "z": true}
	if n := unionSize(a, b); n != 3 {
		t.Errorf("unionSize = %d, want 3", n)
	}
}

func TestContains(t *testing.T) {
	if !contains([]string{"a", "b", "c"}, "b") {
		t.Error("expected to contain b")
	}
	if contains([]string{"a", "b", "c"}, "d") {
		t.Error("expected NOT to contain d")
	}
	if contains(nil, "a") {
		t.Error("expected NOT to contain a in nil slice")
	}
}

func TestContainsCombo(t *testing.T) {
	combos := [][]string{{"a", "b"}, {"c", "d"}}
	if !containsCombo(combos, []string{"a", "b"}) {
		t.Error("expected to contain [a,b]")
	}
	if containsCombo(combos, []string{"a", "c"}) {
		t.Error("expected NOT to contain [a,c]")
	}
	if containsCombo(combos, []string{"a", "b", "c"}) {
		t.Error("expected NOT to contain [a,b,c] (different length)")
	}
}

func TestSliceEqual(t *testing.T) {
	if !sliceEqual([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("expected equal slices")
	}
	if sliceEqual([]string{"a", "b"}, []string{"a", "c"}) {
		t.Error("expected NOT equal slices")
	}
	if sliceEqual([]string{"a"}, []string{"a", "b"}) {
		t.Error("expected NOT equal slices (different length)")
	}
	if !sliceEqual(nil, nil) {
		t.Error("expected nil slices to be equal")
	}
}

func TestSortedCopy(t *testing.T) {
	original := []string{"c", "a", "b"}
	cp := sortedCopy(original)
	if cp[0] != "a" || cp[1] != "b" || cp[2] != "c" {
		t.Errorf("sortedCopy = %v, want [a b c]", cp)
	}
	if original[0] != "c" {
		t.Error("original should not be modified")
	}
}

func TestMapValues(t *testing.T) {
	m := map[string]int{"a": 1, "b": 2, "c": 3}
	vals := mapValues(m)
	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	sort.Ints(vals)
	if vals[0] != 1 || vals[1] != 2 || vals[2] != 3 {
		t.Errorf("mapValues = %v, want [1 2 3]", vals)
	}
}

func TestWriteJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	writeJSON(path, map[string]string{"key": "value"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) == "" {
		t.Error("expected non-empty file")
	}
}

func TestWriteBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.dat")
	writeBytes(path, []byte("hello"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("data = %q, want hello", data)
	}
	writeBytes(path, nil)
}

func TestReadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	os.WriteFile(path, []byte(`{"key":"value"}`), 0644)
	var m map[string]string
	readJSON(path, &m)
	if m["key"] != "value" {
		t.Errorf("key = %q, want value", m["key"])
	}
	readJSON("/nonexistent/path.json", &m)
}

func TestScheduleStore_SaveListGetSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	ss := NewScheduleStore(path, nil)

	s := &schemas.Schedule{ID: "s1", CronExpr: "0 * * * *"}
	ss.Set(s)

	got := ss.Get("s1")
	if got == nil || got.ID != "s1" {
		t.Fatal("expected to get s1 after Set")
	}

	list := ss.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(list))
	}
	if list[0].ID != "s1" {
		t.Errorf("list[0].ID = %q, want s1", list[0].ID)
	}

	if err := ss.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if ss.GetPath() != path {
		t.Errorf("GetPath = %q, want %q", ss.GetPath(), path)
	}
}

func TestScheduleStore_GetNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	ss := NewScheduleStore(path, nil)
	if ss.Get("nonexistent") != nil {
		t.Error("expected nil for nonexistent schedule")
	}
}

func TestScheduleStore_ListEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")
	ss := NewScheduleStore(path, nil)
	list := ss.List()
	if len(list) != 0 {
		t.Errorf("expected 0 schedules, got %d", len(list))
	}
}

func TestTaskPattern_PatternID(t *testing.T) {
	p := TaskPattern{ID: "p1"}
	if p.PatternID() != "p1" {
		t.Errorf("PatternID = %q, want p1", p.PatternID())
	}
}

func TestTaskPattern_GetTaskType(t *testing.T) {
	p := TaskPattern{TaskType: "coding"}
	if p.GetTaskType() != "coding" {
		t.Errorf("GetTaskType = %q, want coding", p.GetTaskType())
	}
}

func TestFileTaskBackend_Claim(t *testing.T) {
	b := &FileTaskBackend{baseDir: t.TempDir()}
	ok, err := b.Claim(nil, "task1", "step1")
	if err != nil {
		t.Fatalf("Claim failed: %v", err)
	}
	if !ok {
		t.Error("expected Claim to return true")
	}
}
