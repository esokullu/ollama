package browser

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChrome serves the /json discovery endpoints and upgrades
// /devtools/page/{id} to a WebSocket that answers CDP commands.
type fakeChrome struct {
	srv *httptest.Server

	mu      sync.Mutex
	targets []Target
	conns   []*wsTestServer

	// handle answers a command; returning nil result sends `{}`.
	handle func(method string, params json.RawMessage) (any, *cdpError)
}

func newFakeChrome(t *testing.T) *fakeChrome {
	t.Helper()

	f := &fakeChrome{}
	mux := http.NewServeMux()

	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"Browser": "Chrome/141.0.0.0"})
	})

	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.targets)
	})

	mux.HandleFunc("/devtools/page/", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}

		key := r.Header.Get("Sec-WebSocket-Key")
		sum := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(sum[:])
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+accept+"\r\n\r\n")

		// Already handshaken here, so the readiness gate starts open.
		ready := make(chan struct{})
		close(ready)
		peer := &wsTestServer{conn: conn, br: brw.Reader, ready: ready}

		f.mu.Lock()
		f.conns = append(f.conns, peer)
		f.mu.Unlock()

		f.serve(peer)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	// One tab by default; the endpoint URL is only known after start.
	base := strings.TrimPrefix(f.srv.URL, "http://")
	f.targets = []Target{{
		ID:    "TAB1",
		Type:  "page",
		Title: "Example",
		URL:   "https://example.com/",
		WSURL: "ws://" + base + "/devtools/page/TAB1",
	}}
	return f
}

// serve reads commands and replies until the socket closes.
func (f *fakeChrome) serve(peer *wsTestServer) {
	for {
		opcode, data, err := peer.readFrame()
		if err != nil {
			return
		}
		if opcode == opClose {
			return
		}
		if opcode != opText {
			continue
		}

		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			continue
		}

		var (
			result any = map[string]any{}
			cerr   *cdpError
		)
		if f.handle != nil {
			if r, e := f.handle(req.Method, req.Params); e != nil {
				cerr = e
			} else if r != nil {
				result = r
			}
		}

		reply := map[string]any{"id": req.ID}
		if cerr != nil {
			reply["error"] = cerr
		} else {
			reply["result"] = result
		}
		payload, _ := json.Marshal(reply)
		if err := peer.writeFrame(true, opText, payload); err != nil {
			return
		}
	}
}

// emit pushes an unsolicited event to the first connected client.
func (f *fakeChrome) emit(t *testing.T, method string, params any) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.conns) == 0 {
		t.Fatal("no client connected")
	}
	payload, _ := json.Marshal(map[string]any{"method": method, "params": params})
	if err := f.conns[len(f.conns)-1].writeFrame(true, opText, payload); err != nil {
		t.Fatalf("emit: %v", err)
	}
}

func (f *fakeChrome) endpoint() string { return f.srv.URL }

func TestCDPVersionAndTargets(t *testing.T) {
	f := newFakeChrome(t)
	c := NewCDP(f.endpoint())
	defer c.Close()

	ctx := context.Background()

	version, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !strings.HasPrefix(version, "Chrome/") {
		t.Errorf("Version = %q, want a Chrome build string", version)
	}

	// A service worker target must not be offered as a viewable tab.
	f.mu.Lock()
	f.targets = append(f.targets, Target{ID: "SW1", Type: "service_worker", WSURL: "ws://x/y"})
	f.mu.Unlock()

	targets, err := c.Targets(ctx)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != "TAB1" {
		t.Fatalf("Targets = %+v, want only the page target", targets)
	}
}

func TestCDPCallCorrelatesReplies(t *testing.T) {
	f := newFakeChrome(t)
	f.handle = func(method string, params json.RawMessage) (any, *cdpError) {
		if method == "Page.navigate" {
			var p struct {
				URL string `json:"url"`
			}
			json.Unmarshal(params, &p)
			return map[string]any{"frameId": "F1", "echo": p.URL}, nil
		}
		return nil, nil
	}

	c := NewCDP(f.endpoint())
	defer c.Close()

	ctx := context.Background()
	if _, err := c.Attach(ctx, ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Concurrent calls must each get their own reply, not each other's.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			url := fmt.Sprintf("https://example.com/%d", i)
			res, err := c.Call(ctx, "Page.navigate", map[string]any{"url": url})
			if err != nil {
				t.Errorf("Call: %v", err)
				return
			}
			var out struct {
				Echo string `json:"echo"`
			}
			json.Unmarshal(res, &out)
			if out.Echo != url {
				t.Errorf("reply carried %q, want %q", out.Echo, url)
			}
		}()
	}
	wg.Wait()
}

