package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDebugPort is Chrome's conventional --remote-debugging-port.
	DefaultDebugPort = 9222

	// screencastQuality trades JPEG size against fidelity. Text stays readable
	// at 60 and the frames stay small enough to push over SSE at 30fps.
	screencastQuality = 60

	// screencastMaxWidth/Height cap the frames Chrome encodes. The pane is a
	// third of a window, so sending full 4K frames would waste most of the
	// bytes on pixels that get scaled away.
	screencastMaxWidth  = 1600
	screencastMaxHeight = 1200

	// frameBuffer is how many frames may queue between the protocol read loop
	// and the pump. Frames are worth dropping rather than delaying: a stale
	// frame is never worth showing once a newer one exists.
	frameBuffer = 4

	// subscriberBuffer is per-viewer. Same reasoning, smaller: a viewer that
	// cannot keep up should skip ahead, not accumulate lag.
	subscriberBuffer = 2
)

// Frame is one screencast image delivered to the UI. Width and Height are the
// page's CSS-pixel viewport, which is what input coordinates must be
// expressed in.
type Frame struct {
	Data    string  `json:"data"` // base64 JPEG
	Width   float64 `json:"width"`
	Height  float64 `json:"height"`
	ScrollX float64 `json:"scrollX"`
	ScrollY float64 `json:"scrollY"`
}

// Status is the pane's view of the world, polled by the UI.
type Status struct {
	Connected bool   `json:"connected"`
	Port      int    `json:"port"`
	Browser   string `json:"browser,omitempty"`
	TabID     string `json:"tabId,omitempty"`
	TabTitle  string `json:"tabTitle,omitempty"`
	TabURL    string `json:"tabUrl,omitempty"`
	WebBrain  string `json:"webbrain,omitempty"` // webbrain_connection text
	Attached  bool   `json:"attached"`           // WebBrain extension on the bridge
	LastError string `json:"lastError,omitempty"`
}

// Manager owns the connection to Chrome and to webbrain-mcp for the lifetime
// of the browser pane. It is safe for concurrent use.
type Manager struct {
	// MCPCommand overrides the command used to launch webbrain-mcp.
	MCPCommand []string

	mu        sync.Mutex
	cdp       *CDP
	mcp       *MCP
	port      int
	connected bool
	lastErr   string

	subs    map[int]chan Frame
	nextSub int

	// lastFrame is replayed to each new subscriber. Chrome only emits a frame
	// when the page repaints, so a page that has finished loading produces
	// exactly one and then nothing. Without a replay, any viewer that
	// subscribes after that frame -- which the pane always does, since Connect
	// waits on webbrain-mcp starting -- would wait forever on a stream that is
	// working perfectly.
	lastFrame *Frame

	frames chan ScreencastFrame
	stop   context.CancelFunc
	done   chan struct{}
}

// NewManager returns a disconnected manager.
func NewManager() *Manager {
	return &Manager{subs: make(map[int]chan Frame)}
}

// Connect attaches to a Chrome already listening on port, starts the
// screencast, and brings up webbrain-mcp. Calling it while connected
// reconnects from scratch.
func (m *Manager) Connect(ctx context.Context, port int, targetID string) (Status, error) {
	if port <= 0 {
		port = DefaultDebugPort
	}

	m.Disconnect()

	cdp := NewCDP(fmt.Sprintf("http://127.0.0.1:%d", port))

	version, err := cdp.Version(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("no browser is listening on port %d. Quit Chrome, then relaunch it with --remote-debugging-port=%d", port, port)
	}

	target, err := cdp.Attach(ctx, targetID)
	if err != nil {
		cdp.Close()
		return Status{}, err
	}

	frames := make(chan ScreencastFrame, frameBuffer)
	cdp.OnEvent(func(method string, params json.RawMessage) {
		if method != "Page.screencastFrame" {
			return
		}
		var fr ScreencastFrame
		if err := json.Unmarshal(params, &fr); err != nil {
			return
		}
		// This runs on the protocol read loop. Acking here would deadlock,
		// because the ack's reply can only be delivered by this same loop, so
		// the frame is handed to the pump instead. A full buffer means the
		// pump is behind; dropping is correct.
		select {
		case frames <- fr:
		default:
		}
	})

	if err := cdp.StartScreencast(ctx, screencastMaxWidth, screencastMaxHeight, screencastQuality); err != nil {
		cdp.Close()
		return Status{}, fmt.Errorf("start screencast: %w", err)
	}

	pumpCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	m.mu.Lock()
	m.cdp = cdp
	m.port = port
	m.connected = true
	m.lastErr = ""
	m.frames = frames
	m.stop = cancel
	m.done = done
	m.mu.Unlock()

	go m.pump(pumpCtx, cdp, frames, done)
	go m.watchDisconnect(pumpCtx, cdp)

	// WebBrain is optional: the pane is still a usable browser view without
	// it, so a failure here is reported rather than fatal.
	webbrain := ""
	attached := false
	if mcp, err := StartMCP(ctx, m.MCPCommand); err != nil {
		webbrain = fmt.Sprintf("WebBrain bridge unavailable: %v", err)
		slog.Warn("browser pane: webbrain-mcp did not start", "error", err)
	} else {
		m.mu.Lock()
		m.mcp = mcp
		m.mu.Unlock()

		if res, err := mcp.Connection(ctx); err == nil {
			webbrain = res.Text
			attached = isBridgeAttached(res.Text)
		}
	}

	return Status{
		Connected: true,
		Port:      port,
		Browser:   version,
		TabID:     target.ID,
		TabTitle:  target.Title,
		TabURL:    target.URL,
		WebBrain:  webbrain,
		Attached:  attached,
	}, nil
}

