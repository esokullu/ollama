//go:build windows || darwin

package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama/ollama/app/store"
)

func TestNormalizeNavigationURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare host gets https", "example.com", "https://example.com"},
		{"host and path", "example.com/pricing", "https://example.com/pricing"},
		{"https preserved", "https://example.com/a?b=c", "https://example.com/a?b=c"},
		{"http preserved", "http://127.0.0.1:8080/", "http://127.0.0.1:8080/"},
		{"surrounding space trimmed", "  example.com  ", "https://example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeNavigationURL(tc.in)
			if err != nil {
				t.Fatalf("normalizeNavigationURL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The pane is a view onto web pages. Letting it drive the browser's own
// privileged surfaces would turn the address bar into a way to read local
// files or reach chrome:// settings.
func TestNormalizeNavigationURLRejectsPrivilegedSchemes(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"chrome://settings",
		"devtools://devtools/bundled/inspector.html",
		"javascript://alert(1)",
		"data://text/html,hi",
	} {
		if got, err := normalizeNavigationURL(raw); err == nil {
			t.Errorf("normalizeNavigationURL(%q) = %q, want an error", raw, got)
		}
	}
}

func TestNormalizeNavigationURLRejectsEmpty(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		if _, err := normalizeNavigationURL(raw); err == nil {
			t.Errorf("normalizeNavigationURL(%q) succeeded, want an error", raw)
		}
	}
}

// A dev-mode server skips token auth, which keeps these tests to the handler
// logic rather than the cookie plumbing.
func newBrowserTestServer(t *testing.T) *Server {
	t.Helper()

	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	t.Cleanup(func() { testStore.Close() })

	return &Server{Dev: true, Store: testStore}
}

func TestBrowserInputRejectsUnknownEventTypes(t *testing.T) {
	s := newBrowserTestServer(t)

	cases := []string{
		`{"kind":"mouse","type":"mouseTeleported","x":1,"y":1}`,
		`{"kind":"key","type":"keySmashed","key":"a"}`,
		`{"kind":"telepathy","type":"keyDown"}`,
	}

	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/browser/input", strings.NewReader(body))
		w := httptest.NewRecorder()

		if err := s.browserInput(w, req); err == nil {
			t.Errorf("browserInput(%s) accepted an event it should reject", body)
		}
	}
}

// Input on a disconnected pane must fail rather than silently succeed, so the
// UI can tell the user the browser went away.
func TestBrowserInputFailsWhenDisconnected(t *testing.T) {
	s := newBrowserTestServer(t)

	body := `{"kind":"mouse","type":"mousePressed","x":10,"y":20,"button":"left","clickCount":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/browser/input", strings.NewReader(body))
	w := httptest.NewRecorder()

	if err := s.browserInput(w, req); err == nil {
		t.Fatal("browserInput succeeded while disconnected")
	}
}

func TestBrowserStatusReportsDisconnected(t *testing.T) {
	s := newBrowserTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/browser/status", nil)
	w := httptest.NewRecorder()

	if err := s.browserStatus(w, req); err != nil {
		t.Fatalf("browserStatus: %v", err)
	}

	var status struct {
		Connected bool `json:"connected"`
	}
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Connected {
		t.Error("connected = true on a fresh server")
	}
}

// "act" lets WebBrain click, type and submit. Anything the UI did not
// explicitly mark as act must fall back to read-only ask.
func TestBrowserRunTaskDefaultsToAskMode(t *testing.T) {
	s := newBrowserTestServer(t)

	for _, body := range []string{
		`{"task":"summarize"}`,
		`{"task":"summarize","mode":""}`,
		`{"task":"summarize","mode":"ACT"}`,
		`{"task":"summarize","mode":"anything-else"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/browser/task", strings.NewReader(body))
		w := httptest.NewRecorder()

		// WebBrain is not running here, so the call fails at the transport.
		// What matters is that it never reaches the manager as "act", which a
		// mode-parsing bug would show up as. Assert on the failure being the
		// connection, not a mode error.
		err := s.browserRunTask(w, req)
		if err == nil {
			t.Fatalf("browserRunTask(%s) succeeded without WebBrain", body)
		}
		if !strings.Contains(err.Error(), "WebBrain is not connected") {
			t.Errorf("browserRunTask(%s) failed with %v, want the WebBrain connection error", body, err)
		}
	}
}

func TestBrowserRunTaskRequiresATask(t *testing.T) {
	s := newBrowserTestServer(t)

	for _, body := range []string{`{}`, `{"task":"   "}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/browser/task", strings.NewReader(body))
		w := httptest.NewRecorder()

		err := s.browserRunTask(w, req)
		if err == nil || !strings.Contains(err.Error(), "task is required") {
			t.Errorf("browserRunTask(%s) = %v, want a missing-task error", body, err)
		}
	}
}

func TestBrowserTaskControlRequiresIdentifiers(t *testing.T) {
	s := newBrowserTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/browser/task/abort", strings.NewReader(`{}`))
	if err := s.browserAbortTask(httptest.NewRecorder(), req); err == nil {
		t.Error("browserAbortTask succeeded without a runId")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/browser/task/respond", strings.NewReader(`{"runId":"r1"}`))
	if err := s.browserRespondTask(httptest.NewRecorder(), req); err == nil {
		t.Error("browserRespondTask succeeded without a clarifyId")
	}
}

func TestBrowserSelectTabRequiresTabID(t *testing.T) {
	s := newBrowserTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/browser/tab", strings.NewReader(`{}`))
	if err := s.browserSelectTab(httptest.NewRecorder(), req); err == nil {
		t.Error("browserSelectTab succeeded without a tabId")
	}
}

// The routes must be registered, or the pane gets the SPA's index.html back
// and fails with an opaque JSON parse error.
func TestBrowserRoutesAreRegistered(t *testing.T) {
	handler := newBrowserTestServer(t).Handler()

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/browser/status"},
		{http.MethodPost, "/api/v1/browser/connect"},
		{http.MethodPost, "/api/v1/browser/disconnect"},
		{http.MethodGet, "/api/v1/browser/tabs"},
		{http.MethodPost, "/api/v1/browser/tab"},
		{http.MethodPost, "/api/v1/browser/input"},
		{http.MethodPost, "/api/v1/browser/navigate"},
		{http.MethodPost, "/api/v1/browser/task"},
		{http.MethodGet, "/api/v1/browser/task"},
		{http.MethodPost, "/api/v1/browser/task/abort"},
		{http.MethodPost, "/api/v1/browser/task/respond"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s %s fell through to the SPA handler", tc.method, tc.path)
		}
	}
}
