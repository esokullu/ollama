package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// mcpProtocolVersion is the revision we negotiate. The webbrain-mcp server
// runs SDK 1.30, which accepts this alongside newer revisions; asking for the
// newest would break against older published builds of the server.
const mcpProtocolVersion = "2025-06-18"

// DefaultMCPCommand launches the published webbrain-mcp server. Pointing this
// at a local checkout ("node", "/path/mcp-server/dist/index.js") is the usual
// way to develop against unreleased changes.
var DefaultMCPCommand = []string{"npx", "-y", "@webbrain/mcp-server"}

// mcpShutdownGrace is how long a closed stdin gets to bring the server down
// before it is killed. npx adds a process layer that can be slow to unwind.
const mcpShutdownGrace = 5 * time.Second

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
	Method  string          `json:"method"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)
}

// MCP is a JSON-RPC client speaking to an MCP server over its stdio. It owns
// the child process for its lifetime.
type MCP struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	server string // name reported by the server at initialize

	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	closed  bool

	nextID atomic.Int64

	exited chan struct{}
	// exitErr records why the child stopped, so a request failing mid-flight
	// can say "server exited" rather than a bare pipe error.
	exitErr atomic.Pointer[error]
}

// StartMCP spawns the MCP server and completes the initialize handshake.
func StartMCP(ctx context.Context, command []string) (*MCP, error) {
	if len(command) == 0 {
		command = DefaultMCPCommand
	}

	cmd := exec.Command(command[0], command[1:]...)
	configureMCPProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", strings.Join(command, " "), err)
	}

	m := &MCP{
		cmd:     cmd,
		stdin:   stdin,
		pending: make(map[int64]chan rpcResponse),
		exited:  make(chan struct{}),
	}

	go m.readLoop(stdout)
	go m.drainStderr(stderr)
	go func() {
		err := cmd.Wait()
		m.exitErr.Store(&err)
		m.failAllPending()
		close(m.exited)
	}()

	if err := m.initialize(ctx); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func (m *MCP) initialize(ctx context.Context) error {
	res, err := m.Call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "ollama-app",
			"version": "1",
		},
	})
	if err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}

	var info struct {
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(res, &info); err == nil {
		m.server = info.ServerInfo.Name
	}

	// The spec requires this notification before any other request.
	return m.Notify("notifications/initialized", nil)
}

// ServerName reports the server's self-declared name, available after start.
func (m *MCP) ServerName() string { return m.server }

func (m *MCP) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	// MCP frames are newline-delimited JSON and a tool result carrying page
	// text can be large, so the default 64KB token limit is far too small.
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Servers occasionally print banners to stdout before framing
			// starts; skipping them is kinder than killing the session.
			slog.Debug("mcp: skipping unparseable stdout line", "line", string(line))
			continue
		}

		// Server-initiated requests and notifications are not something we
		// subscribe to, so anything without a matching id is dropped.
		if resp.ID == nil {
			continue
		}

		m.mu.Lock()
		ch, ok := m.pending[*resp.ID]
		delete(m.pending, *resp.ID)
		m.mu.Unlock()

		if ok {
			ch <- resp
			close(ch)
		}
	}
}

func (m *MCP) drainStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 8<<10), 1<<20)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			slog.Debug("webbrain-mcp", "message", line)
		}
	}
}

func (m *MCP) failAllPending() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ch := range m.pending {
		close(ch)
		delete(m.pending, id)
	}
}

// Notify sends a request that expects no reply.
func (m *MCP) Notify(method string, params any) error {
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return m.writeLine(payload)
}

// Call sends a request and waits for its reply.
func (m *MCP) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := m.nextID.Add(1)
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", method, err)
	}

	ch := make(chan rpcResponse, 1)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("mcp server is not running")
	}
	m.pending[id] = ch
	m.mu.Unlock()

	if err := m.writeLine(payload); err != nil {
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		return nil, fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		m.mu.Lock()
		delete(m.pending, id)
		m.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			if err := m.exitErr.Load(); err != nil && *err != nil {
				return nil, fmt.Errorf("%s: webbrain-mcp exited: %w", method, *err)
			}
			return nil, fmt.Errorf("%s: webbrain-mcp stopped responding", method)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %w", method, resp.Error)
		}
		return resp.Result, nil
	}
}

func (m *MCP) writeLine(payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("mcp server is not running")
	}
	if _, err := m.stdin.Write(append(payload, '\n')); err != nil {
		return err
	}
	return nil
}

// ToolResult is the text a tool returned, plus whether the server flagged it
// as an error. MCP reports tool failures in-band rather than as RPC errors.
type ToolResult struct {
	Text    string
	IsError bool
}

// CallTool invokes a tool and flattens its content blocks to text. Non-text
// blocks are not something the WebBrain tools emit, so they are ignored.
func (m *MCP) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, err := m.Call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return ToolResult{}, err
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return ToolResult{}, fmt.Errorf("decode %s result: %w", name, err)
	}

	var b strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(block.Text)
		}
	}
	return ToolResult{Text: b.String(), IsError: out.IsError}, nil
}

// Tools lists the tool names the server exposes.
func (m *MCP) Tools(ctx context.Context) ([]string, error) {
	res, err := m.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}

	names := make([]string, 0, len(out.Tools))
	for _, t := range out.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}

// Close shuts the server down and waits for the process to exit.
func (m *MCP) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	stdin := m.stdin
	m.mu.Unlock()

	// Closing stdin is the graceful stdio shutdown signal; the server exits on
	// its own, and Wait's goroutine wakes up the pending callers.
	if stdin != nil {
		stdin.Close()
	}

	select {
	case <-m.exited:
	case <-time.After(mcpShutdownGrace):
		killMCPProcess(m.cmd)
		<-m.exited
	}

	// Even a clean exit of the launcher can leave the real server behind, so
	// sweep the group unconditionally. Killing an already-dead group is a
	// no-op.
	killMCPProcess(m.cmd)

	m.failAllPending()
	return nil
}

// --- WebBrain tool wrappers ---

// Connection reports whether the WebBrain extension is attached to the bridge,
// and how to fix it when it is not.
func (m *MCP) Connection(ctx context.Context) (ToolResult, error) {
	return m.CallTool(ctx, "webbrain_connection", nil)
}

// RunOptions configures a delegated browser task.
type RunOptions struct {
	Task           string
	Mode           string // "ask" (read-only) or "act"
	TabID          *int
	TimeoutSeconds int
	// Wait false returns a run_id immediately instead of blocking until the
	// task settles, which is what the pane wants so the UI stays responsive.
	Wait bool
}

// Run delegates a task to WebBrain in the user's browser.
func (m *MCP) Run(ctx context.Context, opts RunOptions) (ToolResult, error) {
	mode := opts.Mode
	if mode == "" {
		mode = "ask"
	}
	args := map[string]any{
		"task": opts.Task,
		"mode": mode,
		"wait": opts.Wait,
	}
	if opts.TabID != nil {
		args["tab_id"] = *opts.TabID
	}
	if opts.TimeoutSeconds > 0 {
		args["timeout_seconds"] = opts.TimeoutSeconds
	}
	return m.CallTool(ctx, "webbrain_run", args)
}

// Status fetches one run, or lists all runs when runID is empty.
func (m *MCP) Status(ctx context.Context, runID string) (ToolResult, error) {
	args := map[string]any{}
	if runID != "" {
		args["run_id"] = runID
	}
	return m.CallTool(ctx, "webbrain_status", args)
}

// Respond answers a run parked at needs_user_input.
func (m *MCP) Respond(ctx context.Context, runID, clarifyID, answer string) (ToolResult, error) {
	return m.CallTool(ctx, "webbrain_respond", map[string]any{
		"run_id":     runID,
		"clarify_id": clarifyID,
		"answer":     answer,
	})
}

// Abort stops a run. Actions already taken are not undone.
func (m *MCP) Abort(ctx context.Context, runID string) (ToolResult, error) {
	return m.CallTool(ctx, "webbrain_abort", map[string]any{"run_id": runID})
}
