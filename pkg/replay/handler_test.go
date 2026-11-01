package replay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	multiStepFixture(t, root)
	mux := http.NewServeMux()
	NewHandler(filepath.Join(root, "logs"), filepath.Join(root, "tasks")).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, root
}

func TestRunsHandlerListsRuns(t *testing.T) {
	server, _ := newTestServer(t)
	response, err := http.Get(server.URL + "/api/replay/runs?limit=5")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var index Index
	if err := json.NewDecoder(response.Body).Decode(&index); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(index.Runs) != 1 || index.Runs[0].TaskID != "task_aaaa1111" {
		t.Fatalf("runs = %+v, want task_aaaa1111", index.Runs)
	}
}

func TestRunHandlerReturnsDetail(t *testing.T) {
	server, _ := newTestServer(t)
	response, err := http.Get(server.URL + "/api/replay/runs/task_aaaa1111")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	var detail RunDetail
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.TaskID != "task_aaaa1111" || len(detail.Events) == 0 || len(detail.Steps) != 2 {
		t.Errorf("detail = %+v, want full run", detail.RunSummary)
	}
}

func TestRunHandlerRejectsBadRequests(t *testing.T) {
	server, _ := newTestServer(t)
	cases := []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/api/replay/runs/task_zzzz9999", http.StatusNotFound},
		{http.MethodGet, "/api/replay/runs/not-a-task", http.StatusBadRequest},
		{http.MethodGet, "/api/replay/runs/task_aaaa1111/extra", http.StatusBadRequest},
		{http.MethodGet, "/api/replay/runs/", http.StatusBadRequest},
		{http.MethodPost, "/api/replay/runs", http.StatusMethodNotAllowed},
	}
	for _, testCase := range cases {
		request, err := http.NewRequest(testCase.method, server.URL+testCase.path, nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", testCase.method, testCase.path, err)
		}
		response.Body.Close()
		if response.StatusCode != testCase.status {
			t.Errorf("%s %s status = %d, want %d", testCase.method, testCase.path, response.StatusCode, testCase.status)
		}
	}
}
