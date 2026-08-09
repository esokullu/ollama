package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Target is one debuggable page reported by Chrome's /json/list endpoint.
type Target struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
	WSURL string `json:"webSocketDebuggerUrl"`
}

// cdpRequest is an outbound Chrome DevTools Protocol command.
type cdpRequest struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// cdpMessage is any inbound frame. A reply carries ID and one of
// Result/Error; an event carries Method and Params.
type cdpMessage struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *cdpError       `json:"error"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("cdp error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message)
}

// EventFunc receives every protocol event on the read loop's goroutine.
// Implementations must not block.
type EventFunc func(method string, params json.RawMessage)

// CDP is a client for one attached page target. Attaching to a different
// target replaces the underlying socket, so a CDP value tracks exactly one
// page at a time.
type CDP struct {
	endpoint string // http://127.0.0.1:9222

	mu      sync.Mutex
	ws      *wsConn
	target  Target
	pending map[int64]chan cdpMessage
	closed  bool

	nextID atomic.Int64

	onEvent atomic.Pointer[EventFunc]

	// done closes when the read loop exits so callers can detect a browser
	// that went away without an explicit disconnect.
	done chan struct{}
}

// NewCDP returns a client for a Chrome listening on endpoint, e.g.
// "http://127.0.0.1:9222". It does not connect; call Attach.
func NewCDP(endpoint string) *CDP {
	return &CDP{
		endpoint: endpoint,
		pending:  make(map[int64]chan cdpMessage),
		done:     make(chan struct{}),
	}
}

// httpClient is short-timeout on purpose: these are loopback JSON endpoints,
// and a hang here should surface as "Chrome is not reachable" quickly.
var httpClient = &http.Client{Timeout: 5 * time.Second}

func (c *CDP) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach chrome at %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("chrome %s returned %s: %s", path, resp.Status, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Version reports the browser build, and doubles as a reachability probe.
func (c *CDP) Version(ctx context.Context) (string, error) {
	var v struct {
		Browser string `json:"Browser"`
	}
	if err := c.getJSON(ctx, "/json/version", &v); err != nil {
		return "", err
	}
	return v.Browser, nil
}

// Targets lists debuggable page targets, most recently used first, which is
// the order Chrome reports them in. Non-page targets (service workers,
// extension backgrounds, the WebBrain side panel) are filtered out.
func (c *CDP) Targets(ctx context.Context) ([]Target, error) {
	var all []Target
	if err := c.getJSON(ctx, "/json/list", &all); err != nil {
		return nil, err
	}

	pages := make([]Target, 0, len(all))
	for _, t := range all {
		if t.Type == "page" && t.WSURL != "" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// Attach connects to a target. Passing an empty targetID attaches to the
// first page target, which is the frontmost tab.
func (c *CDP) Attach(ctx context.Context, targetID string) (Target, error) {
	targets, err := c.Targets(ctx)
	if err != nil {
		return Target{}, err
	}
	if len(targets) == 0 {
		return Target{}, errors.New("no debuggable tabs; open a tab in the browser you launched with --remote-debugging-port")
	}

	target := targets[0]
	if targetID != "" {
		found := false
		for _, t := range targets {
			if t.ID == targetID {
				target, found = t, true
				break
			}
		}
		if !found {
			return Target{}, fmt.Errorf("tab %s is no longer open", targetID)
		}
	}

	ws, err := wsDial(ctx, target.WSURL)
	if err != nil {
		return Target{}, fmt.Errorf("attach to tab: %w", err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		ws.Close()
		return Target{}, errors.New("client is closed")
	}
	old := c.ws
	c.ws = ws
	c.target = target
	// Replies for the previous socket will never arrive.
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if old != nil {
		old.Close()
	}

	go c.readLoop(ws)
	return target, nil
}

// OnEvent installs the handler for protocol events, replacing any previous
// one. Pass nil to drop events.
func (c *CDP) OnEvent(fn EventFunc) {
	if fn == nil {
		c.onEvent.Store(nil)
		return
	}
	c.onEvent.Store(&fn)
}

func (c *CDP) readLoop(ws *wsConn) {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			c.mu.Lock()
			// Only tear down if this is still the live socket; Attach may have
			// swapped in a new one and closed this one deliberately.
			current := c.ws == ws
			if current {
				for id, ch := range c.pending {
					close(ch)
					delete(c.pending, id)
				}
			}
			stillOpen := !c.closed
			c.mu.Unlock()

			if current && stillOpen {
				c.signalDone()
			}
			return
		}

		var msg cdpMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if msg.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if ok {
				ch <- msg
				close(ch)
			}
			continue
		}

		if msg.Method != "" {
			if fn := c.onEvent.Load(); fn != nil {
				(*fn)(msg.Method, msg.Params)
			}
		}
	}
}

func (c *CDP) signalDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// Done closes when the connection to the browser drops.
func (c *CDP) Done() <-chan struct{} { return c.done }

// Target reports the currently attached tab as of the last refresh.
func (c *CDP) Target() Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target
}

// RefreshTarget re-reads the attached tab's title and URL. The values captured
// at attach time go stale the moment the page navigates -- whether the user
// typed a URL or WebBrain drove it -- and the socket stays bound to the same
// target across navigations, so nothing else would ever correct them.
func (c *CDP) RefreshTarget(ctx context.Context) Target {
	c.mu.Lock()
	current := c.target
	c.mu.Unlock()

	if current.ID == "" {
		return current
	}

	targets, err := c.Targets(ctx)
	if err != nil {
		return current
	}

	for _, t := range targets {
		if t.ID != current.ID {
			continue
		}
		c.mu.Lock()
		if t.WSURL == "" {
			t.WSURL = c.target.WSURL
		}
		c.target = t
		c.mu.Unlock()
		return t
	}
	return current
}

// Call issues a command and waits for its reply.
func (c *CDP) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	payload, err := json.Marshal(cdpRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", method, err)
	}

	ch := make(chan cdpMessage, 1)

	c.mu.Lock()
	if c.ws == nil || c.closed {
		c.mu.Unlock()
		return nil, errors.New("not attached to a tab")
	}
	ws := c.ws
	c.pending[id] = ch
	c.mu.Unlock()

	if err := ws.WriteText(payload); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case msg, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: connection to browser lost", method)
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%s: %w", method, msg.Error)
		}
		return msg.Result, nil
	}
}

// Close disconnects from the browser.
func (c *CDP) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	ws := c.ws
	c.ws = nil
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()

	c.signalDone()
	if ws != nil {
		return ws.Close()
	}
	return nil
}

// ScreencastFrame is the decoded payload of a Page.screencastFrame event.
type ScreencastFrame struct {
	Data      string `json:"data"` // base64 JPEG
	SessionID int64  `json:"sessionId"`
	Metadata  struct {
		OffsetTop       float64 `json:"offsetTop"`
		PageScaleFactor float64 `json:"pageScaleFactor"`
		DeviceWidth     float64 `json:"deviceWidth"`
		DeviceHeight    float64 `json:"deviceHeight"`
		ScrollOffsetX   float64 `json:"scrollOffsetX"`
		ScrollOffsetY   float64 `json:"scrollOffsetY"`
		Timestamp       float64 `json:"timestamp"`
	} `json:"metadata"`
}

// StartScreencast begins streaming frames. Frames arrive as
// Page.screencastFrame events and each must be acked or Chrome stops sending.
func (c *CDP) StartScreencast(ctx context.Context, maxWidth, maxHeight, quality int) error {
	if _, err := c.Call(ctx, "Page.enable", nil); err != nil {
		return err
	}
	_, err := c.Call(ctx, "Page.startScreencast", map[string]any{
		"format":        "jpeg",
		"quality":       quality,
		"maxWidth":      maxWidth,
		"maxHeight":     maxHeight,
		"everyNthFrame": 1,
	})
	return err
}

// AckFrame tells Chrome the frame was consumed and it may send the next one.
func (c *CDP) AckFrame(ctx context.Context, sessionID int64) error {
	_, err := c.Call(ctx, "Page.screencastFrameAck", map[string]any{"sessionId": sessionID})
	return err
}

// StopScreencast halts the frame stream, leaving the connection attached.
func (c *CDP) StopScreencast(ctx context.Context) error {
	_, err := c.Call(ctx, "Page.stopScreencast", nil)
	return err
}

// Navigate points the attached tab at a URL.
func (c *CDP) Navigate(ctx context.Context, url string) error {
	_, err := c.Call(ctx, "Page.navigate", map[string]any{"url": url})
	return err
}

// MouseEvent is a pointer event in CSS pixels of the page's viewport.
type MouseEvent struct {
	Type       string  `json:"type"` // mousePressed, mouseReleased, mouseMoved, mouseWheel
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     string  `json:"button,omitempty"` // none, left, middle, right
	Buttons    int     `json:"buttons,omitempty"`
	ClickCount int     `json:"clickCount,omitempty"`
	// Both deltas are always sent: Chrome rejects a mouseWheel that carries
	// only one of them, and a purely vertical scroll has deltaX of exactly 0,
	// which omitempty would drop.
	DeltaX    float64 `json:"deltaX"`
	DeltaY    float64 `json:"deltaY"`
	Modifiers int     `json:"modifiers,omitempty"`
}

// DispatchMouse forwards a pointer event to the page.
func (c *CDP) DispatchMouse(ctx context.Context, ev MouseEvent) error {
	if ev.Button == "" {
		ev.Button = "none"
	}
	_, err := c.Call(ctx, "Input.dispatchMouseEvent", ev)
	return err
}

// KeyEvent is a keyboard event. Type is keyDown, keyUp, or char.
type KeyEvent struct {
	Type                  string `json:"type"`
	Text                  string `json:"text,omitempty"`
	UnmodifiedText        string `json:"unmodifiedText,omitempty"`
	Key                   string `json:"key,omitempty"`
	Code                  string `json:"code,omitempty"`
	WindowsVirtualKeyCode int    `json:"windowsVirtualKeyCode,omitempty"`
	NativeVirtualKeyCode  int    `json:"nativeVirtualKeyCode,omitempty"`
	Modifiers             int    `json:"modifiers,omitempty"`
}

// DispatchKey forwards a keyboard event to the page.
func (c *CDP) DispatchKey(ctx context.Context, ev KeyEvent) error {
	_, err := c.Call(ctx, "Input.dispatchKeyEvent", ev)
	return err
}
