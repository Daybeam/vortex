package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/pkg/safelimits"
)

// This file pins the discriminator behind a live incident (2026-11-03, register
// docs/audit/unwired-code-register.md section O13): a second
// orchestrator_desktop.exe launch found 127.0.0.1:8000 occupied and started its
// own master engine unconditionally, so two full hubs shared one
// orchestrator_runtime.db (HA design A7: concurrent SQLite writers corrupt data)
// and both polled the same Telegram bot token. The port-busy branch needs to
// attach to an occupant that can actually serve the UI, and only fall back to a
// second engine for occupants that cannot.

type recordingTransport struct {
	status  int
	body    string
	err     error
	headers http.Header
	path    string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.headers = req.Header.Clone()
	r.path = req.URL.Path
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
	}, nil
}

func TestHubAnswersAPI_AttachesOnlyToAHubThatAcceptsOurKey(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		want       bool
		wantReason string
	}{
		{"hub with matching key", http.StatusOK, true, "-> 200"},
		// The occupant is the stdio/SSE MCP server: /health answers but /api/*
		// does not exist, so its 404 must not be read as a hub.
		{"bare MCP server on the port", http.StatusNotFound, false, "HTTP 404"},
		// A hub whose admin key differs from ours, or an apikey-mode transport.
		// Attaching would hand the UI a 401 on every call, so this is not
		// attachable either -- and the reason has to name the status.
		{"key rejected", http.StatusUnauthorized, false, "HTTP 401"},
		{"hub still booting", http.StatusServiceUnavailable, false, "HTTP 503"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &recordingTransport{status: tc.status, body: `{"config":"redacted"}`}
			ok, reason := HubAnswersAPI(&http.Client{Transport: tr}, "http://127.0.0.1:8000", "secret-key")
			if ok != tc.want {
				t.Errorf("attachable = %v, want %v (reason %q)", ok, tc.want, reason)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason %q does not carry %q", reason, tc.wantReason)
			}
		})
	}
}

func TestHubAnswersAPI_SendsInjectedKeyAndNormalisesBaseURL(t *testing.T) {
	tr := &recordingTransport{status: 200, body: "{}"}
	if _, reason := HubAnswersAPI(&http.Client{Transport: tr}, "http://127.0.0.1:8000/", "secret-key"); !strings.Contains(reason, "-> 200") {
		t.Fatalf("reason = %q", reason)
	}
	if tr.path != "/api/config" {
		t.Errorf("trailing slash in baseURL produced path %q", tr.path)
	}
	if got := tr.headers.Get("Authorization"); got != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want the bearer form of the key being injected", got)
	}

	tr2 := &recordingTransport{status: 200, body: "{}"}
	HubAnswersAPI(&http.Client{Transport: tr2}, "http://127.0.0.1:8000", "")
	if tr2.headers.Get("Authorization") != "" {
		t.Error("empty key must not send an Authorization header (an empty key means the hub is auth-free)")
	}
}

// A refused port or a hung occupant must resolve to "not attachable" rather
// than panicking or blocking the launch: the desktop app is about to start its
// own engine and a probe failure is a detail, not a fault.
func TestHubAnswersAPI_TransportErrorIsNotAttachable(t *testing.T) {
	tr := &recordingTransport{err: errors.New("dial tcp 127.0.0.1:8000: connection refused")}
	ok, reason := HubAnswersAPI(&http.Client{Transport: tr}, "http://127.0.0.1:8000", "k")
	if ok {
		t.Error("a refused connection reported as attachable")
	}
	if !strings.Contains(reason, "connection refused") {
		t.Errorf("reason %q lost the underlying error", reason)
	}
}

// The reason string is written to stderr on every second launch, so it must not
// echo the body: /api/config returns role, provider and channel configuration.
func TestHubAnswersAPI_ReasonDoesNotLeakResponseBody(t *testing.T) {
	tr := &recordingTransport{status: 200, body: `{"telegram_bot_token":"LEAKED"}`}
	_, reason := HubAnswersAPI(&http.Client{Transport: tr}, "http://127.0.0.1:8000", "k")
	if strings.Contains(reason, "LEAKED") {
		t.Errorf("probe reason leaked the response body: %q", reason)
	}
	if !strings.Contains(reason, "bytes") {
		t.Errorf("probe reason should report size only, got %q", reason)
	}
}

