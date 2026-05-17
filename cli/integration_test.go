package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rpcResp is a lightweight JSON-RPC response decoder used by the E2E tests.
type rpcResp struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
	Error   map[string]any `json:"error,omitempty"`
}

// startTestServer spins up a Tasker mock + tasker-mcp HTTP server on an
// ephemeral local port. When runWatcher is true the tools-file mtime watcher
// goroutine is also started so hot-reload tests can observe reloads.
//
// Global state (taskerHost/Port/ApiKey/timeout/toolsPath) is overwritten for
// the duration of the test and restored via t.Cleanup.
func startTestServer(t *testing.T, taskerHandler http.HandlerFunc, initialTools []TaskerTool, runWatcher bool) (mcpURL string, taskerMock *httptest.Server, toolsPathOut string) {
	t.Helper()

	prevHost := taskerHost
	prevPort := taskerPort
	prevKey := taskerApiKey
	prevTimeout := taskerTimeoutDur
	prevTools := toolsPath

	taskerMock = httptest.NewServer(taskerHandler)
	u, err := url.Parse(taskerMock.URL)
	if err != nil {
		taskerMock.Close()
		t.Fatalf("parse tasker mock url %q: %v", taskerMock.URL, err)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
	}
	taskerHost = host
	taskerPort = port
	taskerApiKey = "test-key"
	taskerTimeoutDur = 5 * time.Second

	dir := t.TempDir()
	toolsPathOut = filepath.Join(dir, "toolDescriptions.json")
	body, err := json.MarshalIndent(initialTools, "", "  ")
	if err != nil {
		taskerMock.Close()
		t.Fatalf("marshal initial tools: %v", err)
	}
	if err := os.WriteFile(toolsPathOut, body, 0o644); err != nil {
		taskerMock.Close()
		t.Fatalf("write tools file: %v", err)
	}
	toolsPath = toolsPathOut

	mcpServer := NewMCPServer()
	mux := buildHTTPMux(mcpServer, "", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		taskerMock.Close()
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()

	ctx, cancel := context.WithCancel(context.Background())
	if runWatcher {
		go watchToolsFile(ctx, mcpServer, toolsPathOut)
	}

	// Allow the listener / watcher to settle.
	time.Sleep(50 * time.Millisecond)

	mcpURL = "http://" + ln.Addr().String() + "/mcp"

	t.Cleanup(func() {
		cancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
		taskerMock.Close()
		taskerHost = prevHost
		taskerPort = prevPort
		taskerApiKey = prevKey
		taskerTimeoutDur = prevTimeout
		toolsPath = prevTools
	})

	return mcpURL, taskerMock, toolsPathOut
}

// mcpCall sends a JSON-RPC POST to the MCP endpoint. Both plain JSON and
// SSE-wrapped ("data: {...}\n\n") response bodies are accepted.
func mcpCall(t *testing.T, mcpURL string, headers map[string]string, body string) (resp *rpcResp, sessionID string, rawBody string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, mcpURL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	defer r.Body.Close()
	bb, _ := io.ReadAll(r.Body)
	rawBody = string(bb)
	sessionID = r.Header.Get("Mcp-Session-Id")

	payload := strings.TrimSpace(rawBody)
	if strings.HasPrefix(payload, "event:") || strings.Contains(payload, "data: ") {
		// SSE response may interleave server-sent notifications (e.g.
		// notifications/tools/list_changed) before the actual JSON-RPC reply.
		// The last data block is the reply we want.
		if idx := strings.LastIndex(payload, "data: "); idx >= 0 {
			payload = strings.TrimSpace(payload[idx+len("data: "):])
		}
	}
	if payload == "" {
		return nil, sessionID, rawBody
	}
	out := &rpcResp{}
	if err := json.Unmarshal([]byte(payload), out); err != nil {
		t.Fatalf("decode rpc response (status=%d, raw=%q): %v", r.StatusCode, rawBody, err)
	}
	return out, sessionID, rawBody
}

// initSession runs the MCP initialize + notifications/initialized handshake
// and returns the session ID used for all subsequent calls.
func initSession(t *testing.T, mcpURL string) string {
	t.Helper()
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"e2e-test","version":"1.0"}}}`
	resp, sid, raw := mcpCall(t, mcpURL, nil, initBody)
	if resp == nil || resp.Result == nil {
		t.Fatalf("initialize: empty result (raw=%q)", raw)
	}
	if pv, _ := resp.Result["protocolVersion"].(string); pv != "2025-11-25" {
		t.Fatalf("initialize protocolVersion = %v, want 2025-11-25", resp.Result["protocolVersion"])
	}
	if sid == "" {
		t.Fatal("initialize: empty Mcp-Session-Id header")
	}
	// Notify initialized; ignore body (server returns 202).
	mcpCall(t, mcpURL, map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return sid
}

// extractText pulls result.content[0].text out of a tools/call response.
func extractText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("missing content array in result: %#v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] not an object: %#v", content[0])
	}
	text, _ := first["text"].(string)
	return text
}

// minimalSchema returns an inputSchema with a single required string property.
func minimalSchema(prop string) map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{prop},
		"properties": map[string]any{
			prop: map[string]any{
				"type":        "string",
				"description": "test prop",
			},
		},
	}
}

// -----------------------------------------------------------------------------
// Test 1: initialize + tools/list returns both registered tools
// -----------------------------------------------------------------------------

func TestE2E_InitializeAndToolsList(t *testing.T) {
	tools := []TaskerTool{
		{TaskerName: "Task A", Name: "tool_a", Description: "tool A", InputSchema: minimalSchema("msg")},
		{TaskerName: "Task B", Name: "tool_b", Description: "tool B", InputSchema: minimalSchema("msg")},
	}
	// Tasker mock allows the startup mcp_list_tools probe (responding 500 so the
	// CLI falls back to the file), but errors on any subsequent call since
	// initialize / tools/list must not hit Tasker.
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		bb, _ := io.ReadAll(r.Body)
		if strings.Contains(string(bb), `"name":"mcp_list_tools"`) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected tasker call: %s %s body=%q", r.Method, r.URL.Path, string(bb))
		http.Error(w, "no", http.StatusInternalServerError)
	}
	mcpURL, _, _ := startTestServer(t, taskerHandler, tools, false)
	sid := initSession(t, mcpURL)

	resp, _, raw := mcpCall(t, mcpURL,
		map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/list: no result (raw=%q)", raw)
	}
	listed, _ := resp.Result["tools"].([]any)
	if len(listed) != 2 {
		t.Fatalf("tools/list returned %d tools, want 2 (raw=%q)", len(listed), raw)
	}
	gotNames := map[string]bool{}
	for _, item := range listed {
		if m, ok := item.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				gotNames[n] = true
			}
		}
	}
	for _, want := range []string{"tool_a", "tool_b"} {
		if !gotNames[want] {
			t.Errorf("tools/list missing %q (got %v)", want, gotNames)
		}
	}
}

// -----------------------------------------------------------------------------
// Test 2: tools/call passes args + auth header to Tasker and returns body
// -----------------------------------------------------------------------------

func TestE2E_ToolCallSuccess(t *testing.T) {
	tools := []TaskerTool{
		{TaskerName: "Probe Task", Name: "probe", Description: "probe", InputSchema: minimalSchema("msg")},
	}

	var (
		mu     sync.Mutex
		gotBody string
		gotAuth string
		gotPath string
	)
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		bb, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(bb)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("task done"))
	}

	mcpURL, _, _ := startTestServer(t, taskerHandler, tools, false)
	sid := initSession(t, mcpURL)

	resp, _, raw := mcpCall(t, mcpURL,
		map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"probe","arguments":{"msg":"hi"}}}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/call: no result (raw=%q)", raw)
	}
	if isErr, _ := resp.Result["isError"].(bool); isErr {
		t.Fatalf("tools/call unexpectedly reported isError; result=%#v", resp.Result)
	}
	text := extractText(t, resp.Result)
	if !strings.Contains(text, "task done") {
		t.Fatalf("tools/call text=%q, want to contain %q", text, "task done")
	}

	mu.Lock()
	body := gotBody
	auth := gotAuth
	path := gotPath
	mu.Unlock()

	if path != "/run_task" {
		t.Errorf("tasker received path %q, want /run_task", path)
	}
	if auth != "Bearer test-key" {
		t.Errorf("tasker Authorization = %q, want %q", auth, "Bearer test-key")
	}
	if !strings.Contains(body, `"name":"Probe Task"`) {
		t.Errorf("tasker body missing tasker_name: %q", body)
	}
	if !strings.Contains(body, `"msg":"hi"`) {
		t.Errorf("tasker body missing argument: %q", body)
	}
}

// -----------------------------------------------------------------------------
// Test 3: non-200 from Tasker becomes an MCP error result (isError=true) that
// includes the upstream body so the LLM can self-correct.
// -----------------------------------------------------------------------------

func TestE2E_ToolCallErrorPassthrough(t *testing.T) {
	tools := []TaskerTool{
		{TaskerName: "Probe Task", Name: "probe", Description: "probe", InputSchema: minimalSchema("msg")},
	}
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("exploded"))
	}
	mcpURL, _, _ := startTestServer(t, taskerHandler, tools, false)
	sid := initSession(t, mcpURL)

	resp, _, raw := mcpCall(t, mcpURL,
		map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"probe","arguments":{"msg":"x"}}}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/call: no result (raw=%q)", raw)
	}
	isErr, _ := resp.Result["isError"].(bool)
	if !isErr {
		t.Fatalf("expected isError=true, got result=%#v", resp.Result)
	}
	text := extractText(t, resp.Result)
	if !strings.Contains(text, "tasker call failed") {
		t.Errorf("error text missing prefix; got %q", text)
	}
	if !strings.Contains(text, "exploded") {
		t.Errorf("error text missing upstream body; got %q", text)
	}
}

// -----------------------------------------------------------------------------
// Test 4: hot reload — rewriting the tools file is picked up by the watcher
// goroutine, and a subsequent tools/list reflects the new tool set.
// -----------------------------------------------------------------------------

func TestE2E_HotReload(t *testing.T) {
	tools := []TaskerTool{
		{TaskerName: "Task A", Name: "tool_a", Description: "tool A", InputSchema: minimalSchema("msg")},
	}
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}
	mcpURL, _, path := startTestServer(t, taskerHandler, tools, true)
	sid := initSession(t, mcpURL)

	// Baseline: one tool registered.
	resp, _, raw := mcpCall(t, mcpURL,
		map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/list pre-reload: no result (raw=%q)", raw)
	}
	if listed, _ := resp.Result["tools"].([]any); len(listed) != 1 {
		t.Fatalf("pre-reload tools count = %d, want 1", len(listed))
	}

	// Rewrite the tools file with a second tool added.
	newTools := []TaskerTool{
		{TaskerName: "Task A", Name: "tool_a", Description: "tool A", InputSchema: minimalSchema("msg")},
		{TaskerName: "Task B", Name: "tool_b", Description: "tool B", InputSchema: minimalSchema("msg")},
	}
	body, err := json.MarshalIndent(newTools, "", "  ")
	if err != nil {
		t.Fatalf("marshal new tools: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("rewrite tools file: %v", err)
	}
	// Force a distinct mtime to defeat filesystem timestamp granularity on
	// Windows / FAT-like filesystems and let the mtime watcher notice.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Watcher ticks every 2s; allow margin.
	time.Sleep(3 * time.Second)

	resp, _, raw = mcpCall(t, mcpURL,
		map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/list post-reload: no result (raw=%q)", raw)
	}
	listed, _ := resp.Result["tools"].([]any)
	if len(listed) != 2 {
		t.Fatalf("post-reload tools count = %d, want 2 (raw=%q)", len(listed), raw)
	}
	names := map[string]bool{}
	for _, item := range listed {
		if m, ok := item.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				names[n] = true
			}
		}
	}
	if !names["tool_b"] {
		t.Errorf("post-reload missing tool_b (got %v)", names)
	}
}

// -----------------------------------------------------------------------------
// Test 5: opt-in structured tool result via X-Tasker-Structured header
// -----------------------------------------------------------------------------

func TestE2E_StructuredResultOptIn(t *testing.T) {
	tools := []TaskerTool{
		{TaskerName: "Sensors", Name: "sensors", Description: "sensors", InputSchema: minimalSchema("msg")},
	}
	const payload = `{"battery":87,"wifi":"on"}`
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}
	mcpURL, _, _ := startTestServer(t, taskerHandler, tools, false)
	sid := initSession(t, mcpURL)

	callBody := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"sensors","arguments":{"msg":"q"}}}`

	t.Run("plain text without header", func(t *testing.T) {
		resp, _, raw := mcpCall(t, mcpURL,
			map[string]string{"Mcp-Session-Id": sid},
			callBody)
		if resp == nil || resp.Result == nil {
			t.Fatalf("tools/call: no result (raw=%q)", raw)
		}
		if _, ok := resp.Result["structuredContent"]; ok {
			t.Errorf("expected no structuredContent without header; got %#v", resp.Result["structuredContent"])
		}
		text := extractText(t, resp.Result)
		if text != payload {
			t.Errorf("text=%q, want raw JSON %q", text, payload)
		}
	})

	t.Run("structured with X-Tasker-Structured: true", func(t *testing.T) {
		resp, _, raw := mcpCall(t, mcpURL,
			map[string]string{
				"Mcp-Session-Id":      sid,
				"X-Tasker-Structured": "true",
			},
			callBody)
		if resp == nil || resp.Result == nil {
			t.Fatalf("tools/call: no result (raw=%q)", raw)
		}
		sc, ok := resp.Result["structuredContent"].(map[string]any)
		if !ok {
			t.Fatalf("expected structuredContent to be a JSON object; got %#v", resp.Result["structuredContent"])
		}
		if got := sc["battery"]; got != float64(87) {
			t.Errorf("structuredContent.battery = %v (%T), want 87", got, got)
		}
		if got := sc["wifi"]; got != "on" {
			t.Errorf("structuredContent.wifi = %v, want \"on\"", got)
		}
		text := extractText(t, resp.Result)
		if text != payload {
			t.Errorf("fallback text=%q, want %q", text, payload)
		}
	})
}

