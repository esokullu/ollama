//go:build windows || darwin

package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ollama/ollama/app/browser"
)

// browserManager returns the pane's manager, creating it on first use. The
// manager holds no connections until Connect is called, so building it lazily
// costs nothing and keeps a browserless app from paying for it at all.
func (s *Server) browserManager() *browser.Manager {
	s.browserOnce.Do(func() {
		s.Browser = browser.NewManager()
	})
	return s.Browser
}

type browserConnectRequest struct {
	Port  int    `json:"port"`
	TabID string `json:"tabId"`
}

func (s *Server) browserConnect(w http.ResponseWriter, r *http.Request) error {
	var req browserConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid request body: %w", err)
	}

	if req.Port == 0 {
		settings, err := s.Store.Settings()
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}
		req.Port = settings.BrowserDebugPort
	}

	status, err := s.browserManager().Connect(r.Context(), req.Port, req.TabID)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(status)
}

func (s *Server) browserDisconnect(w http.ResponseWriter, r *http.Request) error {
	s.browserManager().Disconnect()

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(browser.Status{})
}

func (s *Server) browserStatus(w http.ResponseWriter, r *http.Request) error {
	status := s.browserManager().Status(r.Context())

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(status)
}

func (s *Server) browserTabs(w http.ResponseWriter, r *http.Request) error {
	tabs, err := s.browserManager().Tabs(r.Context())
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"tabs": tabs})
}

func (s *Server) browserSelectTab(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		TabID string `json:"tabId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if req.TabID == "" {
		return errors.New("tabId is required")
	}

	target, err := s.browserManager().SelectTab(r.Context(), req.TabID)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(target)
}

// browserStream pushes screencast frames as newline-delimited JSON, matching
// how the chat endpoint streams.
func (s *Server) browserStream(w http.ResponseWriter, r *http.Request) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming not supported")
	}

	w.Header().Set("Content-Type", "text/jsonl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	frames, release := s.browserManager().Subscribe()
	defer release()

	enc := json.NewEncoder(w)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			if err := enc.Encode(frame); err != nil {
				// The viewer went away mid-write; not worth logging as a fault.
				return nil
			}
			flusher.Flush()
		}
	}
}

func (s *Server) browserNavigate(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	target, err := normalizeNavigationURL(req.URL)
	if err != nil {
		return err
	}
	if err := s.browserManager().Navigate(r.Context(), target); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]string{"url": target})
}

// normalizeNavigationURL turns what a user types in the pane's address bar
// into a URL Chrome will accept. Schemes that would let the pane reach the
// browser's own privileged surfaces are refused: the pane is a view onto web
// pages, not a way to drive chrome:// or read local files.
func normalizeNavigationURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}

	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("cannot open %s:// URLs in the browser pane", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("invalid url: missing host")
	}
	return u.String(), nil
}

type browserInputRequest struct {
	Kind string `json:"kind"` // "mouse" or "key"

	// Mouse
	Type       string  `json:"type"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     string  `json:"button"`
	Buttons    int     `json:"buttons"`
	ClickCount int     `json:"clickCount"`
	DeltaX     float64 `json:"deltaX"`
	DeltaY     float64 `json:"deltaY"`

	// Key
	Text                  string `json:"text"`
	UnmodifiedText        string `json:"unmodifiedText"`
	Key                   string `json:"key"`
	Code                  string `json:"code"`
	WindowsVirtualKeyCode int    `json:"windowsVirtualKeyCode"`

	Modifiers int `json:"modifiers"`
}

func (s *Server) browserInput(w http.ResponseWriter, r *http.Request) error {
	var req browserInputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	m := s.browserManager()

	switch req.Kind {
	case "mouse":
		switch req.Type {
		case "mousePressed", "mouseReleased", "mouseMoved", "mouseWheel":
		default:
			return fmt.Errorf("unsupported mouse event %q", req.Type)
		}
		err := m.SendMouse(r.Context(), browser.MouseEvent{
			Type:       req.Type,
			X:          req.X,
			Y:          req.Y,
			Button:     req.Button,
			Buttons:    req.Buttons,
			ClickCount: req.ClickCount,
			DeltaX:     req.DeltaX,
			DeltaY:     req.DeltaY,
			Modifiers:  req.Modifiers,
		})
		if err != nil {
			return err
		}
	case "key":
		switch req.Type {
		case "keyDown", "keyUp", "char", "rawKeyDown":
		default:
			return fmt.Errorf("unsupported key event %q", req.Type)
		}
		err := m.SendKey(r.Context(), browser.KeyEvent{
			Type:                  req.Type,
			Text:                  req.Text,
			UnmodifiedText:        req.UnmodifiedText,
			Key:                   req.Key,
			Code:                  req.Code,
			WindowsVirtualKeyCode: req.WindowsVirtualKeyCode,
			NativeVirtualKeyCode:  req.WindowsVirtualKeyCode,
			Modifiers:             req.Modifiers,
		})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported input kind %q", req.Kind)
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

// browserTaskResponse carries a WebBrain tool's text back to the pane. The
// tools answer in prose meant for a model, so it is passed through untouched
// rather than parsed into fields that do not exist.
type browserTaskResponse struct {
	Text    string `json:"text"`
	IsError bool   `json:"isError"`
}

func (s *Server) browserRunTask(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Task string `json:"task"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if strings.TrimSpace(req.Task) == "" {
		return errors.New("task is required")
	}

	// Anything not explicitly "act" runs read-only. Act mode can click, type
	// and submit, so it is never the fallback for an unrecognised value.
	mode := "ask"
	if req.Mode == "act" {
		mode = "act"
	}

	res, err := s.browserManager().RunTask(r.Context(), req.Task, mode)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(browserTaskResponse{Text: res.Text, IsError: res.IsError})
}

func (s *Server) browserTaskStatus(w http.ResponseWriter, r *http.Request) error {
	res, err := s.browserManager().TaskStatus(r.Context(), r.URL.Query().Get("runId"))
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(browserTaskResponse{Text: res.Text, IsError: res.IsError})
}

func (s *Server) browserAbortTask(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		RunID string `json:"runId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if req.RunID == "" {
		return errors.New("runId is required")
	}

	res, err := s.browserManager().AbortTask(r.Context(), req.RunID)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(browserTaskResponse{Text: res.Text, IsError: res.IsError})
}

func (s *Server) browserRespondTask(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		RunID     string `json:"runId"`
		ClarifyID string `json:"clarifyId"`
		Answer    string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if req.RunID == "" || req.ClarifyID == "" {
		return errors.New("runId and clarifyId are required")
	}

	res, err := s.browserManager().RespondTask(r.Context(), req.RunID, req.ClarifyID, req.Answer)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(browserTaskResponse{Text: res.Text, IsError: res.IsError})
}