// ── O18 (2026-11-04): the mutations the fake-transport table above could not
// kill. Everything below runs against a real httptest server, because a fake
// RoundTripper cannot produce a redirect, cannot overflow the body limit, and
// does not fail if the key moves from the header into the query string.

// A redirect that lands on something answering 200 is not a hub — it is a
// different program, and the verdict must be about the occupant on the port.
// Without this, `== 200` on the post-redirect status would attach the UI to
// whatever the occupant feels like pointing at.
func TestHubAnswersAPI_DoesNotFollowRedirects(t *testing.T) {
	var realHubCalls int
	realHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		realHubCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":"ok"}`))
	}))
	defer realHub.Close()

	occupant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/config" {
			http.Redirect(w, r, realHub.URL+"/api/config", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer occupant.Close()

	ok, reason := HubAnswersAPI(&http.Client{Timeout: 2 * time.Second}, occupant.URL, "secret-key")
	if ok {
		t.Fatalf("a 302 to a 200 was reported as attachable (O18): %q", reason)
	}
	if !strings.Contains(reason, "HTTP 302") {
		t.Errorf("reason %q does not name the status the occupant actually returned", reason)
	}
	if realHubCalls != 0 {
		t.Errorf("probe followed the redirect out to %q (%d calls); the admin key's destination must be the loopback port", realHub.URL, realHubCalls)
	}
}

// The key must travel as a header only: a query-string form lands it in access
// logs, browser history and any Referer the occupant emits.
func TestHubAnswersAPI_KeepsTheKeyOutOfTheURL(t *testing.T) {
	var gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, reason := HubAnswersAPI(&http.Client{}, srv.URL, "secret-key"); !strings.Contains(reason, "-> 200") {
		t.Fatalf("reason = %q", reason)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want the bearer form", gotAuth)
	}
	if strings.Contains(gotQuery, "secret-key") {
		t.Errorf("the key reached the URL as a query parameter: %q", gotQuery)
	}
}

// Pins the LimitReader: the fake-transport table used 2-byte bodies, so dropping
// the bound survived. A hostile or misconfigured occupant that streams an
// unbounded body must not be able to make the launcher allocate without limit.
func TestHubAnswersAPI_ReadsBoundedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Stream more than the limit in 1 MiB chunks so the test does not have to
		// hold the whole oversized body in memory.
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < safelimits.MaxResponseBody/(1<<20)+2; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ok, reason := HubAnswersAPI(&http.Client{Timeout: 10 * time.Second}, srv.URL, "")
	if !ok {
		t.Fatalf("a 200 must still be attachable, reason %q", reason)
	}
	var url string
	var n int
	if _, err := fmt.Sscanf(reason, "GET %s -> 200 (%d bytes)", &url, &n); err != nil {
		t.Fatalf("reason %q has no parsable size: %v", reason, err)
	}
	if n > safelimits.MaxResponseBody {
		t.Errorf("read %d bytes, more than the %d limit", n, safelimits.MaxResponseBody)
	}
}

func TestLoopbackURL_RejectsNonNumericPorts(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"8000", "http://127.0.0.1:8000", false},
		{" 8000 ", "http://127.0.0.1:8000", false},
		{"1", "http://127.0.0.1:1", false},
		{"65535", "http://127.0.0.1:65535", false},
		// The O18 shape: userinfo syntax, parses as another host.
		{"x@host.example:8000", "", true},
		{"8000/../evil", "", true},
		{"0", "", true},
		{"65536", "", true},
		{"", "", true},
		{"http://127.0.0.1:8000", "", true},
	}
	for _, tc := range cases {
		got, err := LoopbackURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("LoopbackURL(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("LoopbackURL(%q) failed: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("LoopbackURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