// -----------------------------------------------------------------------------
// Test 6: opting in but body is not JSON → text only, no structured field.
// -----------------------------------------------------------------------------

func TestE2E_StructuredResultNonJSON(t *testing.T) {
	tools := []TaskerTool{
		{TaskerName: "Status", Name: "status", Description: "status", InputSchema: minimalSchema("msg")},
	}
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}
	mcpURL, _, _ := startTestServer(t, taskerHandler, tools, false)
	sid := initSession(t, mcpURL)

	resp, _, raw := mcpCall(t, mcpURL,
		map[string]string{
			"Mcp-Session-Id":      sid,
			"X-Tasker-Structured": "true",
		},
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"status","arguments":{"msg":"q"}}}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/call: no result (raw=%q)", raw)
	}
	if _, ok := resp.Result["structuredContent"]; ok {
		t.Errorf("expected no structuredContent for non-JSON body; got %#v", resp.Result["structuredContent"])
	}
	if text := extractText(t, resp.Result); text != "OK" {
		t.Errorf("text=%q, want \"OK\"", text)
	}
}

// -----------------------------------------------------------------------------
// Test 7: opting in with a JSON array body → structuredContent is an []any.
// -----------------------------------------------------------------------------

func TestE2E_StructuredResultArray(t *testing.T) {
	tools := []TaskerTool{
		{TaskerName: "List", Name: "list_things", Description: "list", InputSchema: minimalSchema("msg")},
	}
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[1,2,3]"))
	}
	mcpURL, _, _ := startTestServer(t, taskerHandler, tools, false)
	sid := initSession(t, mcpURL)

	resp, _, raw := mcpCall(t, mcpURL,
		map[string]string{
			"Mcp-Session-Id":      sid,
			"X-Tasker-Structured": "1",
		},
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_things","arguments":{"msg":"q"}}}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/call: no result (raw=%q)", raw)
	}
	arr, ok := resp.Result["structuredContent"].([]any)
	if !ok {
		t.Fatalf("expected structuredContent to be a JSON array; got %#v", resp.Result["structuredContent"])
	}
	if len(arr) != 3 {
		t.Fatalf("structuredContent len=%d, want 3", len(arr))
	}
	for i, want := range []float64{1, 2, 3} {
		if got, _ := arr[i].(float64); got != want {
			t.Errorf("structuredContent[%d] = %v, want %v", i, arr[i], want)
		}
	}
	if text := extractText(t, resp.Result); text != "[1,2,3]" {
		t.Errorf("fallback text=%q, want \"[1,2,3]\"", text)
	}
}

