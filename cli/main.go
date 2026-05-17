package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Online tool discovery protocol: on startup (and on every SIGHUP / mtime
// reload) the CLI asks the Tasker side for a live tool list by invoking the
// `mcp_list_tools` task via POST /run_task, falling back to the --tools JSON
// file on any failure. See docs/online-tool-discovery.md for the full contract.

var version = "dev" // overridden by -ldflags "-X main.version=..."

// Global variables for Tasker server host and port.
var toolsPath string
var taskerHost string
var taskerPort string
var taskerApiKey string
var taskerTimeoutDur time.Duration

// structuredCtxKey is a typed context key set by the HTTP middleware when the
// caller opts into Structured Tool Results via the X-Tasker-Structured header.
// stdio transport never sets this, so structured output is HTTP-only.
type structuredCtxKey struct{}

// GenericMap is a new type for tool arguments.
type GenericMap map[string]interface{}

// TaskerTool defines the structure for a tool loaded from JSON.
type TaskerTool struct {
	TaskerName  string                 `json:"tasker_name"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// genericToolHandler returns a tool handler function for a given Tasker tool.
func genericToolHandler(tool TaskerTool) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments
		if args == nil {
			return mcp.NewToolResultError("Arguments must be provided"), nil
		}
		argsMap, ok := args.(map[string]interface{})
		if !ok {
			return mcp.NewToolResultError("Arguments must be an object"), nil
		}
		// Log the tool call.
		// TODO: pull session id from ctx when mcp-go v0.49 exposes a stable accessor.
		slog.Info("tool called", "name", tool.Name, "args", argsMap)
		// Execute the Tasker task.
		result, err := runTaskerTask(tool.TaskerName, argsMap)
		if err != nil {
			return mcp.NewToolResultErrorf("tasker call failed: %v", err), nil
		}
		// Opt-in Structured Tool Result: caller sets X-Tasker-Structured: true|1
		// on the HTTP request, the middleware writes a flag into ctx, and we
		// attempt to parse the Tasker response body as JSON. On success we
		// return structured content (with the raw text as fallback). On any
		// failure (flag unset, stdio, body not JSON-shaped, parse error) we
		// fall through to plain text — fully backward compatible.
		if flag, _ := ctx.Value(structuredCtxKey{}).(bool); flag {
			trimmed := strings.TrimSpace(result)
			if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
				var parsed any
				if jsonErr := json.Unmarshal([]byte(trimmed), &parsed); jsonErr == nil {
					slog.Info("returning structured result", "name", tool.Name)
					return mcp.NewToolResultStructured(parsed, result), nil
				}
				slog.Debug("structured opted in but body not valid JSON", "name", tool.Name)
			}
		}
		// Return the result using the new result constructor.
		return mcp.NewToolResultText(result), nil
	}
}

// runTaskerTask sends an HTTP POST to the Tasker endpoint to execute the task.
func runTaskerTask(taskerName string, args map[string]interface{}) (string, error) {
	payload := map[string]interface{}{
		"name":      taskerName,
		"arguments": args,
	}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	// Build the URL using the specified taskerHost and taskerPort.
	taskerURL := fmt.Sprintf("http://%s:%s/run_task", taskerHost, taskerPort)
	req, err := http.NewRequest("POST", taskerURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if taskerApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+taskerApiKey)
	}
	client := &http.Client{Timeout: taskerTimeoutDur}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		slog.Warn("tasker call failed", "url", taskerURL, "status", resp.StatusCode)
		return "", fmt.Errorf("HTTP error: %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(bodyBytes), nil
}

// loadToolsFromFile reads and unmarshals the JSON file containing tool definitions.
func loadToolsFromFile(filePath string) ([]TaskerTool, error) {
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	var tools []TaskerTool
	if err := json.Unmarshal(fileBytes, &tools); err != nil {
		return nil, err
	}
	return tools, nil
}

// loadToolsFromTasker queries the Tasker side for a live tool list by invoking
// the `mcp_list_tools` task and parsing its response body as a TaskerTool
// array. Returns a sentinel error if the task is missing, the body is not a
// JSON array, or the array is empty — callers should fall back to file.
func loadToolsFromTasker(host, port, apiKey string, timeout time.Duration) ([]TaskerTool, error) {
	payload := map[string]interface{}{"name": "mcp_list_tools", "arguments": map[string]interface{}{}}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://%s:%s/run_task", host, port)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tasker mcp_list_tools returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var tools []TaskerTool
	if err := json.Unmarshal(body, &tools); err != nil {
		return nil, fmt.Errorf("tasker mcp_list_tools body not a TaskerTool array: %w", err)
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("tasker mcp_list_tools returned empty array")
	}
	return tools, nil
}

// tryLoadTools first asks Tasker for an online tool list via mcp_list_tools.
// On any failure it falls back to the local toolDescriptions.json. The second
// return value describes which source was used ("tasker" or "file") so it can
// be logged.
func tryLoadTools(localPath string) ([]TaskerTool, string, error) {
	if tools, err := loadToolsFromTasker(taskerHost, taskerPort, taskerApiKey, 5*time.Second); err == nil {
		return tools, "tasker", nil
	} else {
		slog.Warn("tasker online discovery unavailable, falling back to file", "err", err)
	}
	tools, err := loadToolsFromFile(localPath)
	if err != nil {
		return nil, "", err
	}
	return tools, "file", nil
}

// buildServerTools translates loaded TaskerTool entries into mcp-go ServerTool
// values (each containing an mcp.Tool description and its handler).
func buildServerTools(tools []TaskerTool) []server.ServerTool {
	result := make([]server.ServerTool, 0, len(tools))
	for _, tool := range tools {
		inputSchema := tool.InputSchema

		var opts []mcp.ToolOption
		if inputSchema != nil {
			var required []string
			if req, ok := inputSchema["required"].([]interface{}); ok {
				for _, r := range req {
					if str, ok := r.(string); ok {
						required = append(required, str)
					}
				}
			}

			if props, ok := inputSchema["properties"].(map[string]interface{}); ok {
				for key, propRaw := range props {
					if prop, ok := propRaw.(map[string]interface{}); ok {
						desc := ""
						if d, ok := prop["description"].(string); ok {
							desc = d
						}

						var propOpts []mcp.PropertyOption
						for _, reqKey := range required {
							if reqKey == key {
								propOpts = append(propOpts, mcp.Required())
								break
							}
						}
						if desc != "" {
							propOpts = append(propOpts, mcp.Description(desc))
						}

						switch t := prop["type"].(string); t {
						case "string":
							opts = append(opts, mcp.WithString(key, propOpts...))
						case "number":
							opts = append(opts, mcp.WithNumber(key, propOpts...))
						default:
							opts = append(opts, mcp.WithString(key, propOpts...))
						}
					}
				}
			}
		}

		allOpts := append([]mcp.ToolOption{mcp.WithDescription(tool.Description)}, opts...)
		toolObj := mcp.NewTool(tool.Name, allOpts...)
		handler := genericToolHandler(tool)
		result = append(result, server.ServerTool{Tool: toolObj, Handler: handler})
	}
	return result
}

// reloadTools reads the tools file from disk, builds the mcp ServerTool slice,
// and atomically swaps it into the running MCPServer. On failure the existing
// tool table is left untouched.
func reloadTools(s *server.MCPServer, path string) (int, error) {
	tools, source, err := tryLoadTools(path)
	if err != nil {
		return 0, err
	}
	serverTools := buildServerTools(tools)
	s.SetTools(serverTools...)
	slog.Info("tools source", "source", source, "count", len(serverTools))
	return len(serverTools), nil
}

// watchToolsFile polls the tools JSON file on disk and triggers a reload
// whenever its modification time changes. Errors are logged; the previous
// tool table remains active until a successful reload.
func watchToolsFile(ctx context.Context, s *server.MCPServer, path string) {
	var lastMod time.Time
	if st, err := os.Stat(path); err == nil {
		lastMod = st.ModTime()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st, err := os.Stat(path)
			if err != nil {
				slog.Warn("tools file stat failed", "err", err)
				continue
			}
			if st.ModTime().Equal(lastMod) {
				continue
			}
			lastMod = st.ModTime()
			n, err := reloadTools(s, path)
			if err != nil {
				slog.Error("reload failed", "err", err)
				continue
			}
			slog.Info("tools reloaded", "count", n, "trigger", "mtime")
		}
	}
}

func NewMCPServer() *server.MCPServer {
	mcpServer := server.NewMCPServer(
		"tasker-mcp-server",
		version,
		server.WithLogging(),
		server.WithToolCapabilities(true),
	)

	_, err := reloadTools(mcpServer, toolsPath)
	if err != nil {
		slog.Error("failed to load tools", "err", err)
		os.Exit(1)
	}
	return mcpServer
}

func normalizePath(path string) string {
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func isHeaderAuthEnabled(name, value string) bool {
	return strings.TrimSpace(name) != "" && value != ""
}

func withHeaderAuth(next http.Handler, name, value string) http.Handler {
	if !isHeaderAuthEnabled(name, value) {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(name) != value {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withStructuredFlag inspects the X-Tasker-Structured request header and, when
// the caller opts in (value "true" or "1", case-insensitive), writes a typed
// flag into the request context so that genericToolHandler can produce a
// Structured Tool Result instead of plain text. Always wraps; opt-in is purely
// a per-request header decision.
func withStructuredFlag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Tasker-Structured"))); v == "true" || v == "1" {
			r = r.WithContext(context.WithValue(r.Context(), structuredCtxKey{}, true))
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSONRPCError(w http.ResponseWriter, id interface{}, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	})
}

// buildHTTPMux constructs the MCP + healthz mux with the standard middleware
// chain. Exposed so tests can run the server on an arbitrary listener.
func buildHTTPMux(mcpServer *server.MCPServer, authHeaderName, authHeaderValue string) *http.ServeMux {
	streamable := server.NewStreamableHTTPServer(mcpServer, server.WithEndpointPath("/mcp"))
	mux := http.NewServeMux()
	var handler http.Handler = streamable
	if isHeaderAuthEnabled(authHeaderName, authHeaderValue) {
		handler = withHeaderAuth(handler, authHeaderName, authHeaderValue)
	}
	// Wrap with structured-flag middleware as the outermost layer so the ctx
	// key is set regardless of whether auth is enabled. (Unauthorized requests
	// never reach the tool handler anyway, so leaking the flag is harmless.)
	handler = withStructuredFlag(handler)
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func startHTTPServer(mcpServer *server.MCPServer, host, port, authHeaderName, authHeaderValue string, taskerTimeout time.Duration) error {
	// taskerTimeout is consumed via the package-level taskerTimeoutDur set in main();
	// this signature is kept for compatibility with the call sites.
	_ = taskerTimeout
	mux := buildHTTPMux(mcpServer, authHeaderName, authHeaderValue)
	addr := fmt.Sprintf("%s:%s", host, port)
	slog.Info("starting streamable-http server", "addr", addr, "endpoint", "/mcp", "health", "/healthz")
	if isHeaderAuthEnabled(authHeaderName, authHeaderValue) {
		slog.Info("header auth enabled on /mcp", "header", authHeaderName)
	}
	return http.ListenAndServe(addr, mux)
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	showVersion := flag.Bool("version", false, "Print version information and exit")
	toolsPathFlag := flag.String("tools", "", "Path to JSON file with Tasker tool definitions (required)")
	host := flag.String("host", "0.0.0.0", "Host address to listen on for network server")
	port := flag.String("port", "8000", "Port to listen on for network server")
	mode := flag.String("mode", "streamable-http", "Transport mode: streamable-http or stdio")
	taskerHostFlag := flag.String("tasker-host", "127.0.0.1", "Tasker server host")
	taskerPortFlag := flag.String("tasker-port", "1821", "Tasker server port")
	taskerApiKeyFlag := flag.String("tasker-api-key", "", "Tasker API Key")
	taskerTimeout := flag.Duration("tasker-timeout", 30*time.Second, "HTTP timeout when calling Tasker")
	authFlag := flag.String("auth", "", `Optional header auth in "Name:Value" form (empty disables)`)
	flag.Parse()

	// Print version and exit if requested
	if *showVersion {
		fmt.Printf("tasker-mcp version %s\n", version)
		fmt.Println("Repository: https://github.com/ahsaboy/tasker-mcp")
		return
	}

	// Set the global Tasker server variables.
	taskerHost = *taskerHostFlag
	taskerPort = *taskerPortFlag
	taskerApiKey = *taskerApiKeyFlag
	toolsPath = *toolsPathFlag
	taskerTimeoutDur = *taskerTimeout

	var authName, authValue string
	if s := strings.TrimSpace(*authFlag); s != "" {
		if i := strings.Index(s, ":"); i > 0 && i < len(s)-1 {
			authName = strings.TrimSpace(s[:i])
			authValue = strings.TrimSpace(s[i+1:])
		} else {
			slog.Warn("--auth malformed, disabled", "value", *authFlag)
		}
	}

	if toolsPath == "" {
		slog.Error("missing required flag", "flag", "-tools")
		os.Exit(1)
	}

	// Instantiate the MCP server using the mcp-go API.
	mcpServer := NewMCPServer()

	// Spin up watchers for hot-reload of the tools file. Both stdio and
	// streamable-http modes benefit from this.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchToolsFile(ctx, mcpServer, toolsPath)
	go watchReloadSignal(ctx, mcpServer, toolsPath)

	switch *mode {
	case "streamable-http":
		if err := startHTTPServer(mcpServer, *host, *port, authName, authValue, *taskerTimeout); err != nil {
			slog.Error("streamable http server error", "err", err)
			os.Exit(1)
		}
	case "http":
		slog.Warn("--mode http is deprecated, use streamable-http")
		if err := startHTTPServer(mcpServer, *host, *port, authName, authValue, *taskerTimeout); err != nil {
			slog.Error("streamable http server error", "err", err)
			os.Exit(1)
		}
	case "stdio":
		if err := server.ServeStdio(mcpServer); err != nil {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	default:
		slog.Error("unknown transport mode", "mode", *mode, "supported", "streamable-http, stdio")
		os.Exit(1)
	}
}
