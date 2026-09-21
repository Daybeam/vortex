package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRunStdioToHTTP(t *testing.T) {
	// 1. Setup a mock Master server
	mockMaster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Errorf("Expected path /mcp, got %s", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "hello") {
			t.Errorf("Expected body to contain 'hello', got %s", string(body))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":"world"}`))
	}))
	defer mockMaster.Close()

	// 2. Setup pipes to simulate stdin/stdout
	oldStdin := os.Stdin
	oldStdout := os.Stdout

	rIn, wIn, _ := os.Pipe()
	rOut, wOut, _ := os.Pipe()

	os.Stdin = rIn
	os.Stdout = wOut

	defer func() {
		os.Stdin = oldStdin
		os.Stdout = oldStdout
	}()

	// 3. Run proxy in a goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- RunStdioToHTTP(mockMaster.URL+"/mcp", "test-key")
	}()

	// 4. Send a message to stdin
	wIn.Write([]byte(`{"id":1,"method":"hello"}` + "\n"))
	wIn.Close() // Close stdin to stop the scanner

	// 5. Read from stdout
	var outBuf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&outBuf, rOut)
		close(done)
	}()

	err := <-errChan
	if err != nil {
		t.Fatalf("Proxy returned error: %v", err)
	}

	wOut.Close()
	<-done

	if !strings.Contains(outBuf.String(), `"world"`) {
		t.Errorf("Expected output to contain 'world', got %s", outBuf.String())
	}
}