// -----------------------------------------------------------------------------
// Test 8: Tasker online discovery via mcp_list_tools — online wins over file.
// The local --tools file is intentionally invalid JSON, proving that online
// discovery is used as the primary source when available.
// -----------------------------------------------------------------------------

func TestE2E_TaskerOnlineDiscovery(t *testing.T) {
	onlineTools := []TaskerTool{
		{TaskerName: "X", Name: "online_tool", Description: "discovered online", InputSchema: minimalSchema("msg")},
	}
	onlineBody, err := json.Marshal(onlineTools)
	if err != nil {
		t.Fatalf("marshal online tools: %v", err)
	}
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		bb, _ := io.ReadAll(r.Body)
		if strings.Contains(string(bb), `"name":"mcp_list_tools"`) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(onlineBody)
			return
		}
		t.Errorf("unexpected tasker call body=%q", string(bb))
		http.Error(w, "no", http.StatusInternalServerError)
	}

	// Set up globals + tasker mock manually (so we can write BAD JSON to the file).
	prevHost, prevPort, prevKey, prevTimeout, prevTools := taskerHost, taskerPort, taskerApiKey, taskerTimeoutDur, toolsPath
	taskerMock := httptest.NewServer(http.HandlerFunc(taskerHandler))
	u, err := url.Parse(taskerMock.URL)
	if err != nil {
		taskerMock.Close()
		t.Fatalf("parse tasker mock url: %v", err)
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	taskerHost = u.Hostname()
	taskerPort = port
	taskerApiKey = "test-key"
	taskerTimeoutDur = 5 * time.Second

	dir := t.TempDir()
	badPath := filepath.Join(dir, "toolDescriptions.json")
	if err := os.WriteFile(badPath, []byte("not a valid json array"), 0o644); err != nil {
		taskerMock.Close()
		t.Fatalf("write bad tools file: %v", err)
	}
	toolsPath = badPath

	mcpServer := NewMCPServer()
	mux := buildHTTPMux(mcpServer, "", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		taskerMock.Close()
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	time.Sleep(50 * time.Millisecond)

	mcpURL := "http://" + ln.Addr().String() + "/mcp"

	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
		taskerMock.Close()
		taskerHost, taskerPort, taskerApiKey, taskerTimeoutDur, toolsPath = prevHost, prevPort, prevKey, prevTimeout, prevTools
	})

	sid := initSession(t, mcpURL)

	resp, _, raw := mcpCall(t, mcpURL,
		map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/list: no result (raw=%q)", raw)
	}
	listed, _ := resp.Result["tools"].([]any)
	if len(listed) != 1 {
		t.Fatalf("expected 1 online tool, got %d (raw=%q)", len(listed), raw)
	}
	names := map[string]bool{}
	for _, item := range listed {
		if m, ok := item.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				names[n] = true
			}
		}
	}
	if !names["online_tool"] {
		t.Errorf("expected online_tool in tools/list, got %v", names)
	}
}

