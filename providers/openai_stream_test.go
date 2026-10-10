package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestOpenAIStreamComplete_TextTokens(t *testing.T) {
	var chunks []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		tokens := []string{"Hello", " world", "!"}
		for _, tok := range tokens {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", tok)
			if flusher != nil {
				flusher.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model:    "gpt-4",
		System:   "test",
		User:     "hi",
		MaxTokens: 100,
	}, func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if resp.Text != "Hello world!" {
		t.Errorf("expected text 'Hello world!', got %q", resp.Text)
	}
	if len(chunks) != 3 {
		t.Errorf("expected 3 onChunk calls, got %d", len(chunks))
	}
	if strings.Join(chunks, "") != "Hello world!" {
		t.Errorf("chunk concatenation mismatch: %v", chunks)
	}
}

func TestOpenAIStreamComplete_ToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		events := []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.txt\","}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"content\":\"hi\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			fmt.Fprintln(w, e)
			fmt.Fprintln(w)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model:    "gpt-4",
		System:   "test",
		User:     "write a file",
		MaxTokens: 100,
	}, nil)
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "write_file" {
		t.Errorf("expected tool name 'write_file', got %q", tc.Name)
	}
	if tc.CallID != "call_1" {
		t.Errorf("expected call_id 'call_1', got %q", tc.CallID)
	}
	path, _ := tc.Arguments["path"].(string)
	if path != "a.txt" {
		t.Errorf("expected path 'a.txt', got %v", tc.Arguments["path"])
	}
	content, _ := tc.Arguments["content"].(string)
	if content != "hi" {
		t.Errorf("expected content 'hi', got %v", tc.Arguments["content"])
	}
}

func TestOpenAIStreamComplete_ErrorFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "internal server error")
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "gpt-4", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	_, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model:    "gpt-4",
		System:   "test",
		User:     "hi",
		MaxTokens: 100,
	}, nil)
	if err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestOpenAIStreamComplete_ReasoningContent(t *testing.T) {
	var chunks []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		events := []string{
			`data: {"choices":[{"delta":{"reasoning_content":"Let me think..."}}]}`,
			`data: {"choices":[{"delta":{"reasoning_content":" about the answer."}}]}`,
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" world!"}}]}`,
			`data: [DONE]`,
		}
		for _, e := range events {
			fmt.Fprintln(w, e)
			fmt.Fprintln(w)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	cfg := &config.ProviderConfig{Provider: "openai", Model: "deepseek-r1", BaseURL: server.URL}
	p := &OpenAIProvider{cfg: cfg, name: "openai", client: server.Client()}

	resp, err := p.StreamComplete(context.Background(), CompleteRequest{
		Model:    "deepseek-r1",
		System:   "test",
		User:     "hi",
		MaxTokens: 100,
	}, func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamComplete failed: %v", err)
	}
	// resp.Text must contain ONLY content, NOT reasoning_content
	if resp.Text != "Hello world!" {
		t.Errorf("expected text 'Hello world!', got %q", resp.Text)
	}
	// onChunk must receive ALL 4 chunks (2 reasoning + 2 content)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 onChunk calls, got %d: %v", len(chunks), chunks)
	}
	// First two chunks are reasoning_content
	if chunks[0] != "Let me think..." {
		t.Errorf("chunk[0] expected reasoning, got %q", chunks[0])
	}
	if chunks[1] != " about the answer." {
		t.Errorf("chunk[1] expected reasoning, got %q", chunks[1])
	}
	// Last two chunks are content
	if chunks[2] != "Hello" || chunks[3] != " world!" {
		t.Errorf("content chunks mismatch: %v", chunks[2:])
	}
}
