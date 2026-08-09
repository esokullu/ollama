package browser

import (
	"context"
	"encoding/base64"
	"os"
	"strconv"
	"testing"
	"time"
)

// Opt-in integration test against a real browser. Fakes cannot catch the
// things this does: a wrong handshake constant, a CDP parameter Chrome
// rejects, or a frame that never arrives because nothing repainted.
//
//	open -a "Google Chrome" --args --remote-debugging-port=9333
//	OLLAMA_TEST_CHROME_PORT=9333 go test ./app/browser/ -run TestRealChrome -v
//
// Set OLLAMA_TEST_WEBBRAIN_MCP to a webbrain-mcp entry point to exercise the
// bridge too; without it the WebBrain half is skipped.
func TestRealChrome(t *testing.T) {
	portEnv := os.Getenv("OLLAMA_TEST_CHROME_PORT")
	if portEnv == "" {
		t.Skip("set OLLAMA_TEST_CHROME_PORT to run against a real browser")
	}
	port, err := strconv.Atoi(portEnv)
	if err != nil {
		t.Fatalf("OLLAMA_TEST_CHROME_PORT=%q: %v", portEnv, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	m := NewManager()
	if mcp := os.Getenv("OLLAMA_TEST_WEBBRAIN_MCP"); mcp != "" {
		m.MCPCommand = []string{"node", mcp}
	} else {
		// A command that exits immediately keeps Connect from reaching for npx
		// and pulling the package off the network mid-test.
		m.MCPCommand = []string{"false"}
	}
	defer m.Disconnect()

	st, err := m.Connect(ctx, port, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Logf("browser=%s tab=%q url=%s", st.Browser, st.TabTitle, st.TabURL)
	t.Logf("webbrain attached=%v", st.Attached)

	frames, release := m.Subscribe()
	defer release()

	// A real JPEG frame from a real page.
	select {
	case fr := <-frames:
		raw, err := base64.StdEncoding.DecodeString(fr.Data)
		if err != nil {
			t.Fatalf("frame is not valid base64: %v", err)
		}
		if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xD8 {
			t.Fatalf("frame is not a JPEG (first bytes %x), len=%d", raw[:min(4, len(raw))], len(raw))
		}
		t.Logf("frame: %d bytes JPEG, viewport %.0fx%.0f", len(raw), fr.Width, fr.Height)
	case <-time.After(20 * time.Second):
		t.Fatal("no screencast frame arrived from real Chrome")
	}

	// Navigation must be reflected in the attached tab.
	if err := m.Navigate(ctx, "https://example.org/"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if url := m.Status(ctx).TabURL; url == "https://example.org/" {
			t.Logf("navigated to %s", url)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if url := m.Status(ctx).TabURL; url != "https://example.org/" {
		t.Fatalf("tab url = %q, want example.org after navigate", url)
	}

	// Input must be accepted by a real Chrome (it rejects malformed events).
	if err := m.SendMouse(ctx, MouseEvent{Type: "mouseMoved", X: 10, Y: 10}); err != nil {
		t.Fatalf("SendMouse move: %v", err)
	}
	if err := m.SendMouse(ctx, MouseEvent{Type: "mousePressed", X: 10, Y: 10, Button: "left", ClickCount: 1}); err != nil {
		t.Fatalf("SendMouse press: %v", err)
	}
	if err := m.SendMouse(ctx, MouseEvent{Type: "mouseReleased", X: 10, Y: 10, Button: "left", ClickCount: 1}); err != nil {
		t.Fatalf("SendMouse release: %v", err)
	}
	if err := m.SendMouse(ctx, MouseEvent{Type: "mouseWheel", X: 10, Y: 10, DeltaY: 120}); err != nil {
		t.Fatalf("SendMouse wheel: %v", err)
	}
	if err := m.SendKey(ctx, KeyEvent{Type: "keyDown", Key: "a", Code: "KeyA", Text: "a", UnmodifiedText: "a", WindowsVirtualKeyCode: 65}); err != nil {
		t.Fatalf("SendKey down: %v", err)
	}
	if err := m.SendKey(ctx, KeyEvent{Type: "char", Text: "a", Key: "a"}); err != nil {
		t.Fatalf("SendKey char: %v", err)
	}
	if err := m.SendKey(ctx, KeyEvent{Type: "keyUp", Key: "a", Code: "KeyA", WindowsVirtualKeyCode: 65}); err != nil {
		t.Fatalf("SendKey up: %v", err)
	}

	// Chrome only emits a frame when the page repaints, so a settled page goes
	// quiet. Force repaints and confirm frames still arrive, which proves the
	// acks are landing -- an unacked frame stops the stream permanently.
	drain := func() int {
		seen := 0
		for {
			select {
			case <-frames:
				seen++
			case <-time.After(2 * time.Second):
				return seen
			}
		}
	}
	drain()

	if err := m.Navigate(ctx, "https://example.com/"); err != nil {
		t.Fatalf("Navigate back: %v", err)
	}
	if got := drain(); got == 0 {
		t.Fatal("no frames after a repaint; the ack loop stalled")
	} else {
		t.Logf("stream still live after input and navigation (%d frames)", got)
	}

	tabs, err := m.Tabs(ctx)
	if err != nil {
		t.Fatalf("Tabs: %v", err)
	}
	t.Logf("tabs: %d", len(tabs))
}
