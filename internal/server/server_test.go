package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/r59q/gitsnow/internal/issues"
)

func testAPI(t *testing.T) http.Handler {
	t.Helper()
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("initialize repository: %v: %s", err, output)
	}
	store, err := issues.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return New(store, fstest.MapFS{"index.html": {Data: []byte("<html>gitsnow</html>")}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func request(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost:7331"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func TestIssueAPI(t *testing.T) {
	handler := testAPI(t)
	response := request(handler, "GET", "/api/issues", "")
	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != `{"issues":[]}` {
		t.Fatalf("unexpected empty list: %d %s", response.Code, response.Body.String())
	}
	response = request(handler, "POST", "/api/issues", `{"title":"Phone workflow","body":"Acceptance criteria"}`)
	if response.Code != 201 {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	var issue issues.Issue
	if err := json.Unmarshal(response.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	if response.Header().Get("Location") != "/api/issues/"+issue.ID {
		t.Fatal("missing resource location")
	}
	path := "/api/issues/" + issue.ID
	if response := request(handler, "GET", path, ""); response.Code != 200 {
		t.Fatalf("get: %d %s", response.Code, response.Body.String())
	}
	body, err := json.Marshal(issues.Input{Title: issue.Title, Body: issue.Body, Status: "closed", Revision: issue.Revision})
	if err != nil {
		t.Fatal(err)
	}
	response = request(handler, "PUT", path, string(body))
	if response.Code != 200 {
		t.Fatalf("update: %d %s", response.Code, response.Body.String())
	}
	var updated issues.Issue
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if response := request(handler, "PUT", path, string(body)); response.Code != 409 {
		t.Fatalf("stale edit: %d %s", response.Code, response.Body.String())
	}
	response = request(handler, "DELETE", path, `{"revision":"`+updated.Revision+`"}`)
	if response.Code != 204 || response.Body.Len() != 0 {
		t.Fatalf("delete: %d %s", response.Code, response.Body.String())
	}
	if response := request(handler, "GET", path, ""); response.Code != 404 {
		t.Fatalf("deleted issue: %d %s", response.Code, response.Body.String())
	}
}

func TestAPIRejectsInvalidRequests(t *testing.T) {
	handler := testAPI(t)
	for _, body := range []string{
		`{`, `{"title":"x","unknown":true}`, `{"title":"x"} {}`, `{"title":""}`, `{"title":"x","status":"invalid"}`,
	} {
		if response := request(handler, "POST", "/api/issues", body); response.Code != 400 {
			t.Errorf("accepted %q: %d %s", body, response.Code, response.Body.String())
		}
	}
	if response := request(handler, "GET", "/api/issues?status=invalid", ""); response.Code != 400 {
		t.Fatalf("accepted invalid filter: %d", response.Code)
	}
	if response := request(handler, "GET", "/api/unknown", ""); response.Code != 404 || !strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("unexpected unknown API response: %d %s", response.Code, response.Body.String())
	}
	if response := request(handler, "POST", "/api/issues", `{"title":"x","body":"`+strings.Repeat("a", 2*1024*1024)+`"}`); response.Code != 413 {
		t.Fatalf("accepted oversized request: %d", response.Code)
	}
	req := httptest.NewRequest("POST", "http://localhost:7331/api/issues", strings.NewReader(`{"title":"x"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 415 {
		t.Fatalf("accepted missing content type: %d", response.Code)
	}
}

func TestLocalhostBoundary(t *testing.T) {
	handler := testAPI(t)
	tests := []struct {
		host, origin, fetchSite string
		status                  int
	}{
		{"localhost:7331", "", "", 200},
		{"127.0.0.1:7331", "http://127.0.0.1:7331", "same-origin", 200},
		{"[::1]:7331", "", "", 200},
		{"attacker.example:7331", "", "", 403},
		{"localhost:7331", "https://attacker.example", "", 403},
		{"localhost:7331", "http://localhost:8000", "", 403},
		{"localhost:7331", "", "cross-site", 403},
		{"localhost:7331", "null", "", 403},
	}
	for _, test := range tests {
		req := httptest.NewRequest("GET", "http://"+test.host+"/api/issues", nil)
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Sec-Fetch-Site", test.fetchSite)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != test.status {
			t.Errorf("host=%q origin=%q: expected %d, got %d", test.host, test.origin, test.status, response.Code)
		}
	}
}

func TestStaticUI(t *testing.T) {
	response := request(testAPI(t), "GET", "/", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "gitsnow") {
		t.Fatalf("UI not served: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing content security policy")
	}
}

func TestRejectsChangedRepository(t *testing.T) {
	handler := testAPI(t)
	req := httptest.NewRequest("POST", "http://localhost:7331/api/issues", strings.NewReader(`{"title":"Wrong repository"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitsnow-Repository", "/a/different/repository")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "repository_changed") {
		t.Fatalf("accepted a stale repository binding: %d %s", response.Code, response.Body.String())
	}
	if response := request(handler, "GET", "/api/issues", ""); strings.TrimSpace(response.Body.String()) != `{"issues":[]}` {
		t.Fatal("rejected request created an issue")
	}
}

func TestSharingStatusAndRetry(t *testing.T) {
	handler := testAPI(t)
	response := request(handler, "GET", "/api/repository", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"state":"local_only"`) {
		t.Fatalf("missing explicit local-only status: %d %s", response.Code, response.Body.String())
	}
	response = request(handler, "POST", "/api/sync", "")
	if response.Code != 202 || !strings.Contains(response.Body.String(), `"state":"local_only"`) {
		t.Fatalf("retry claimed shared without a remote: %d %s", response.Code, response.Body.String())
	}
}