func TestCDPCallSurfacesProtocolError(t *testing.T) {
	f := newFakeChrome(t)
	f.handle = func(method string, params json.RawMessage) (any, *cdpError) {
		return nil, &cdpError{Code: -32000, Message: "Cannot navigate to invalid URL"}
	}

	c := NewCDP(f.endpoint())
	defer c.Close()

	ctx := context.Background()
	if _, err := c.Attach(ctx, ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	_, err := c.Call(ctx, "Page.navigate", map[string]any{"url": "not-a-url"})
	if err == nil {
		t.Fatal("Call succeeded on a protocol error")
	}
	if !strings.Contains(err.Error(), "Cannot navigate to invalid URL") {
		t.Errorf("error = %v, want it to carry Chrome's message", err)
	}
}

func TestCDPDeliversScreencastFrames(t *testing.T) {
	f := newFakeChrome(t)
	c := NewCDP(f.endpoint())
	defer c.Close()

	frames := make(chan ScreencastFrame, 4)
	c.OnEvent(func(method string, params json.RawMessage) {
		if method != "Page.screencastFrame" {
			return
		}
		var fr ScreencastFrame
		if err := json.Unmarshal(params, &fr); err != nil {
			return
		}
		frames <- fr
	})

	ctx := context.Background()
	if _, err := c.Attach(ctx, ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := c.StartScreencast(ctx, 1280, 800, 60); err != nil {
		t.Fatalf("StartScreencast: %v", err)
	}

	f.emit(t, "Page.screencastFrame", map[string]any{
		"data":      "/9j/fakejpeg",
		"sessionId": 7,
		"metadata": map[string]any{
			"deviceWidth":     1280.0,
			"deviceHeight":    800.0,
			"pageScaleFactor": 1.0,
			"scrollOffsetY":   120.0,
		},
	})

	select {
	case fr := <-frames:
		if fr.Data != "/9j/fakejpeg" {
			t.Errorf("frame data = %q", fr.Data)
		}
		if fr.SessionID != 7 {
			t.Errorf("sessionId = %d, want 7", fr.SessionID)
		}
		if fr.Metadata.DeviceWidth != 1280 || fr.Metadata.ScrollOffsetY != 120 {
			t.Errorf("metadata not decoded: %+v", fr.Metadata)
		}
		// Acking is what keeps Chrome sending; it must not error.
		if err := c.AckFrame(ctx, fr.SessionID); err != nil {
			t.Errorf("AckFrame: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no screencast frame delivered")
	}
}

func TestCDPCallFailsWhenNotAttached(t *testing.T) {
	f := newFakeChrome(t)
	c := NewCDP(f.endpoint())
	defer c.Close()

	if _, err := c.Call(context.Background(), "Page.navigate", nil); err == nil {
		t.Fatal("Call succeeded without an attached tab")
	}
}

func TestCDPAttachRejectsUnknownTarget(t *testing.T) {
	f := newFakeChrome(t)
	c := NewCDP(f.endpoint())
	defer c.Close()

	_, err := c.Attach(context.Background(), "NOPE")
	if err == nil {
		t.Fatal("Attach succeeded for a tab that is not open")
	}
	if !strings.Contains(err.Error(), "no longer open") {
		t.Errorf("error = %v, want it to say the tab is gone", err)
	}
}

func TestCDPDoneFiresWhenBrowserDrops(t *testing.T) {
	f := newFakeChrome(t)
	c := NewCDP(f.endpoint())
	defer c.Close()

	ctx := context.Background()
	if _, err := c.Attach(ctx, ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Simulate the user quitting Chrome.
	f.mu.Lock()
	for _, peer := range f.conns {
		peer.conn.Close()
	}
	f.mu.Unlock()

	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never fired after the browser went away")
	}
}

// Chrome rejects a mouseWheel that carries only one delta, and a purely
// vertical scroll has deltaX of exactly zero -- the value omitempty drops.
func TestMouseEventAlwaysCarriesBothDeltas(t *testing.T) {
	payload, err := json.Marshal(MouseEvent{Type: "mouseWheel", X: 10, Y: 20, DeltaY: 120})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := got["deltaX"]; !ok {
		t.Error("deltaX was omitted; Chrome rejects a wheel event without it")
	}
	if _, ok := got["deltaY"]; !ok {
		t.Error("deltaY was omitted")
	}
	if got["deltaY"] != float64(120) {
		t.Errorf("deltaY = %v, want 120", got["deltaY"])
	}
}

// The socket stays bound to one target across navigations, so the title and
// URL captured at attach time would otherwise never update.
func TestCDPRefreshTargetPicksUpNavigation(t *testing.T) {
	f := newFakeChrome(t)
	c := NewCDP(f.endpoint())
	defer c.Close()

	ctx := context.Background()
	if _, err := c.Attach(ctx, ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := c.Target().URL; got != "https://example.com/" {
		t.Fatalf("initial url = %q", got)
	}

	// The page navigates underneath us.
	f.mu.Lock()
	f.targets[0].URL = "https://example.org/pricing"
	f.targets[0].Title = "Pricing"
	f.mu.Unlock()

	got := c.RefreshTarget(ctx)
	if got.URL != "https://example.org/pricing" || got.Title != "Pricing" {
		t.Errorf("RefreshTarget = %q / %q, want the navigated page", got.Title, got.URL)
	}
	if cached := c.Target(); cached.URL != "https://example.org/pricing" {
		t.Errorf("cached target was not updated: %q", cached.URL)
	}
}

func TestCDPRefreshTargetKeepsCurrentWhenTabIsGone(t *testing.T) {
	f := newFakeChrome(t)
	c := NewCDP(f.endpoint())
	defer c.Close()

	ctx := context.Background()
	if _, err := c.Attach(ctx, ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	f.mu.Lock()
	f.targets = nil
	f.mu.Unlock()

	if got := c.RefreshTarget(ctx); got.ID != "TAB1" {
		t.Errorf("RefreshTarget = %+v, want the last known target", got)
	}
}
