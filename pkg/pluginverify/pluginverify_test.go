package pluginverify

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyScriptIntegrity_EmptyHash_AllowsAlways(t *testing.T) {
	// Empty hash = backward compat, always allows (even if file doesn't exist)
	err := VerifyScriptIntegrity("/nonexistent/path.py", "")
	if err != nil {
		t.Fatalf("empty hash should allow: %v", err)
	}
}

func TestVerifyScriptIntegrity_MatchingHash(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "test.py")
	content := []byte("print('hello')\n")
	if err := os.WriteFile(scriptPath, content, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	hashHex := hex.EncodeToString(hash[:])

	err := VerifyScriptIntegrity(scriptPath, hashHex)
	if err != nil {
		t.Fatalf("matching hash should pass: %v", err)
	}
}

func TestVerifyScriptIntegrity_MismatchedHash(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "test.py")
	if err := os.WriteFile(scriptPath, []byte("print('hello')\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := VerifyScriptIntegrity(scriptPath, "abc123invalidhash")
	if err == nil {
		t.Fatal("mismatched hash should fail")
	}
}

func TestVerifyScriptIntegrity_MissingFile(t *testing.T) {
	err := VerifyScriptIntegrity("/nonexistent/path.py", "somehash")
	if err == nil {
		t.Fatal("missing file with non-empty hash should fail")
	}
}

func TestVerifyScriptIntegrity_TamperedFile(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "test.py")
	original := []byte("print('original')\n")
	if err := os.WriteFile(scriptPath, original, 0644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(original)
	hashHex := hex.EncodeToString(hash[:])

	// Tamper the file
	if err := os.WriteFile(scriptPath, []byte("print('tampered')\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := VerifyScriptIntegrity(scriptPath, hashHex)
	if err == nil {
		t.Fatal("tampered file should fail integrity check")
	}
}

func TestFilterEnvVars_EmptyAllowed_ReturnsNil(t *testing.T) {
	result := FilterEnvVars([]string{"PATH=/usr/bin", "HOME=/root"}, nil)
	if result != nil {
		t.Fatalf("empty allowed should return nil, got %v", result)
	}
}

func TestFilterEnvVars_Subset(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/root", "API_KEY=secret", "DEBUG=1"}
	allowed := []string{"PATH", "DEBUG"}
	result := FilterEnvVars(env, allowed)
	if len(result) != 2 {
		t.Fatalf("expected 2 vars, got %d: %v", len(result), result)
	}
	// Should contain PATH and DEBUG but not API_KEY
	hasPath, hasDebug, hasAPIKey := false, false, false
	for _, kv := range result {
		switch kv {
		case "PATH=/usr/bin":
			hasPath = true
		case "DEBUG=1":
			hasDebug = true
		case "API_KEY=secret":
			hasAPIKey = true
		}
	}
	if !hasPath || !hasDebug {
		t.Errorf("expected PATH and DEBUG in result: %v", result)
	}
	if hasAPIKey {
		t.Error("API_KEY should be filtered out")
	}
}

func TestFilterEnvVars_NoMatch(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/root"}
	allowed := []string{"NONEXISTENT"}
	result := FilterEnvVars(env, allowed)
	if len(result) != 0 {
		t.Fatalf("expected 0 vars, got %d: %v", len(result), result)
	}
}

func TestIsWriteTool_WriteTools(t *testing.T) {
	writeTools := []string{
		"create_user", "update_record", "delete_item", "execute_query",
		"set_config", "put_data", "post_message", "write_file",
		"insert_row", "remove_cache", "drop_table", "alter_schema",
		"deploy_app", "start_service", "stop_service", "kill_process",
		"submit_form", "send_email", "commit_change", "push_code",
	}
	for _, tool := range writeTools {
		if !IsWriteTool(tool) {
			t.Errorf("expected %q to be a write tool", tool)
		}
	}
}

func TestIsWriteTool_ReadTools(t *testing.T) {
	readTools := []string{
		"list_users", "get_record", "search_items", "read_file",
		"query_data", "fetch_config", "describe_table", "show_status",
		"view_logs", "check_health", "inspect", "stats",
	}
	for _, tool := range readTools {
		if IsWriteTool(tool) {
			t.Errorf("expected %q to NOT be a write tool", tool)
		}
	}
}

func TestIsWriteTool_CaseInsensitive(t *testing.T) {
	if !IsWriteTool("CREATE_USER") {
		t.Error("CREATE_USER should be a write tool (case-insensitive)")
	}
	if !IsWriteTool("DeleteItem") {
		t.Error("DeleteItem should be a write tool (case-insensitive)")
	}
}

func TestVerifyScriptSignature_EmptyPubKey_AllowsAlways(t *testing.T) {
	// Empty public key = backward compat, always allows
	err := VerifyScriptSignature("/nonexistent/script.py", "/nonexistent/script.sig", "")
	if err != nil {
		t.Fatalf("empty pub key should allow: %v", err)
	}
}

func TestVerifyScriptSignature_ValidSignature(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "plugin.py")
	sigPath := filepath.Join(dir, "plugin.sig")

	scriptContent := []byte("def run(ctx): pass\n")
	if err := os.WriteFile(scriptPath, scriptContent, 0644); err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, scriptContent)
	if err := os.WriteFile(sigPath, sig, 0644); err != nil {
		t.Fatal(err)
	}
	pubKeyB64 := base64.StdEncoding.EncodeToString(pub)

	if err := VerifyScriptSignature(scriptPath, sigPath, pubKeyB64); err != nil {
		t.Fatalf("valid signature should pass: %v", err)
	}
}

func TestVerifyScriptSignature_TamperedScript(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "plugin.py")
	sigPath := filepath.Join(dir, "plugin.sig")

	original := []byte("def run(ctx): pass\n")
	if err := os.WriteFile(scriptPath, original, 0644); err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, original)
	if err := os.WriteFile(sigPath, sig, 0644); err != nil {
		t.Fatal(err)
	}
	pubKeyB64 := base64.StdEncoding.EncodeToString(pub)

	// Tamper the script after signing
	if err := os.WriteFile(scriptPath, []byte("def run(ctx): EVIL\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err = VerifyScriptSignature(scriptPath, sigPath, pubKeyB64)
	if err == nil {
		t.Fatal("tampered script should fail signature verification")
	}
}

func TestVerifyScriptSignature_MissingSigFile(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "plugin.py")
	if err := os.WriteFile(scriptPath, []byte("print('hi')\n"), 0644); err != nil {
		t.Fatal(err)
	}

	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pubKeyB64 := base64.StdEncoding.EncodeToString(pub)

	err = VerifyScriptSignature(scriptPath, filepath.Join(dir, "nonexistent.sig"), pubKeyB64)
	if err == nil {
		t.Fatal("missing signature file should fail")
	}
}

func TestVerifyScriptSignature_InvalidPubKeyEncoding(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "plugin.py")
	if err := os.WriteFile(scriptPath, []byte("print('hi')\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err := VerifyScriptSignature(scriptPath, "sig", "!!!not-valid-base64!!!")
	if err == nil {
		t.Fatal("invalid base64 public key should fail")
	}
}

func TestVerifyScriptSignature_WrongPubKeySize(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "plugin.py")
	if err := os.WriteFile(scriptPath, []byte("print('hi')\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 16 bytes instead of 32
	shortKey := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	err := VerifyScriptSignature(scriptPath, "sig", shortKey)
	if err == nil {
		t.Fatal("wrong-size public key should fail")
	}
}
