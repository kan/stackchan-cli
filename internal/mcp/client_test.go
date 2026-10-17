package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRequest is a message the client sent to the gateway.
type fakeRequest struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// fakeGateway stands in for the gateway's stdio. Each request the client sends
// goes to handle, which replies with send (or not at all).
type fakeGateway struct {
	out io.WriteCloser // the client's stdout
}

func (g *fakeGateway) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	_, _ = g.out.Write(append(b, '\n'))
}

// reply answers req with a JSON-RPC result.
func (g *fakeGateway) reply(req fakeRequest, result any) {
	g.send(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
}

// textResult is a tools/call result with one text block.
func textResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

// newFakeClient connects a client to a fake gateway that passes every
// request to handle (on its own goroutine).
func newFakeClient(t *testing.T, handle func(g *fakeGateway, req fakeRequest)) *Client {
	t.Helper()
	reqR, reqW := io.Pipe()   // client -> gateway
	respR, respW := io.Pipe() // gateway -> client
	g := &fakeGateway{out: respW}
	c := newClient(reqW, respR)
	go func() {
		sc := bufio.NewScanner(reqR)
		for sc.Scan() {
			var req fakeRequest
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				t.Errorf("client sent invalid JSON %q: %v", sc.Text(), err)
				continue
			}
			handle(g, req)
		}
	}()
	t.Cleanup(func() {
		_ = c.Close() // closes reqW, which ends the fake gateway
		_ = respW.Close()
	})
	return c
}

func TestInitializeThenCallTool(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var gotArgs map[string]any
	c := newFakeClient(t, func(g *fakeGateway, req fakeRequest) {
		mu.Lock()
		methods = append(methods, req.Method)
		mu.Unlock()
		switch req.Method {
		case "initialize":
			g.reply(req, map[string]any{"protocolVersion": "2024-11-05"})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			mu.Lock()
			gotArgs = p.Arguments
			mu.Unlock()
			g.reply(req, textResult(`{"face":"happy","ok":true}`))
		}
	})

	if err := c.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	res, err := c.CallTool("set_avatar", map[string]any{"face": "happy"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Errorf("IsError = true, want false")
	}
	if got, want := res.Text(), `{"face":"happy","ok":true}`; got != want {
		t.Errorf("Text() = %q, want %q", got, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(methods, ","), "initialize,notifications/initialized,tools/call"; got != want {
		t.Errorf("methods = %s, want %s", got, want)
	}
	if gotArgs["face"] != "happy" {
		t.Errorf("arguments = %v, want face=happy", gotArgs)
	}
}

func TestHasErrorField(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{`{"error": "No ESP32 device connected"}`, true},
		{`{"error": {"code": 1}}`, true},
		{`{"error": null, "ok": true}`, false},
		{`{"ok": true}`, false},
		{`not json`, false},
		{``, false},
	}
	for _, tt := range tests {
		if got := hasErrorField(tt.text); got != tt.want {
			t.Errorf("hasErrorField(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestCallToolSetsIsErrorFromErrorField(t *testing.T) {
	c := newFakeClient(t, func(g *fakeGateway, req fakeRequest) {
		g.reply(req, textResult(`{"error": "No ESP32 device connected"}`))
	})
	res, err := c.CallTool("move_head", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true for a result with an error field")
	}
}

func TestRPCErrorIsReturned(t *testing.T) {
	c := newFakeClient(t, func(g *fakeGateway, req fakeRequest) {
		g.send(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": -32601, "message": "no such tool"}})
	})
	_, err := c.CallTool("nope", nil)
	if err == nil || !strings.Contains(err.Error(), "rpc error -32601: no such tool") {
		t.Fatalf("err = %v, want the rpc error", err)
	}
}

func TestNotificationGoesToHandlerNotToCaller(t *testing.T) {
	type note struct {
		method string
		params string
	}
	notes := make(chan note, 2)
	c := newFakeClient(t, func(g *fakeGateway, req fakeRequest) {
		// Notifications arrive before the reply; neither may be taken as it.
		g.send(map[string]any{"jsonrpc": "2.0", "method": "stackchan/event", "params": map[string]any{"event_type": "touch", "duration_ms": 300}})
		g.send(map[string]any{"jsonrpc": "2.0", "id": nil, "method": "stackchan/event", "params": map[string]any{"event_type": "touch", "duration_ms": 1500}})
		g.send("not an object")
		g.reply(req, textResult("done"))
	})
	c.OnNotification(func(method string, params json.RawMessage) {
		notes <- note{method, string(params)}
	})

	res, err := c.CallTool("get_status", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.Text() != "done" {
		t.Errorf("Text() = %q, want the reply, not a notification", res.Text())
	}
	for _, wantMs := range []string{`"duration_ms":300`, `"duration_ms":1500`} {
		select {
		case n := <-notes:
			if n.method != "stackchan/event" || !strings.Contains(n.params, wantMs) {
				t.Errorf("notification = %+v, want stackchan/event with %s", n, wantMs)
			}
		case <-time.After(time.Second):
			t.Fatalf("notification with %s not delivered", wantMs)
		}
	}
}

func TestCallTimesOut(t *testing.T) {
	var lateID *int
	c := newFakeClient(t, func(g *fakeGateway, req fakeRequest) {
		if lateID == nil { // leave the first call unanswered
			lateID = req.ID
			return
		}
		// The first reply arrives only now; it must not answer this call.
		g.reply(fakeRequest{ID: lateID}, textResult("late"))
		g.reply(req, textResult("fresh"))
	})
	start := time.Now()
	_, err := c.CallToolTimeout("say", nil, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %s", elapsed)
	}

	res, err := c.CallTool("get_status", nil)
	if err != nil {
		t.Fatalf("CallTool after a timeout: %v", err)
	}
	if res.Text() != "fresh" {
		t.Errorf("Text() = %q, want %q (the late reply was taken as this call's)", res.Text(), "fresh")
	}
}

func TestGatewayClosingStdoutFailsTheCall(t *testing.T) {
	c := newFakeClient(t, func(g *fakeGateway, req fakeRequest) {
		_ = g.out.Close() // the gateway died without answering
	})
	_, err := c.CallTool("get_status", nil)
	if err == nil || !strings.Contains(err.Error(), "gateway closed stdout before responding") {
		t.Fatalf("err = %v, want the closed-stdout error", err)
	}
}

// TestStartAndClose runs this test binary as a fake gateway process
// (TestHelperGateway), to check Start and that Close lets it exit by itself.
func TestStartAndClose(t *testing.T) {
	c, err := Start(os.Args[0], []string{"-test.run=^TestHelperGateway$"}, append(os.Environ(), "STACKCHAN_FAKE_GATEWAY=1"), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "get_status" {
		t.Errorf("tools = %+v, want [get_status]", tools)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Exit code 0 means the gateway saw EOF and exited; a kill gives non-zero.
	if code := c.cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("gateway exit code = %d, want 0 (closed gracefully, not killed)", code)
	}
}

// TestHelperGateway is not a test: TestStartAndClose starts it as a child
// process that answers initialize and tools/list until stdin is closed.
func TestHelperGateway(t *testing.T) {
	if os.Getenv("STACKCHAN_FAKE_GATEWAY") != "1" {
		t.Skip("helper process for TestStartAndClose")
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req fakeRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any = map[string]any{}
		if req.Method == "tools/list" {
			result = map[string]any{"tools": []map[string]any{{"name": "get_status", "inputSchema": map[string]any{}}}}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		fmt.Println(string(b))
	}
	os.Exit(0)
}
