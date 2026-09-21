package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/env"
)

// ExternalScriptProvider executes scripts as sub-processes. Python and JS are
// built in; any other extension can be registered by the developer via
// config.ExternalRuntimes.Runtimes without touching this source file.
type ExternalScriptProvider struct {
	cfg        *config.ProviderConfig
	scriptPath string
	runtime    string // "python" | "node" | developer-registered command
	exePath    string
	extraArgs  []string // leading args before scriptPath, e.g. ["run"] for "deno run"
}

func NewExternalScriptProvider(cfg *config.ProviderConfig, scriptPath string, runtimes config.ExternalRuntimes) (*ExternalScriptProvider, error) {
	ext := strings.ToLower(filepath.Ext(scriptPath))
	var runtime, exePath string
	var extraArgs []string

	switch ext {
	case ".py":
		runtime = "python"
		exePath = runtimes.PythonPath
		if exePath == "" {
			exePath = env.GetPythonCmd() // fallback to detected cmd
		}
	case ".js":
		runtime = "node"
		exePath = runtimes.NodePath
		if exePath == "" {
			exePath = "node" // fallback to PATH
		}
	default:
		// FIX (2026-07-14): the extension-to-runtime mapping used to be a
		// closed switch (.py/.js only), forcing anyone who wanted to write
		// a provider in Ruby, Deno/TS, or anything else to edit this file
		// and recompile. Any extension can now be registered purely via
		// config (config.ExternalRuntimes.Runtimes), e.g.
		// "runtimes": {".rb": ["ruby"], ".ts": ["deno", "run"]} - the stdin/
		// stdout JSON bridge protocol below is already language-agnostic,
		// this was the only closed part of it.
		cmdParts, ok := runtimes.Runtimes[ext]
		if !ok || len(cmdParts) == 0 {
			return nil, fmt.Errorf("unsupported external script extension: %s (register it in config.external_runtimes.runtimes, e.g. {\"%s\": [\"your-interpreter\"]})", ext, ext)
		}
		runtime = cmdParts[0]
		exePath = cmdParts[0]
		if len(cmdParts) > 1 {
			extraArgs = cmdParts[1:]
		}
	}

	return &ExternalScriptProvider{
		cfg:        cfg,
		scriptPath: scriptPath,
		runtime:    runtime,
		exePath:    exePath,
		extraArgs:  extraArgs,
	}, nil
}

func (p *ExternalScriptProvider) Name() string {
	return fmt.Sprintf("external:%s:%s", p.runtime, filepath.Base(p.scriptPath))
}

func (p *ExternalScriptProvider) Complete(ctx context.Context, req CompleteRequest) (*ProviderResponse, error) {
	// Prepare JSON input encompassing both the request and the provider configuration
	// to allow the script to act as a "Gateway" (accessing URLs, Tokens, etc.)
	bridgeInput := map[string]any{
		"request": req,
		"config": map[string]any{
			"base_url": p.cfg.BaseURL,
			"extra":    p.cfg.Extra,
			"model":    p.cfg.Model,
		},
	}

	input, err := json.Marshal(bridgeInput)
	if err != nil {
		return nil, err
	}

	// For external scripts, we use a simple "bridge" protocol:
	// The script is called with the bridgeInput as a JSON string via Stdin.
	args := append(append([]string{}, p.extraArgs...), p.scriptPath)
	cmd := exec.CommandContext(ctx, p.exePath, args...)
	cmd.Stdin = bytes.NewReader(input)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("script error: %w (stderr: %s)", err, stderr.String())
	}

	var resp ProviderResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("invalid script output: %w (output: %s)", err, stdout.String())
	}

	return &resp, nil
}

// StreamComplete is a fallback that delegates to Complete for ExternalScriptProvider.
func (p *ExternalScriptProvider) StreamComplete(ctx context.Context, req CompleteRequest, onChunk func(string) error) (*ProviderResponse, error) {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := onChunk(resp.Text); err != nil {
		return nil, err
	}
	return resp, nil
}

func (p *ExternalScriptProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	return nil, fmt.Errorf("embeddings not supported by ExternalScriptProvider")
}

func (p *ExternalScriptProvider) Reload() error {
	return nil // No-op for external scripts
}