// -----------------------------------------------------------------------------
// Test 9: Tasker online discovery fallback — when mcp_list_tools returns 404,
// the CLI falls back to the local --tools JSON file.
// -----------------------------------------------------------------------------

func TestE2E_TaskerOnlineFallbackToFile(t *testing.T) {
	fileTools := []TaskerTool{
		{TaskerName: "Task Y", Name: "file_tool_y", Description: "from file", InputSchema: minimalSchema("msg")},
	}
	taskerHandler := func(w http.ResponseWriter, r *http.Request) {
		bb, _ := io.ReadAll(r.Body)
		if strings.Contains(string(bb), `"name":"mcp_list_tools"`) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		t.Errorf("unexpected tasker call body=%q", string(bb))
		http.Error(w, "no", http.StatusInternalServerError)
	}

	mcpURL, _, _ := startTestServer(t, taskerHandler, fileTools, false)
	sid := initSession(t, mcpURL)

	resp, _, raw := mcpCall(t, mcpURL,
		map[string]string{"Mcp-Session-Id": sid},
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if resp == nil || resp.Result == nil {
		t.Fatalf("tools/list: no result (raw=%q)", raw)
	}
	listed, _ := resp.Result["tools"].([]any)
	if len(listed) != 1 {
		t.Fatalf("expected 1 file tool, got %d (raw=%q)", len(listed), raw)
	}
	names := map[string]bool{}
	for _, item := range listed {
		if m, ok := item.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				names[n] = true
			}
		}
	}
	if !names["file_tool_y"] {
		t.Errorf("expected file_tool_y in tools/list, got %v", names)
	}
}

