package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/daybeam/vortex/pkg/safelimits"
)

// RunStdioToHTTP reads JSON-RPC messages from stdin and forwards them to a Master's HTTP endpoint.
// It writes responses back to stdout. All logging goes to stderr.
func RunStdioToHTTP(targetURL string, apiKey string) error {
	fmt.Fprintf(os.Stderr, "[proxy] Forwarding stdio to %s\n", targetURL)

	scanner := bufio.NewScanner(os.Stdin)
	// Increase scanner buffer for large JSON-RPC messages (e.g., tool results with large data)
	const maxCapacity = 10 * 1024 * 1024 // 10MB
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, maxCapacity)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Forward to Master via HTTP POST
		req, err := http.NewRequest("POST", targetURL, bytes.NewReader(line))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[proxy] Error creating request: %v\n", err)
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[proxy] Error forwarding request: %v\n", err)
			continue
		}

		// Read response and write to stdout
		respData, err := readAllAndClose(resp.Body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[proxy] Error reading response: %v\n", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "[proxy] Master returned error status: %d, body: %s\n", resp.StatusCode, string(respData))
			continue
		}

		// Write to stdout (must be followed by newline for some MCP clients)
		os.Stdout.Write(respData)
		os.Stdout.Write([]byte("\n"))
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stdin scanner error: %w", err)
	}

	return nil
}

// readAllAndClose reads all bytes from r and ensures the body is closed,
// even if ReadAll returns an error or panics. This avoids leaking HTTP
// connection bodies in the proxy loop (audit finding M5).
func readAllAndClose(body io.ReadCloser) ([]byte, error) {
	defer body.Close()
	// audit H1: cap to prevent OOM from unbounded proxy responses
	return io.ReadAll(io.LimitReader(body, safelimits.MaxResponseBody))
}