// isBridgeAttached reads webbrain_connection's prose. The tool reports status
// as text for a model to read, so there is no structured field to check; it
// says "Not connected" when the extension has not dialled in.
func isBridgeAttached(text string) bool {
	if text == "" {
		return false
	}
	return !strings.Contains(strings.ToLower(text), "not connected")
}

// pump acks frames and fans them out. Running off the read loop is what makes
// acking safe.
func (m *Manager) pump(ctx context.Context, cdp *CDP, frames <-chan ScreencastFrame, done chan struct{}) {
	defer close(done)

	for {
		select {
		case <-ctx.Done():
			return
		case fr, ok := <-frames:
			if !ok {
				return
			}

			ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := cdp.AckFrame(ackCtx, fr.SessionID)
			cancel()
			if err != nil {
				// A failed ack stops the stream, so there is nothing useful to
				// do but let the disconnect watcher notice.
				slog.Debug("browser pane: frame ack failed", "error", err)
			}

			m.broadcast(Frame{
				Data:    fr.Data,
				Width:   fr.Metadata.DeviceWidth,
				Height:  fr.Metadata.DeviceHeight,
				ScrollX: fr.Metadata.ScrollOffsetX,
				ScrollY: fr.Metadata.ScrollOffsetY,
			})
		}
	}
}

// watchDisconnect marks the manager disconnected when the browser goes away,
// so the UI stops showing a frozen last frame as if it were live.
func (m *Manager) watchDisconnect(ctx context.Context, cdp *CDP) {
	select {
	case <-ctx.Done():
	case <-cdp.Done():
		m.mu.Lock()
		if m.cdp == cdp {
			m.connected = false
			m.lastErr = "the browser closed or stopped listening on its debugging port"
		}
		m.mu.Unlock()
	}
}

func (m *Manager) broadcast(f Frame) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastFrame = &f

	for _, ch := range m.subs {
		select {
		case ch <- f:
		default:
			// Viewer is behind; skip this frame for it.
		}
	}
}

// Subscribe returns a channel of frames and a function to release it. The most
// recent frame, if any, is delivered immediately so a viewer joining a settled
// page sees it rather than an empty pane.
func (m *Manager) Subscribe() (<-chan Frame, func()) {
	ch := make(chan Frame, subscriberBuffer)

	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = ch
	if m.lastFrame != nil {
		ch <- *m.lastFrame
	}
	m.mu.Unlock()

	return ch, func() {
		m.mu.Lock()
		if existing, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(existing)
		}
		m.mu.Unlock()
	}
}

// Status reports the current connection state.
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	cdp, mcp, port, connected, lastErr := m.cdp, m.mcp, m.port, m.connected, m.lastErr
	m.mu.Unlock()

	st := Status{Connected: connected, Port: port, LastError: lastErr}
	if !connected || cdp == nil {
		return st
	}

	// Re-read rather than using the cached target: the page navigates under us
	// whenever the user or WebBrain moves it, and the address bar follows this.
	target := cdp.RefreshTarget(ctx)
	st.TabID, st.TabTitle, st.TabURL = target.ID, target.Title, target.URL

	if mcp != nil {
		if res, err := mcp.Connection(ctx); err == nil {
			st.WebBrain = res.Text
			st.Attached = isBridgeAttached(res.Text)
		}
	}
	return st
}

