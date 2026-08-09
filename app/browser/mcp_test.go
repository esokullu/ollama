package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake MCP server runs as a re-exec of the test binary. This keeps the
// test hermetic — real stdio, real process lifetime, no node or npx.
const mcpHelperEnv = "OLLAMA_TEST_MCP_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(mcpHelperEnv); mode != "" {
		runFakeMCPServer(mode)
		return
	}
	os.Exit(m.Run())
}

// runFakeMCPServer speaks just enough MCP to satisfy the client. mode selects
// a behaviour: "ok", "toolerror", or "crash".
func runFakeMCPServer(mode string) {
	if mode == "stubborn" {
		// Outlive stdin closing, the way the real server does under npx: the
		// wrapper holds the pipe open so the server never sees EOF. Only a
		// process-group kill reaches it.
		go func() {
			io.Copy(io.Discard, os.Stdin)
			select {} // deliberately ignore EOF
		}()
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)

	respond := func(id *int64, result any) {
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  result,
		})
		fmt.Fprintf(os.Stdout, "%s\n", payload)
	}

	for scanner.Scan() {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			// A banner on stdout before framing is something real servers do;
			// the client must tolerate it.
			fmt.Fprintln(os.Stdout, "webbrain-mcp starting")
			respond(req.ID, map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "webbrain", "version": "0.1.0"},
			})
		case "notifications/initialized":
			if mode == "crash" {
				os.Exit(1)
			}
		case "tools/list":
			respond(req.ID, map[string]any{
				"tools": []map[string]any{
					{"name": "webbrain_run"},
					{"name": "webbrain_status"},
					{"name": "webbrain_connection"},
				},
			})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)

			if mode == "toolerror" {
				respond(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "Not connected."}},
					"isError": true,
				})
				continue
			}

			// Echo the arguments back so the test can assert on them, and use
			// two blocks to exercise content flattening.
			args, _ := json.Marshal(p.Arguments)
			respond(req.ID, map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": p.Name},
					{"type": "image", "data": "ignored"},
					{"type": "text", "text": string(args)},
				},
			})
		}
	}
}

func startFakeMCP(t *testing.T, mode string) *MCP {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(mcpHelperEnv, mode)

	m, err := StartMCP(ctx, []string{self})
	if err != nil {
		t.Fatalf("StartMCP: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestMCPInitializeAndListTools(t *testing.T) {
	m := startFakeMCP(t, "ok")

	if m.ServerName() != "webbrain" {
		t.Errorf("ServerName = %q, want webbrain", m.ServerName())
	}

	tools, err := m.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 3 || tools[0] != "webbrain_run" {
		t.Errorf("Tools = %v", tools)
	}
}

func TestMCPRunPassesArguments(t *testing.T) {
	m := startFakeMCP(t, "ok")

	tab := 42
	res, err := m.Run(context.Background(), RunOptions{
		Task:           "list failed payments",
		Mode:           "act",
		TabID:          &tab,
		TimeoutSeconds: 120,
		Wait:           false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// First block is the tool name, second is the echoed arguments.
	name, args, ok := strings.Cut(res.Text, "\n")
	if !ok {
		t.Fatalf("result did not carry both content blocks: %q", res.Text)
	}
	if name != "webbrain_run" {
		t.Errorf("tool = %q, want webbrain_run", name)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(args), &got); err != nil {
		t.Fatalf("decode echoed args: %v", err)
	}
	if got["task"] != "list failed payments" {
		t.Errorf("task = %v", got["task"])
	}
	if got["mode"] != "act" {
		t.Errorf("mode = %v, want act", got["mode"])
	}
	if got["wait"] != false {
		t.Errorf("wait = %v, want false", got["wait"])
	}
	if got["tab_id"] != float64(42) {
		t.Errorf("tab_id = %v, want 42", got["tab_id"])
	}
	if got["timeout_seconds"] != float64(120) {
		t.Errorf("timeout_seconds = %v, want 120", got["timeout_seconds"])
	}
}

func TestMCPRunDefaultsToAskMode(t *testing.T) {
	m := startFakeMCP(t, "ok")

	// Ask mode is read-only. Defaulting to it matters: an empty mode must
	// never silently grant click-and-type access.
	res, err := m.Run(context.Background(), RunOptions{Task: "summarize this page"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	_, args, _ := strings.Cut(res.Text, "\n")
	var got map[string]any
	json.Unmarshal([]byte(args), &got)
	if got["mode"] != "ask" {
		t.Errorf("mode = %v, want ask", got["mode"])
	}
}

func TestMCPSurfacesToolErrorsInBand(t *testing.T) {
	m := startFakeMCP(t, "toolerror")

	res, err := m.Connection(context.Background())
	if err != nil {
		t.Fatalf("Connection returned a transport error: %v", err)
	}
	if !res.IsError {
		t.Error("IsError = false, want true for a failed tool call")
	}
	if !strings.Contains(res.Text, "Not connected") {
		t.Errorf("Text = %q, want the server's message", res.Text)
	}
}

func TestMCPReportsServerExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(mcpHelperEnv, "crash")

	// The server exits right after initialize, so either the handshake fails
	// or the first call does. Both are acceptable; a hang is not.
	m, err := StartMCP(ctx, []string{self})
	if err != nil {
		return
	}
	defer m.Close()

	if _, err := m.Tools(ctx); err == nil {
		t.Fatal("Tools succeeded against a server that exited")
	}
}

func TestMCPCallAfterCloseFails(t *testing.T) {
	m := startFakeMCP(t, "ok")
	m.Close()

	if _, err := m.Tools(context.Background()); err == nil {
		t.Fatal("Tools succeeded after Close")
	}
}

// A server that never sees stdin close must still be terminated, or it lingers
// holding the WebBrain bridge port and blocks every later session.
func TestMCPCloseKillsAServerThatIgnoresStdin(t *testing.T) {
	m := startFakeMCP(t, "stubborn")

	pid := m.cmd.Process.Pid
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Close waits for the process to be reaped, so it is already gone; confirm
	// the OS agrees rather than trusting the bookkeeping.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err != nil {
			return // signal could not be delivered: the process is gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pid %d survived Close; it is still holding the bridge port", pid)
}
