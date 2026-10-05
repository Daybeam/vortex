package config

import (
	"bytes"
	"log"
	"testing"
)

// captureLog redirects log output to a buffer for the duration of the test.
func captureLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	return &buf, func() { log.SetOutput(orig) }
}

func TestWarnInvalidSystemKeys_UnknownKey(t *testing.T) {
	buf, restore := captureLog(t)
	defer restore()

	data := []byte(`{"system":{"max_context_window":16384,"confidence_threshold":0.7}}`)
	warnInvalidSystemKeys(data)

	output := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("system.max_context_window")) {
		t.Errorf("expected warning about system.max_context_window, got: %s", output)
	}
	if !bytes.Contains(buf.Bytes(), []byte("not a valid key")) {
		t.Errorf("expected 'not a valid key' in warning, got: %s", output)
	}
}

func TestWarnInvalidSystemKeys_AllValidKeysNoWarning(t *testing.T) {
	buf, restore := captureLog(t)
	defer restore()

	// All keys here are valid SystemSettings fields
	data := []byte(`{"system":{"confidence_threshold":0.7,"max_context_keep":100,"max_tool_turns":30,"non_interactive":true,"delegation_mode":false}}`)
	warnInvalidSystemKeys(data)

	if buf.Len() > 0 {
		t.Errorf("expected no warning for valid config, got: %s", buf.String())
	}
}

func TestWarnInvalidSystemKeys_MultipleUnknownKeys(t *testing.T) {
	buf, restore := captureLog(t)
	defer restore()

	data := []byte(`{"system":{"max_context_window":16384,"foo_bar":"x","another_bad":123}}`)
	warnInvalidSystemKeys(data)

	output := buf.String()
	// All three unknown keys should be warned about
	for _, key := range []string{"max_context_window", "foo_bar", "another_bad"} {
		if !bytes.Contains(buf.Bytes(), []byte("system."+key)) {
			t.Errorf("expected warning about system.%s, got: %s", key, output)
		}
	}
}

func TestWarnInvalidSystemKeys_NoSystemSection(t *testing.T) {
	buf, restore := captureLog(t)
	defer restore()

	data := []byte(`{"providers":{}}`)
	warnInvalidSystemKeys(data)

	if buf.Len() > 0 {
		t.Errorf("expected no warning when system section absent, got: %s", buf.String())
	}
}

func TestWarnInvalidSystemKeys_InvalidJSON(t *testing.T) {
	buf, restore := captureLog(t)
	defer restore()

	// Invalid JSON — should silently return, not panic
	warnInvalidSystemKeys([]byte(`{invalid json`))

	if buf.Len() > 0 {
		t.Errorf("expected no output for invalid JSON, got: %s", buf.String())
	}
}

func TestValidSystemKeys_ContainsExpectedFields(t *testing.T) {
	// Verify that the reflection-based key set includes known fields
	expected := []string{
		"confidence_threshold", "max_context_keep", "non_interactive",
		"delegation_mode", "max_concurrent_steps", "systemone",
		"tool_repetition_threshold", "sandbox", "telemetry",
	}
	for _, key := range expected {
		if !validSystemKeys[key] {
			t.Errorf("validSystemKeys missing expected key: %s", key)
		}
	}
}

func TestValidSystemKeys_ExcludesNonExistentFields(t *testing.T) {
	// Verify that common traps are NOT in the valid set
	nonExistent := []string{
		"max_context_window", "context_window", "max_tokens",
		"model_context_window", "ctx_size",
	}
	for _, key := range nonExistent {
		if validSystemKeys[key] {
			t.Errorf("validSystemKeys should NOT contain: %s", key)
		}
	}
}