// Tabs lists the debuggable tabs, for the pane's tab picker.
func (m *Manager) Tabs(ctx context.Context) ([]Target, error) {
	m.mu.Lock()
	cdp := m.cdp
	m.mu.Unlock()

	if cdp == nil {
		return nil, errors.New("not connected to a browser")
	}
	return cdp.Targets(ctx)
}

// SelectTab reattaches the pane to a different tab and restarts the stream.
func (m *Manager) SelectTab(ctx context.Context, targetID string) (Target, error) {
	m.mu.Lock()
	cdp, port := m.cdp, m.port
	m.mu.Unlock()

	if cdp == nil {
		return Target{}, errors.New("not connected to a browser")
	}

	target, err := cdp.Attach(ctx, targetID)
	if err != nil {
		return Target{}, err
	}
	// Attach replaced the socket, so the screencast has to be requested again
	// on the new one.
	if err := cdp.StartScreencast(ctx, screencastMaxWidth, screencastMaxHeight, screencastQuality); err != nil {
		return Target{}, fmt.Errorf("restart screencast on port %d: %w", port, err)
	}
	return target, nil
}

// Navigate points the attached tab at a URL.
func (m *Manager) Navigate(ctx context.Context, url string) error {
	cdp, err := m.liveCDP()
	if err != nil {
		return err
	}
	return cdp.Navigate(ctx, url)
}

// SendMouse forwards a pointer event in page CSS pixels.
func (m *Manager) SendMouse(ctx context.Context, ev MouseEvent) error {
	cdp, err := m.liveCDP()
	if err != nil {
		return err
	}
	return cdp.DispatchMouse(ctx, ev)
}

// SendKey forwards a keyboard event.
func (m *Manager) SendKey(ctx context.Context, ev KeyEvent) error {
	cdp, err := m.liveCDP()
	if err != nil {
		return err
	}
	return cdp.DispatchKey(ctx, ev)
}

func (m *Manager) liveCDP() (*CDP, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.connected || m.cdp == nil {
		return nil, errors.New("browser pane is not connected")
	}
	return m.cdp, nil
}

func (m *Manager) liveMCP() (*MCP, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mcp == nil {
		return nil, errors.New("WebBrain is not connected. Set the cloud bridge to ws://127.0.0.1:17374/extension in WebBrain's settings")
	}
	return m.mcp, nil
}

// RunTask hands a task to WebBrain. It does not wait for the run to settle;
// the caller polls TaskStatus with the returned text's run_id.
func (m *Manager) RunTask(ctx context.Context, task, mode string) (ToolResult, error) {
	mcp, err := m.liveMCP()
	if err != nil {
		return ToolResult{}, err
	}
	return mcp.Run(ctx, RunOptions{Task: task, Mode: mode, Wait: false})
}

// TaskStatus polls a run, or lists all runs when runID is empty.
func (m *Manager) TaskStatus(ctx context.Context, runID string) (ToolResult, error) {
	mcp, err := m.liveMCP()
	if err != nil {
		return ToolResult{}, err
	}
	return mcp.Status(ctx, runID)
}

// RespondTask answers a run waiting on the user.
func (m *Manager) RespondTask(ctx context.Context, runID, clarifyID, answer string) (ToolResult, error) {
	mcp, err := m.liveMCP()
	if err != nil {
		return ToolResult{}, err
	}
	return mcp.Respond(ctx, runID, clarifyID, answer)
}

// AbortTask stops a run.
func (m *Manager) AbortTask(ctx context.Context, runID string) (ToolResult, error) {
	mcp, err := m.liveMCP()
	if err != nil {
		return ToolResult{}, err
	}
	return mcp.Abort(ctx, runID)
}

// Disconnect tears down both connections. It is safe to call when already
// disconnected, and the manager can be reconnected afterwards.
func (m *Manager) Disconnect() {
	m.mu.Lock()
	cdp, mcp, stop, done := m.cdp, m.mcp, m.stop, m.done
	m.cdp, m.mcp, m.stop, m.done = nil, nil, nil, nil
	m.connected = false
	// A stale frame from the previous session would otherwise be replayed to
	// the next subscriber as if it were live.
	m.lastFrame = nil
	m.mu.Unlock()

	if stop != nil {
		stop()
	}
	if cdp != nil {
		cdp.Close()
	}
	if mcp != nil {
		mcp.Close()
	}
	if done != nil {
		<-done
	}
}
