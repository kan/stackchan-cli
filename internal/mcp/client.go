// Package mcp implements a minimal MCP (Model Context Protocol) client that
// speaks newline-delimited JSON-RPC 2.0 over a child process's stdio.
//
// It is purpose-built for the stackchan-mcp gateway: we spawn the gateway as a
// child process and talk to it over stdin/stdout. The device-side WebSocket
// handshake (hello/session_id) is handled by the gateway itself, so this client
// only deals with the LLM-facing stdio surface: initialize -> tools/list /
// tools/call.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Client is a one-shot-friendly MCP stdio client bound to a child gateway process.
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	timeout time.Duration

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan response
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Tool describes one entry from tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolResult is the result of a tools/call.
type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

// ContentBlock is one piece of a tool result (we mainly use text).
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text returns the concatenated text content of the result.
func (r ToolResult) Text() string {
	out := ""
	for _, c := range r.Content {
		if c.Type == "text" {
			out += c.Text
		}
	}
	return out
}

// Start spawns the gateway executable and begins the stdout read loop.
// stderr is forwarded to the provided writer (nil discards it).
func Start(exePath string, args []string, env []string, stderr io.Writer) (*Client, error) {
	cmd := exec.Command(exePath, args...)
	cmd.Env = env
	if stderr != nil {
		cmd.Stderr = stderr
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", exePath, err)
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		timeout: 20 * time.Second,
		nextID:  1,
		pending: make(map[int]chan response),
	}
	go c.readLoop(stdout)
	return c, nil
}

func (c *Client) readLoop(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	// tools/list responses are several KB; allow generous line size.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var resp response
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			continue // skip non-JSON / partial lines
		}
		if resp.ID == nil {
			continue // server notification; ignored in one-shot mode
		}
		c.mu.Lock()
		ch := c.pending[*resp.ID]
		delete(c.pending, *resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
	// stdout closed: fail any outstanding waiters.
	c.mu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *Client) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(request{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: gateway closed stdout before responding", method)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: rpc error %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(c.timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: timed out after %s", method, c.timeout)
	}
}

func (c *Client) notify(method string, params any) error {
	return c.write(request{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) write(req request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(b)
	return err
}

// Initialize performs the MCP initialize handshake and sends the
// notifications/initialized notification.
func (c *Client) Initialize() error {
	_, err := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "stackchan-cli", "version": "0.1.0"},
	})
	if err != nil {
		return err
	}
	return c.notify("notifications/initialized", map[string]any{})
}

// ListTools returns the gateway's advertised tools.
func (c *Client) ListTools() ([]Tool, error) {
	raw, err := c.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tools/list: decode: %w", err)
	}
	return out.Tools, nil
}

// CallTool invokes a tool by name with the given arguments.
func (c *Client) CallTool(name string, args map[string]any) (ToolResult, error) {
	raw, err := c.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return ToolResult{}, err
	}
	var res ToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return ToolResult{}, fmt.Errorf("tools/call %s: decode: %w", name, err)
	}
	return res, nil
}

// Close terminates the gateway child process.
// Close shuts the gateway down gracefully: closing stdin signals EOF to the
// stdio MCP server, which lets the gateway flush pending WebSocket writes and
// close the device connection cleanly (so the device leaves the "speaking"
// state and stops lip-sync, rather than being left hanging by an abrupt kill).
// If it doesn't exit promptly, fall back to killing it.
func (c *Client) Close() error {
	_ = c.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-done
	}
	return nil
}
