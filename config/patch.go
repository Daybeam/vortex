package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// RFC 6902 JSON Patch operations
const (
	OpAdd     = "add"
	OpRemove  = "remove"
	OpReplace = "replace"
	OpMove    = "move"
	OpCopy    = "copy"
	OpTest    = "test"
)

// PatchOp represents a single JSON Patch operation.
type PatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	From  string          `json:"from,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// ApplyPatch applies a sequence of operations to a target object (must be a pointer).
func ApplyPatch(target any, ops []PatchOp) error {
	for _, op := range ops {
		if err := op.Apply(target); err != nil {
			return err
		}
	}
	return nil
}

// Apply executes a single patch operation on the target.
func (op *PatchOp) Apply(target any) error {
	val := reflect.ValueOf(target)
	if val.Kind() != reflect.Ptr || val.IsNil() {
		return fmt.Errorf("patch: target must be a non-nil pointer")
	}

	parts := strings.Split(strings.TrimPrefix(op.Path, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		parts = nil
	}

	switch op.Op {
	case OpAdd, OpReplace:
		return op.applyAddReplace(val.Elem(), parts)
	case OpRemove:
		return op.applyRemove(val.Elem(), parts)
	case OpTest:
		return op.applyTest(val.Elem(), parts)
	case OpMove:
		return op.applyMove(val.Elem(), parts)
	case OpCopy:
		return op.applyCopy(val.Elem(), parts)
	default:
		return fmt.Errorf("patch: unsupported operation %q", op.Op)
	}
}

func normalize(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "_", "")
}

func (op *PatchOp) applyAddReplace(v reflect.Value, parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("patch: root replacement not supported")
	}

	parent, key, err := navigate(v, parts)
	if err != nil {
		return err
	}

	// Determine target type from parent
	var targetType reflect.Type
	switch parent.Kind() {
	case reflect.Map:
		targetType = parent.Type().Elem()
	case reflect.Slice:
		targetType = parent.Type().Elem()
	case reflect.Struct:
		f, ok := parent.Type().FieldByName(key)
		if !ok {
			// Check for JSON tags or case-insensitive/snake_case match
			for i := 0; i < parent.Type().NumField(); i++ {
				sf := parent.Type().Field(i)
				tag := strings.Split(sf.Tag.Get("json"), ",")[0]
				if tag == key || strings.EqualFold(sf.Name, key) || normalize(sf.Name) == normalize(key) {
					f = sf
					ok = true
					break
				}
			}
		}
		if ok {
			targetType = f.Type
		}
	case reflect.Ptr:
		return fmt.Errorf("patch internal: navigate returned pointer for %s", op.Path)
	}

	if targetType == nil {
		return fmt.Errorf("patch: could not determine target type for path %s", op.Path)
	}

	nv := reflect.New(targetType)
	if err := json.Unmarshal(op.Value, nv.Interface()); err != nil {
		return fmt.Errorf("patch: unmarshal value failed for path %s: %w", op.Path, err)
	}
	newValue := nv.Elem()

	return setLeaf(parent, key, newValue)
}

func (op *PatchOp) applyRemove(v reflect.Value, parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("patch: root removal not supported")
	}

	parent, key, err := navigate(v, parts)
	if err != nil {
		return err
	}

	switch parent.Kind() {
	case reflect.Map:
		parent.SetMapIndex(reflect.ValueOf(key), reflect.Value{})
		return nil
	case reflect.Slice:
		idx, err := strconv.Atoi(key)
		if err != nil {
			return fmt.Errorf("patch: slice index must be integer: %s", key)
		}
		if idx < 0 || idx >= parent.Len() {
			return fmt.Errorf("patch: slice index out of bounds: %d", idx)
		}
		newSlice := reflect.AppendSlice(parent.Slice(0, idx), parent.Slice(idx+1, parent.Len()))
		parent.Set(newSlice)
		return nil
	case reflect.Struct:
		// Struct removal is usually just zeroing out
		field := parent.FieldByName(key)
		if !field.IsValid() {
			// Check JSON tags
			for i := 0; i < parent.Type().NumField(); i++ {
				sf := parent.Type().Field(i)
				tag := strings.Split(sf.Tag.Get("json"), ",")[0]
				if tag == key {
					field = parent.Field(i)
					break
				}
			}
		}
		if field.IsValid() && field.CanSet() {
			field.Set(reflect.Zero(field.Type()))
			return nil
		}
		return fmt.Errorf("patch: struct field %s not found or not settable", key)
	}

	return fmt.Errorf("patch: cannot remove from kind %s", parent.Kind())
}

func (op *PatchOp) applyTest(v reflect.Value, parts []string) error {
	target, err := navigateValue(v, parts)
	if err != nil {
		return fmt.Errorf("patch test failed: path not found: %s", op.Path)
	}

	// Compare current value with op.Value
	currentJSON, err := json.Marshal(target.Interface())
	if err != nil {
		return err
	}

	var v1, v2 any
	if err := json.Unmarshal(currentJSON, &v1); err != nil {
		return err
	}
	if err := json.Unmarshal(op.Value, &v2); err != nil {
		return err
	}

	if !reflect.DeepEqual(v1, v2) {
		return fmt.Errorf("patch test failed: value mismatch at %s", op.Path)
	}
	return nil
}

func (op *PatchOp) applyMove(v reflect.Value, parts []string) error {
	fromParts := strings.Split(strings.TrimPrefix(op.From, "/"), "/")
	fromVal, err := navigateValue(v, fromParts)
	if err != nil {
		return fmt.Errorf("patch move failed: from path not found: %s", op.From)
	}

	// We need a copy of the value to move
	valToMove := reflect.New(fromVal.Type()).Elem()
	valToMove.Set(fromVal)

	// Remove from source
	if err := op.applyRemove(v, fromParts); err != nil {
		return err
	}

	// Add to target
	parent, key, err := navigate(v, parts)
	if err != nil {
		return err
	}
	return setLeaf(parent, key, valToMove)
}

func (op *PatchOp) applyCopy(v reflect.Value, parts []string) error {
	fromParts := strings.Split(strings.TrimPrefix(op.From, "/"), "/")
	fromVal, err := navigateValue(v, fromParts)
	if err != nil {
		return fmt.Errorf("patch copy failed: from path not found: %s", op.From)
	}

	// Add to target
	parent, key, err := navigate(v, parts)
	if err != nil {
		return err
	}
	return setLeaf(parent, key, fromVal)
}

func navigateValue(v reflect.Value, parts []string) (reflect.Value, error) {
	if len(parts) == 0 {
		return v, nil
	}
	parent, key, err := navigate(v, parts)
	if err != nil {
		return reflect.Value{}, err
	}

	if parent.Kind() == reflect.Ptr {
		parent = parent.Elem()
	}

	switch parent.Kind() {
	case reflect.Map:
		mv := parent.MapIndex(reflect.ValueOf(key))
		if !mv.IsValid() {
			return reflect.Value{}, fmt.Errorf("patch: map key %s not found", key)
		}
		return mv, nil
	case reflect.Slice:
		idx, err := strconv.Atoi(key)
		if err != nil {
			return reflect.Value{}, err
		}
		if idx < 0 || idx >= parent.Len() {
			return reflect.Value{}, fmt.Errorf("patch: index %d out of bounds", idx)
		}
		return parent.Index(idx), nil
	case reflect.Struct:
		f := parent.FieldByName(key)
		if !f.IsValid() {
			for i := 0; i < parent.Type().NumField(); i++ {
				sf := parent.Type().Field(i)
				tag := strings.Split(sf.Tag.Get("json"), ",")[0]
				if tag == key || strings.EqualFold(sf.Name, key) || normalize(sf.Name) == normalize(key) {
					f = parent.Field(i)
					break
				}
			}
		}
		if f.IsValid() {
			return f, nil
		}
		return reflect.Value{}, fmt.Errorf("patch: struct field %s not found", key)
	}
	return reflect.Value{}, fmt.Errorf("patch: cannot navigate through kind %s", parent.Kind())
}

func navigate(v reflect.Value, parts []string) (reflect.Value, string, error) {
	curr := v
	for i, part := range parts {
		if curr.Kind() == reflect.Ptr {
			if curr.IsNil() {
				return reflect.Value{}, "", fmt.Errorf("patch: nil pointer at part %s", part)
			}
			curr = curr.Elem()
		}

		if i == len(parts)-1 {
			return curr, part, nil
		}

		switch curr.Kind() {
		case reflect.Map:
			mv := curr.MapIndex(reflect.ValueOf(part))
			if !mv.IsValid() {
				return reflect.Value{}, "", fmt.Errorf("patch: map key %s not found", part)
			}
			curr = mv
		case reflect.Slice:
			idx, err := strconv.Atoi(part)
			if err != nil {
				return reflect.Value{}, "", fmt.Errorf("patch: slice index must be integer: %s", part)
			}
			if idx < 0 || idx >= curr.Len() {
				return reflect.Value{}, "", fmt.Errorf("patch: slice index out of bounds: %d", idx)
			}
			curr = curr.Index(idx)
		case reflect.Struct:
			f := curr.FieldByName(part)
			if !f.IsValid() {
				// Try case-insensitive or JSON tag
				found := false
				for j := 0; j < curr.Type().NumField(); j++ {
					sf := curr.Type().Field(j)
					tag := strings.Split(sf.Tag.Get("json"), ",")[0]
					if tag == part || strings.EqualFold(sf.Name, part) || normalize(sf.Name) == normalize(part) {
						f = curr.Field(j)
						found = true
						break
					}
				}
				if !found {
					return reflect.Value{}, "", fmt.Errorf("patch: struct field %s not found", part)
				}
			}
			curr = f
		default:
			return reflect.Value{}, "", fmt.Errorf("patch: cannot navigate through kind %s", curr.Kind())
		}
	}
	return curr, "", nil
}

func setLeaf(parent reflect.Value, key string, newValue reflect.Value) error {
	if parent.Kind() == reflect.Ptr {
		parent = parent.Elem()
	}

	switch parent.Kind() {
	case reflect.Map:
		parent.SetMapIndex(reflect.ValueOf(key), newValue)
		return nil
	case reflect.Slice:
		idx, err := strconv.Atoi(key)
		if err != nil {
			// Special handling for "-" (RFC 6902: append to end)
			if key == "-" {
				parent.Set(reflect.Append(parent, newValue))
				return nil
			}
			return fmt.Errorf("patch: slice index must be integer: %s", key)
		}
		if idx < 0 || idx > parent.Len() {
			return fmt.Errorf("patch: slice index out of bounds: %d", idx)
		}

		// RFC 6902: Add to slice index means INSERT, not REPLACE
		newSlice := reflect.MakeSlice(parent.Type(), parent.Len()+1, parent.Cap()+1)
		reflect.Copy(newSlice.Slice(0, idx), parent.Slice(0, idx))
		newSlice.Index(idx).Set(newValue)
		reflect.Copy(newSlice.Slice(idx+1, newSlice.Len()), parent.Slice(idx, parent.Len()))
		parent.Set(newSlice)
		return nil
	case reflect.Struct:
		f := parent.FieldByName(key)
		if !f.IsValid() {
			for i := 0; i < parent.Type().NumField(); i++ {
				sf := parent.Type().Field(i)
				tag := strings.Split(sf.Tag.Get("json"), ",")[0]
				if tag == key || strings.EqualFold(sf.Name, key) || normalize(sf.Name) == normalize(key) {
					f = parent.Field(i)
					break
				}
			}
		}
		if f.IsValid() && f.CanSet() {
			f.Set(newValue)
			return nil
		}
		return fmt.Errorf("patch: struct field %s not found or not settable", key)
	}
	return fmt.Errorf("patch: cannot set on kind %s", parent.Kind())
}
