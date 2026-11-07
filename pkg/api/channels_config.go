package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Channels config endpoints (TELEGRAM_UI_CONFIG plan, 2026-10-25).
//
// Why .env + restart instead of hot-reload: TELEGRAM_BOT_TOKEN is read once at
// startup by RegisterInputChannels, and TelegramChannel captures the token for
// the lifetime of its long-poll goroutine. The registry's .env watcher reloads
// the env var in-process, but the running goroutine keeps polling with the old
// token — so a restart is genuinely required. The API says so explicitly
// (restart_required: true) rather than faking liveness.

const telegramTokenEnvKey = "TELEGRAM_BOT_TOKEN"

// envFilePath resolves the .env location the same way the main packages do:
// the executable's directory when it looks like an install root (config.json
// present), otherwise the working directory. Kept as a helper (not a field)
// so every build variant shares one resolution rule.
func envFilePath() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			return filepath.Join(dir, ".env")
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, ".env")
	}
	return ".env"
}

// channelsConfigResponse is the GET /api/config/channels payload.
type channelsConfigResponse struct {
	Telegram channelStatus `json:"telegram"`
}

type channelStatus struct {
	Enabled     bool   `json:"enabled"`      // a token is configured
	BotUsername string `json:"bot_username"` // from getMe; empty when unverified
	Error       string `json:"error"`        // getMe failed (token rejected / unreachable)
}

// ChannelsConfigHandler handles GET /api/config/channels — reports which chat
// input channels are configured. The token itself is never returned.
func (h *APIHandler) ChannelsConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := channelStatus{}
	if token := os.Getenv(telegramTokenEnvKey); token != "" {
		status.Enabled = true
		// Verify the token against getMe so the UI can distinguish
		// "configured" from "configured and actually reachable".
		client := &http.Client{Timeout: 5 * time.Second}
		var me struct {
			OK     bool `json:"ok"`
			Result struct {
				Username string `json:"username"`
			} `json:"result"`
		}
		if err := tgCall(token, client, "getMe", nil, &me); err != nil {
			status.Error = fmt.Sprintf("telegram unreachable: %v", err)
		} else if !me.OK {
			status.Error = "token rejected by telegram"
		} else {
			status.BotUsername = me.Result.Username
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(channelsConfigResponse{Telegram: status})
}

// UpdateChannelsHandler handles POST /api/config/channels — persists a channel
// token to .env. Response carries restart_required: true because the channel
// goroutines bind their tokens at startup (see file comment).
func (h *APIHandler) UpdateChannelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TelegramBotToken string `json:"telegram_bot_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(req.TelegramBotToken)
	if token == "" {
		http.Error(w, "telegram_bot_token is required", http.StatusBadRequest)
		return
	}
	// A token is a single .env line — anything with a line break would
	// corrupt the file (and smuggle extra keys).
	if strings.ContainsAny(token, "\r\n") {
		http.Error(w, "telegram_bot_token must not contain line breaks", http.StatusBadRequest)
		return
	}

	if err := writeEnvKey(envFilePath(), telegramTokenEnvKey, token); err != nil {
		http.Error(w, fmt.Sprintf("failed to write .env: %v", err), http.StatusInternalServerError)
		return
	}
	// Apply in-process too, so a subsequent GET reflects the saved token
	// without waiting for the .env watcher (the channel itself still needs
	// a restart — that is what restart_required reports).
	os.Setenv(telegramTokenEnvKey, token)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":           "saved",
		"restart_required": true,
	})
}

// writeEnvKey sets KEY=VALUE in a .env file, replacing an existing line or
// appending a new one. The output stays loadDotEnv-compatible (one KEY=VALUE
// per line, no quoting needed — the loader trims quotes itself). Writes go
// through a temp file + rename so a crash never leaves a half-written .env.
func writeEnvKey(path, key, value string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Normalize CRLF, then split. A trailing newline produces one empty
	// element — drop it so the re-joined file gains no blank line.
	lines := strings.Split(strings.ReplaceAll(string(existing), "\r\n", "\n"), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	replaced := false
	kept := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if k, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == key {
				if !replaced {
					kept = append(kept, key+"="+value)
					replaced = true
				}
				continue // drop duplicate/old occurrences of the key
			}
		}
		kept = append(kept, line)
	}
	if !replaced {
		kept = append(kept, key+"="+value)
	}

	content := strings.Join(kept, "\n") + "\n"

	// Preserve the file's mode when it exists (secrets stay owner-only on
	// first write: 0600).
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