// -----------------------------------------------------------------------------
// Test 10: Both online and file unavailable — tryLoadTools surfaces an error.
// We point Tasker at a refusing address and the file path to a nonexistent
// file, and assert tryLoadTools returns a non-nil error.
// -----------------------------------------------------------------------------

func TestE2E_TaskerOnlineBothFail(t *testing.T) {
	prevHost, prevPort, prevKey, prevTimeout := taskerHost, taskerPort, taskerApiKey, taskerTimeoutDur
	// Point Tasker at a port that should refuse connections quickly.
	taskerHost = "127.0.0.1"
	taskerPort = "1"
	taskerApiKey = "test-key"
	taskerTimeoutDur = 500 * time.Millisecond
	t.Cleanup(func() {
		taskerHost, taskerPort, taskerApiKey, taskerTimeoutDur = prevHost, prevPort, prevKey, prevTimeout
	})

	tools, source, err := tryLoadTools(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err == nil {
		t.Fatalf("expected error when both online and file fail; got tools=%v source=%q", tools, source)
	}
	if source != "" {
		t.Errorf("expected empty source on total failure, got %q", source)
	}
	if tools != nil {
		t.Errorf("expected nil tools on total failure, got %v", tools)
	}
}

// TestE2E_NoToolsFlagOnlineOnly — when --tools is empty and Tasker online
// discovery succeeds, tryLoadTools returns the online list with no warning
// path. This is the "no fallback configured" happy case.
func TestE2E_NoToolsFlagOnlineOnly(t *testing.T) {
	taskerMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"name":"mcp_list_tools"`) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"tasker_name":"X","name":"online_only_tool","description":"d","inputSchema":{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}}]`))
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	t.Cleanup(taskerMock.Close)

	host, port, _ := strings.Cut(strings.TrimPrefix(taskerMock.URL, "http://"), ":")
	prevHost, prevPort, prevKey, prevTimeout := taskerHost, taskerPort, taskerApiKey, taskerTimeoutDur
	taskerHost = host
	taskerPort = port
	taskerApiKey = "test-key"
	taskerTimeoutDur = 5 * time.Second
	t.Cleanup(func() {
		taskerHost, taskerPort, taskerApiKey, taskerTimeoutDur = prevHost, prevPort, prevKey, prevTimeout
	})

	tools, source, err := tryLoadTools("")
	if err != nil {
		t.Fatalf("tryLoadTools with empty path + online available: %v", err)
	}
	if source != "tasker" {
		t.Errorf("source = %q, want tasker", source)
	}
	if len(tools) != 1 || tools[0].Name != "online_only_tool" {
		t.Errorf("unexpected tools: %+v", tools)
	}
}

// TestE2E_NoToolsFlagOnlineFails — when --tools is empty and Tasker is
// unreachable, tryLoadTools returns an error (no silent file fallback).
func TestE2E_NoToolsFlagOnlineFails(t *testing.T) {
	prevHost, prevPort, prevKey, prevTimeout := taskerHost, taskerPort, taskerApiKey, taskerTimeoutDur
	taskerHost = "127.0.0.1"
	taskerPort = "1" // refused
	taskerApiKey = ""
	taskerTimeoutDur = 500 * time.Millisecond
	t.Cleanup(func() {
		taskerHost, taskerPort, taskerApiKey, taskerTimeoutDur = prevHost, prevPort, prevKey, prevTimeout
	})

	tools, source, err := tryLoadTools("")
	if err == nil {
		t.Fatalf("expected error when --tools empty and online down; got tools=%v source=%q", tools, source)
	}
	if !strings.Contains(err.Error(), "--tools not set") {
		t.Errorf("expected error mentioning --tools not set, got: %v", err)
	}
	if tools != nil || source != "" {
		t.Errorf("expected zero values on failure, got tools=%v source=%q", tools, source)
	}
}
