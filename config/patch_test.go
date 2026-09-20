package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

type testStruct struct {
	Name  string            `json:"name"`
	Score int               `json:"score"`
	Items []string          `json:"items"`
	Meta  map[string]string `json:"meta"`
	Sub   *testStruct       `json:"sub,omitempty"`
}

func TestPatch_Apply(t *testing.T) {
	t.Run("Add_Replace_Map", func(t *testing.T) {
		target := &testStruct{
			Meta: make(map[string]string),
		}

		ops := []PatchOp{
			{Op: OpAdd, Path: "/name", Value: json.RawMessage(`"test"`)},
			{Op: OpAdd, Path: "/meta/key1", Value: json.RawMessage(`"val1"`)},
			{Op: OpReplace, Path: "/name", Value: json.RawMessage(`"updated"`)},
		}

		if err := ApplyPatch(target, ops); err != nil {
			t.Fatalf("ApplyPatch failed: %v", err)
		}

		if target.Name != "updated" {
			t.Errorf("expected updated, got %s", target.Name)
		}
		if target.Meta["key1"] != "val1" {
			t.Errorf("expected val1, got %s", target.Meta["key1"])
		}
	})

	t.Run("Slice_Operations", func(t *testing.T) {
		target := &testStruct{
			Items: []string{"a", "b"},
		}

		ops := []PatchOp{
			{Op: OpAdd, Path: "/items/1", Value: json.RawMessage(`"x"`)}, // Insert at index 1
			{Op: OpAdd, Path: "/items/-", Value: json.RawMessage(`"z"`)}, // Append to end
		}

		if err := ApplyPatch(target, ops); err != nil {
			t.Fatalf("ApplyPatch failed: %v", err)
		}

		expected := []string{"a", "x", "b", "z"}
		if !reflect.DeepEqual(target.Items, expected) {
			t.Errorf("expected %v, got %v", expected, target.Items)
		}

		// Test Remove from slice
		if err := ApplyPatch(target, []PatchOp{{Op: OpRemove, Path: "/items/1"}}); err != nil {
			t.Fatalf("Remove failed: %v", err)
		}
		expectedRem := []string{"a", "b", "z"}
		if !reflect.DeepEqual(target.Items, expectedRem) {
			t.Errorf("expected %v, got %v", expectedRem, target.Items)
		}
	})

	t.Run("Test_Operation", func(t *testing.T) {
		target := &testStruct{Name: "valid"}

		// Success
		if err := ApplyPatch(target, []PatchOp{{Op: OpTest, Path: "/name", Value: json.RawMessage(`"valid"`)}}); err != nil {
			t.Errorf("Test failed: %v", err)
		}

		// Failure
		if err := ApplyPatch(target, []PatchOp{{Op: OpTest, Path: "/name", Value: json.RawMessage(`"wrong"`)}}); err == nil {
			t.Error("expected Test error, got nil")
		}
	})

	t.Run("Registry_Patch", func(t *testing.T) {
		reg := &Registry{
			Roles: make(map[string]*Role),
		}

		roleJSON, _ := json.Marshal(Role{ID: "role1", Name: "Test Role"})
		ops := []PatchOp{
			{Op: OpAdd, Path: "/roles/role1", Value: json.RawMessage(roleJSON)},
		}

		if err := ApplyPatch(reg, ops); err != nil {
			t.Fatalf("Registry patch failed: %v", err)
		}

		if reg.Roles["role1"] == nil || reg.Roles["role1"].Name != "Test Role" {
			t.Errorf("Role not correctly patched: %+v", reg.Roles["role1"])
		}
	})

	t.Run("Case_Insensitive_Struct", func(t *testing.T) {
		target := &testStruct{}
		ops := []PatchOp{
			{Op: OpAdd, Path: "/NAME", Value: json.RawMessage(`"case-test"`)},
		}
		if err := ApplyPatch(target, ops); err != nil {
			t.Fatalf("Case insensitive patch failed: %v", err)
		}
		if target.Name != "case-test" {
			t.Errorf("expected case-test, got %s", target.Name)
		}
	})
}
