package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/dceluis/mcp-go/mcp"
	"github.com/dceluis/mcp-go/server"
)

// Global variables for Tasker server host and port.
var toolsPath string
var taskerHost string
var taskerPort string
var taskerApiKey string

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
		// Log the tool call.
		log.Printf("Tool called: %s with args: %+v", tool.Name, args)
		// Execute the Tasker task.
		result, err := runTaskerTask(tool.TaskerName, args)
		if err != nil {
			return nil, err
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
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
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

func NewMCPServer() *server.MCPServer {
	mcpServer := server.NewMCPServer(
		"tasker-mcp-server",
		"1.0.0",
		server.WithLogging(),
	)

	taskerTools, err := loadToolsFromFile(toolsPath)
	if err != nil {
		log.Fatalf("Failed to load tools from file: %v", err)
	}

	for _, tool := range taskerTools {
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
		mcpServer.AddTool(toolObj, handler)
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

func startHTTPServer(mcpServer *server.MCPServer, host, port, mcpPath, healthPath, authHeaderName, authHeaderValue string) error {
	mcpPath = normalizePath(mcpPath)
	healthPath = normalizePath(healthPath)
	if mcpPath == "" {
		return fmt.Errorf("mcp path cannot be empty")
	}

	authEnabled := isHeaderAuthEnabled(authHeaderName, authHeaderValue)
	mux := http.NewServeMux()

	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Allow", "POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "Method not allowed")
			return
		}

		sessionID := r.Header.Get("Mcp-Session-Id")
		if sessionID == "" {
			sessionID = r.URL.Query().Get("sessionId")
		}
		if sessionID == "" {
			sessionID = "http"
		}

		ctx := mcpServer.WithContext(r.Context(), server.NotificationContext{
			ClientID:  sessionID,
			SessionID: sessionID,
		})

		var rawMessage json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&rawMessage); err != nil {
			writeJSONRPCError(w, nil, mcp.PARSE_ERROR, "Parse error")
			return
		}

		response := mcpServer.HandleMessage(ctx, rawMessage)
		if response == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("Failed to encode MCP response: %v", err)
		}
	})

	mux.Handle(mcpPath, withHeaderAuth(mcpHandler, authHeaderName, authHeaderValue))

	if healthPath != "" {
		mux.HandleFunc(healthPath, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		})
	}

	addr := fmt.Sprintf("%s:%s", host, port)
	log.Printf("Starting HTTP MCP server on %s (mcp=%s, health=%s)", addr, mcpPath, healthPath)
	if authEnabled {
		log.Printf("HTTP mode header auth enabled for %s using header %q; health endpoint remains public", mcpPath, authHeaderName)
	} else {
		log.Printf("HTTP mode header auth disabled; network MCP endpoint %s is running in compatibility mode and %s remains public", mcpPath, healthPath)
	}
	return http.ListenAndServe(addr, mux)
}

func main() {
	toolsPathFlag := flag.String("tools", "", "Path to JSON file with Tasker tool definitions")
	host := flag.String("host", "0.0.0.0", "Host address to listen on for network server (default: 0.0.0.0)")
	port := flag.String("port", "8000", "Port to listen on for network server (default: 8000)")
	mode := flag.String("mode", "stdio", "Transport mode: http, streamable-http, sse, or stdio (default: stdio)")
	mcpPath := flag.String("mcp-path", "/mcp", "Path for HTTP MCP endpoint (default: /mcp)")
	healthPath := flag.String("health-path", "/healthz", "Path for HTTP health endpoint (default: /healthz)")
	authHeaderName := flag.String("auth-header-name", "", "Header name required for MCP/SSE authentication (example: X-Tasker-Token)")
	authHeaderValue := flag.String("auth-header-value", "", "Expected header value for MCP/SSE authentication")
	taskerHostFlag := flag.String("tasker-host", "0.0.0.0", "Tasker server host (default: 0.0.0.0)")
	taskerPortFlag := flag.String("tasker-port", "1821", "Tasker server port (default: 1821)")
	taskerApiKeyFlag := flag.String("tasker-api-key", "", "Tasker API Key")
	flag.Parse()

	// Set the global Tasker server variables.
	taskerHost = *taskerHostFlag
	taskerPort = *taskerPortFlag
	taskerApiKey = *taskerApiKeyFlag
	toolsPath = *toolsPathFlag

	authEnabled := isHeaderAuthEnabled(*authHeaderName, *authHeaderValue)
	if !authEnabled && (strings.TrimSpace(*authHeaderName) != "" || *authHeaderValue != "") {
		configuredFlag := "--auth-header-name"
		if strings.TrimSpace(*authHeaderName) != "" {
			configuredFlag = "--auth-header-value"
		}
		log.Printf("Warning: header auth requires both --auth-header-name and --auth-header-value; %s is missing, so header auth is disabled", configuredFlag)
	}

	if toolsPath == "" {
		log.Fatal("Please provide the -tools flag with the path to the JSON file containing tool definitions")
	}

	// Instantiate the MCP server using the mcp-go API.
	mcpServer := NewMCPServer()

	switch *mode {
	case "http", "streamable-http":
		if err := startHTTPServer(mcpServer, *host, *port, *mcpPath, *healthPath, *authHeaderName, *authHeaderValue); err != nil {
			log.Fatalf("HTTP server error: %v", err)
		}
	case "sse":
		log.Printf("SSE mode is kept for compatibility. Please migrate clients to --mode http (HTTP mode).")
		addr := fmt.Sprintf("%s:%s", *host, *port)
		sseServer := server.NewSSEServer(mcpServer)
		mux := http.NewServeMux()
		protectedSSEHandler := withHeaderAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sseServer.ServeHTTP(w, r)
		}), *authHeaderName, *authHeaderValue)
		mux.Handle("/sse", protectedSSEHandler)
		mux.Handle("/message", protectedSSEHandler)
		if authEnabled {
			log.Printf("SSE compatibility mode header auth enabled for /sse and /message using header %q", *authHeaderName)
		} else {
			log.Printf("SSE compatibility mode header auth disabled; /sse and /message are running in compatibility mode")
		}
		log.Printf("Starting SSE server on %s...", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatalf("SSE server error: %v", err)
		}
	case "stdio":
		if err := server.ServeStdio(mcpServer); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	default:
		log.Fatalf("Unknown transport mode: %s", *mode)
	}
}
