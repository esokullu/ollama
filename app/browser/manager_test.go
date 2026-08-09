package browser

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestManager wires a manager to a fake Chrome, with webbrain-mcp pointed
// at the in-binary fake so no network or npx is involved.
func newTestManager(t *testing.T) (*Manager, *fakeChrome) {
	t.Helper()

	f := newFakeChrome(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(mcpHelperEnv, "ok")

	m := NewManager()
	m.MCPCommand = []string{self}
	t.Cleanup(m.Disconnect)
	return m, f
}

// portOf extracts the port the fake Chrome bound to.
func portOf(t *testing.T, f *fakeChrome) int {
	t.Helper()
	_, portStr, ok := strings.Cut(strings.TrimPrefix(f.endpoint(), "http://"), ":")
	if !ok {
		t.Fatalf("cannot parse endpoint %q", f.endpoint())
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return port
}

func TestManagerConnectReportsTabAndBrowser(t *testing.T) {
	m, f := newTestManager(t)

	st, err := m.Connect(context.Background(), portOf(t, f), "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if !st.Connected {
		t.Error("Connected = false")
	}
	if !strings.HasPrefix(st.Browser, "Chrome/") {
		t.Errorf("Browser = %q", st.Browser)
	}
	if st.TabID != "TAB1" || st.TabURL != "https://example.com/" {
		t.Errorf("tab = %s %s", st.TabID, st.TabURL)
	}
}

func TestManagerConnectFailsWithoutBrowser(t *testing.T) {
	m, _ := newTestManager(t)

	// Port 1 is never a debugging port.
	_, err := m.Connect(context.Background(), 1, "")
	if err == nil {
		t.Fatal("Connect succeeded with no browser listening")
	}
	if !strings.Contains(err.Error(), "--remote-debugging-port") {
		t.Errorf("error = %v, want it to tell the user how to relaunch Chrome", err)
	}
}

// The frame handler runs on the protocol read loop, and acking is a request
// whose reply only that loop can deliver. If the ack were issued inline the
// pipeline would wedge on the first frame, so this asserts frames keep
// flowing and that each one is acked.
func TestManagerAcksFramesWithoutBlockingReadLoop(t *testing.T) {
	m, f := newTestManager(t)

	acked := make(chan int64, 16)
	f.handle = func(method string, params json.RawMessage) (any, *cdpError) {
		if method == "Page.screencastFrameAck" {
			var p struct {
				SessionID int64 `json:"sessionId"`
			}
			json.Unmarshal(params, &p)
			select {
			case acked <- p.SessionID:
			default:
			}
		}
		return nil, nil
	}

	if _, err := m.Connect(context.Background(), portOf(t, f), ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	frames, release := m.Subscribe()
	defer release()

	for i := 1; i <= 3; i++ {
		f.emit(t, "Page.screencastFrame", map[string]any{
			"data":      "frame" + strconv.Itoa(i),
			"sessionId": i,
			"metadata": map[string]any{
				"deviceWidth":   1024.0,
				"deviceHeight":  768.0,
				"scrollOffsetY": float64(i * 10),
			},
		})
	}

	seen := map[int64]bool{}
	deadline := time.After(10 * time.Second)
	for len(seen) < 3 {
		select {
		case id := <-acked:
			seen[id] = true
		case <-deadline:
			t.Fatalf("only %d of 3 frames were acked; the read loop is blocked", len(seen))
		}
	}

	// And the frames reached a subscriber with metadata intact.
	select {
	case fr := <-frames:
		if fr.Width != 1024 || fr.Height != 768 {
			t.Errorf("frame viewport = %vx%v, want 1024x768", fr.Width, fr.Height)
		}
		if !strings.HasPrefix(fr.Data, "frame") {
			t.Errorf("frame data = %q", fr.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame reached the subscriber")
	}
}

func TestManagerBroadcastsToEverySubscriber(t *testing.T) {
	m, f := newTestManager(t)

	if _, err := m.Connect(context.Background(), portOf(t, f), ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	a, releaseA := m.Subscribe()
	b, releaseB := m.Subscribe()
	defer releaseA()
	defer releaseB()

	f.emit(t, "Page.screencastFrame", map[string]any{
		"data":      "shared",
		"sessionId": 1,
		"metadata":  map[string]any{"deviceWidth": 800.0, "deviceHeight": 600.0},
	})

	var wg sync.WaitGroup
	for name, ch := range map[string]<-chan Frame{"a": a, "b": b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case fr := <-ch:
				if fr.Data != "shared" {
					t.Errorf("subscriber %s got %q", name, fr.Data)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("subscriber %s received nothing", name)
			}
		}()
	}
	wg.Wait()
}

// A viewer that stops reading must not stall the pump for everyone else.
func TestManagerDropsFramesForSlowSubscriber(t *testing.T) {
	m, f := newTestManager(t)

	if _, err := m.Connect(context.Background(), portOf(t, f), ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	slow, releaseSlow := m.Subscribe()
	defer releaseSlow()
	fast, releaseFast := m.Subscribe()
	defer releaseFast()

	// Never read from slow. Emit well past its buffer.
	for i := 1; i <= 20; i++ {
		f.emit(t, "Page.screencastFrame", map[string]any{
			"data":      "f" + strconv.Itoa(i),
			"sessionId": i,
			"metadata":  map[string]any{"deviceWidth": 800.0, "deviceHeight": 600.0},
		})
	}

	// The fast subscriber must still be receiving.
	received := 0
	timeout := time.After(10 * time.Second)
	for received < 2 {
		select {
		case <-fast:
			received++
		case <-timeout:
			t.Fatalf("fast subscriber received only %d frames; a slow viewer stalled the pump", received)
		}
	}
	_ = slow
}

func TestManagerRejectsInputWhenDisconnected(t *testing.T) {
	m, _ := newTestManager(t)

	if err := m.SendMouse(context.Background(), MouseEvent{Type: "mousePressed"}); err == nil {
		t.Error("SendMouse succeeded while disconnected")
	}
	if err := m.Navigate(context.Background(), "https://example.com"); err == nil {
		t.Error("Navigate succeeded while disconnected")
	}
	if _, err := m.RunTask(context.Background(), "do a thing", "ask"); err == nil {
		t.Error("RunTask succeeded while disconnected")
	}
}

func TestManagerForwardsInputAsPageCoordinates(t *testing.T) {
	m, f := newTestManager(t)

	got := make(chan json.RawMessage, 4)
	f.handle = func(method string, params json.RawMessage) (any, *cdpError) {
		if method == "Input.dispatchMouseEvent" {
			select {
			case got <- params:
			default:
			}
		}
		return nil, nil
	}

	if _, err := m.Connect(context.Background(), portOf(t, f), ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	err := m.SendMouse(context.Background(), MouseEvent{
		Type: "mousePressed", X: 120, Y: 340, Button: "left", ClickCount: 1,
	})
	if err != nil {
		t.Fatalf("SendMouse: %v", err)
	}

	select {
	case params := <-got:
		var ev MouseEvent
		if err := json.Unmarshal(params, &ev); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if ev.X != 120 || ev.Y != 340 {
			t.Errorf("coordinates = %v,%v want 120,340", ev.X, ev.Y)
		}
		if ev.Button != "left" || ev.ClickCount != 1 {
			t.Errorf("button = %q clicks = %d", ev.Button, ev.ClickCount)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no mouse event reached the browser")
	}
}

// A mouse move carries no button; it must serialize as "none" rather than
// being dropped by omitempty, which Chrome rejects.
func TestManagerDefaultsMouseButtonToNone(t *testing.T) {
	m, f := newTestManager(t)

	got := make(chan json.RawMessage, 4)
	f.handle = func(method string, params json.RawMessage) (any, *cdpError) {
		if method == "Input.dispatchMouseEvent" {
			select {
			case got <- params:
			default:
			}
		}
		return nil, nil
	}

	if _, err := m.Connect(context.Background(), portOf(t, f), ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := m.SendMouse(context.Background(), MouseEvent{Type: "mouseMoved", X: 5, Y: 5}); err != nil {
		t.Fatalf("SendMouse: %v", err)
	}

	select {
	case params := <-got:
		var raw map[string]any
		json.Unmarshal(params, &raw)
		if raw["button"] != "none" {
			t.Errorf("button = %v, want \"none\"", raw["button"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no mouse event reached the browser")
	}
}

func TestManagerDisconnectIsIdempotent(t *testing.T) {
	m, f := newTestManager(t)

	if _, err := m.Connect(context.Background(), portOf(t, f), ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	m.Disconnect()
	m.Disconnect()

	if st := m.Status(context.Background()); st.Connected {
		t.Error("Connected = true after Disconnect")
	}
}

func TestManagerReconnectsAfterDisconnect(t *testing.T) {
	m, f := newTestManager(t)
	port := portOf(t, f)

	if _, err := m.Connect(context.Background(), port, ""); err != nil {
		t.Fatalf("first Connect: %v", err)
	}
	m.Disconnect()

	st, err := m.Connect(context.Background(), port, "")
	if err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if !st.Connected {
		t.Error("Connected = false after reconnect")
	}
}

func TestIsBridgeAttached(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"", false},
		{"Not connected. Listening on ws://127.0.0.1:17374/extension", false},
		{"Connected to WebBrain in Chrome/141, 3 tabs available.", true},
	}
	for _, tc := range cases {
		if got := isBridgeAttached(tc.text); got != tc.want {
			t.Errorf("isBridgeAttached(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// Chrome emits a frame only when the page repaints, so a settled page produces
// exactly one. The pane always subscribes after Connect returns -- Connect
// waits on webbrain-mcp starting, which takes seconds -- so without a replay
// the pane would sit on an empty viewport over a perfectly healthy stream.
func TestManagerReplaysLastFrameToLateSubscriber(t *testing.T) {
	m, f := newTestManager(t)

	if _, err := m.Connect(context.Background(), portOf(t, f), ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The one and only frame arrives before anyone is listening.
	early, releaseEarly := m.Subscribe()
	f.emit(t, "Page.screencastFrame", map[string]any{
		"data":      "only-frame",
		"sessionId": 1,
		"metadata":  map[string]any{"deviceWidth": 1000.0, "deviceHeight": 613.0},
	})
	select {
	case <-early:
	case <-time.After(5 * time.Second):
		t.Fatal("the frame never reached the first subscriber")
	}
	releaseEarly()

	// A viewer joining afterwards must still see the page.
	late, releaseLate := m.Subscribe()
	defer releaseLate()

	select {
	case fr := <-late:
		if fr.Data != "only-frame" {
			t.Errorf("replayed frame = %q, want the retained one", fr.Data)
		}
		if fr.Width != 1000 || fr.Height != 613 {
			t.Errorf("replayed viewport = %vx%v", fr.Width, fr.Height)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no frame replayed to the late subscriber")
	}
}

// A frame from a previous session must not be replayed as if it were live.
func TestManagerDropsRetainedFrameOnDisconnect(t *testing.T) {
	m, f := newTestManager(t)
	port := portOf(t, f)

	if _, err := m.Connect(context.Background(), port, ""); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	sub, release := m.Subscribe()
	f.emit(t, "Page.screencastFrame", map[string]any{
		"data":      "stale",
		"sessionId": 1,
		"metadata":  map[string]any{"deviceWidth": 800.0, "deviceHeight": 600.0},
	})
	select {
	case <-sub:
	case <-time.After(5 * time.Second):
		t.Fatal("frame never arrived")
	}
	release()

	m.Disconnect()

	fresh, releaseFresh := m.Subscribe()
	defer releaseFresh()

	select {
	case fr := <-fresh:
		t.Fatalf("a stale frame %q was replayed after disconnect", fr.Data)
	case <-time.After(300 * time.Millisecond):
		// Nothing replayed, which is correct.
	}
}
