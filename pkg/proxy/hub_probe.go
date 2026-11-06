package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/daybeam/vortex/pkg/safelimits"
)

// LoopbackURL builds the probe base URL for a loopback port read from
// configuration (VORTEX_PORT, which .env can set to anything).
//
// O18 (2026-11-04): the desktop launcher used to concatenate this string
// straight into "http://127.0.0.1:" + port. A value shaped like
// "x@host.example:8000" turns the result into the userinfo form of
// http://host.example:8000, so the probe — carrying the admin key in
// Authorization — leaves the machine. Input is operator-controlled, so that is a
// footgun rather than a remote hole, but the port has exactly one legal shape, so
// it is checked here instead of trusted downstream.
func LoopbackURL(port string) (string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q is not a TCP port number", port)
	}
	return "http://127.0.0.1:" + strconv.Itoa(n), nil
}

// HubAnswersAPI reports whether the process already listening at baseURL serves
// the hub API surface with apiKey, and returns the reason either way so callers
// can log why they chose differently.
//
// It exists for the second-launch decision in the desktop edition (register
// docs/audit/unwired-code-register.md section O13): a launch that finds the
// configured port occupied has to tell "another hub with this same config" from
// "some unrelated process on the port", because the two need opposite actions.
// Only a 200 from /api/config proves the occupant answers the exact endpoint the
// frontend's backendReady gate polls, using the exact key the launcher is about
// to inject. A bare MCP server returns 404 for that path, and an apikey-mode
// transport or a mismatched key returns 401 — both are reported as not
// attachable, which keeps the previous fallback in place for them.
func HubAnswersAPI(client *http.Client, baseURL, apiKey string) (bool, string) {
	url := strings.TrimRight(baseURL, "/") + "/api/config"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Sprintf("probe URL %q invalid: %v", url, err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// O18 (2026-11-04): never follow redirects, and do it here rather than
	// relying on the caller's client. The verdict is a status code, so a 302 to
	// anything that answers 200 would otherwise be reported as "a hub that
	// accepts this key" — and an occupant that redirects is not a hub, it is a
	// different program. ErrUseLastResponse makes the 3xx itself the response the
	// check below reads. The clone keeps this function's guarantee independent of
	// how the caller configured its client.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return false, fmt.Sprintf("GET %s failed: %v", url, err)
	}
	defer resp.Body.Close()
	// Read and discard a bounded amount of the body: leaving it unread keeps the
	// connection out of the pool. Only the size is logged, never the content —
	// /api/config carries role and provider configuration.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxResponseBody))
	if resp.StatusCode == http.StatusOK {
		return true, fmt.Sprintf("GET %s -> 200 (%d bytes)", url, len(body))
	}
	return false, fmt.Sprintf("GET %s -> HTTP %d", url, resp.StatusCode)
}
