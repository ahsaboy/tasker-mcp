# Tasker MCP

This document will guide you through setting up and running the Tasker MCP integration, including instructions for installing dependencies, preparing servers, and updating tasks.

---

## Usage Guide

### Step 1: Import the Tasker Profile

- Import `dist/mcp_server.prj.xml` into your Tasker app.
- After importing, run the `MCP generate_api_key` task to generate an API key for secure access.

### Step 2: Select and Run Your Server

**CLI Server:**

- Download the binary for your device's architecture from the latest [GitHub Release](https://github.com/ahsaboy/tasker-mcp/releases), or build it yourself (see "Building the CLI Server Yourself" below).
- Copy both the binary and the `toolDescriptions.json` file to your device (phone or PC).
- Rename the binary to `mcp-server` after copying.

**Example:**

Using `scp`:

```bash
scp ./tasker-mcp-server-cli-aarch64 user@phone_ip:/data/data/com.termux/files/home/mcp-server
```

Using `adb push`:

```bash
adb push ./tasker-mcp-server-cli-aarch64 /data/data/com.termux/files/home/mcp-server
```

- Run the server in recommended **HTTP mode** with:

```bash
./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_...
```

- Header auth is optional and disabled by default. It only turns on when `--auth` parses to a non-empty `Name:Value`, so existing network deployments are unchanged unless you opt in.

- Run HTTP mode with header auth enabled:

```bash
./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_... --auth X-Tasker-Token:your-secret
```

- HTTP mode request examples (the `/mcp` endpoint requires `Accept: application/json, text/event-stream`):

```bash
# Missing header -> 401
curl -i -X POST http://127.0.0.1:8000/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

# Correct header -> MCP response
curl -i -X POST http://127.0.0.1:8000/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'X-Tasker-Token: your-secret' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

# Health endpoint stays public
curl -i http://127.0.0.1:8000/healthz
```

- `stdio` transport is unaffected by header auth (you must pass `--mode stdio` now since the default is `streamable-http`):

```bash
payload='{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": { "name": "tasker_flash_text", "arguments": { "text": "Hi" }  } }'
echo $payload | ./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_... --mode stdio
```

- Streamable HTTP is the recommended remote transport (MCP specification 2025-11-25):

```bash
./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_...
```

- Streamable HTTP supports bidirectional communication on the fixed `/mcp` endpoint:
  - Clients use POST to send requests and receive immediate responses
  - Clients use GET to establish long-lived connections for receiving server-pushed notifications
  - Uses `Mcp-Session-Id` header for session management

#### Command-Line Flags

The `tasker-mcp-server-cli` application accepts the following flags:

- `--tools`: Path to JSON file with Tasker tool definitions. **Optional**: if omitted, the CLI requires Tasker online discovery via `mcp_list_tools` (see below) and exits if unavailable. If both `--tools` and online discovery are configured, online takes precedence and `--tools` is used as a fallback.
- `--host`: Host address to listen on (default: `0.0.0.0`).
- `--port`: Port to listen on (default: `8000`).
- `--mode`: Transport mode: `streamable-http` or `stdio` (default: `streamable-http`).
- `--tasker-host`: Tasker server host (default: `127.0.0.1`).
- `--tasker-port`: Tasker server port (default: `1821`).
- `--tasker-api-key`: The Tasker API Key.
- `--tasker-timeout`: HTTP timeout when calling Tasker (default: `30s`).
- `--auth`: Optional header auth, format `"Name:Value"` (e.g. `"X-Tasker-Token:secret"`). Empty disables auth.
- `--version`: Print version information and exit.

**Header auth behavior:**
- Header auth is disabled by default; it is enabled only when `--auth` parses to a non-empty `Name:Value`.
- Malformed `--auth` (missing colon or empty value) logs a warning and keeps auth disabled.
- Protection scope is the `/mcp` endpoint only.
- `/healthz` remains public.
- `--mode stdio` is unaffected.

#### Or run via Docker

Pull the multi-arch image from GitHub Container Registry (supports `linux/amd64` and `linux/arm64`):

```bash
docker run --rm -p 8000:8000 \
  -v $(pwd)/toolDescriptions.json:/etc/tasker-mcp/toolDescriptions.json:ro \
  ghcr.io/ahsaboy/tasker-mcp:latest \
  --tools /etc/tasker-mcp/toolDescriptions.json \
  --tasker-host host.docker.internal \
  --tasker-api-key tk_...
```

A new image is published automatically when a `v*` tag is pushed.

### Step 3: Connect Your MCP-enabled App

- Connect your MCP-enabled application by pointing it to the running server.

#### Example Configuration for Claude Desktop with stdio transport

```json
{
  "mcpServers": {
    "tasker": {
      "command": "/home/luis/tasker-mcp/dist/tasker-mcp-server-cli-x86_64",
      "args": [
        "--tools",
        "/home/luis/tasker-mcp/dist/toolDescriptions.json",
        "--tasker-host",
        "192.168.1.123",
        "--tasker-api-key",
        "tk_...",
        "--mode",
        "stdio"
      ]
    }
  }
}
```

---

## Building the CLI Server Yourself

### Unix/Linux:

- Install Go using your package manager:

```bash
sudo apt-get install golang-go
```

- Build the CLI server (cross-compiling example for ARM64):

```bash
cd cli
GOOS=linux GOARCH=arm64 go build -o dist/tasker-mcp-server-cli-aarch64 main.go
```

---

## Updating the MCP Profile with Additional Tasks

Due to limitations in Tasker's argument handling, follow these steps carefully to mark tasks as MCP-enabled:

### Step 1: Set Task Comment

- Add a comment directly in the task settings. This comment becomes the tool description.

### Step 2: Configure Tool Arguments Using Task Variables

Tasker supports only two positional arguments (`par1`, `par2`). To work around this, we'll use Task Variables:

- **A TaskVariable becomes an MCP argument if:**
  1. **Configure on Import**: unchecked
  2. **Immutable**: true
  3. **Value**: empty

After setting the above values you can also set some additional metadata:&#x20;

- **Metadata mapping:**
  - **Type**: Derived from Task Variable's type (`number`, `string`, `onoff`, etc).
  - **Description**: Set via the variable's `Prompt` field.
  - **Required**: If the `Same as Value` field is checked.

**Note:** Temporarily enable "Configure on Import" to set the Prompt description if hidden, then disable it again. The prompt will survive.\


These steps will make sure valid tool descriptions can be generated when we export our custom project later.\
&#x20;Task Variables cannot be pass-through from other tasks, though, so we need to do one last thing in order to get all the variables from the MCP request properly set.

### Step 3: Copy the special action

Copy the action `MCP#parse_args` to the top of your MCP task to enable argument parsing. You can get this from any of the default tasks. But do not modify this action!

### Step 4: Exporting and Generating Updated Tool Descriptions

Now your custom tasks are ready:

- Export your `mcp-server` project and save it on your PC.
- Ensure Node.js is installed, then run:

```bash
cd utils
npm install
node xml-to-tools.js /path/to/your/exported/mcp_server.prj.xml > toolDescriptions.json
```

Use this `toolDescriptions.json` file with your server.

### Optional: skip the offline regeneration step

If you add an `mcp_list_tools` task to your Tasker project that returns the current tool table as a JSON array, the CLI will fetch it automatically at startup (and on every SIGHUP / mtime reload) and fall back to the `--tools` file only on failure. After editing your tools, simply `kill -HUP <pid>` (Linux) or restart the CLI — no more XML export + `xml-to-tools.js` round-trip.

See [`docs/online-tool-discovery.md`](docs/online-tool-discovery.md) for the full protocol, Tasker-side implementation guide, and troubleshooting tips.

---

## Client configuration examples

Sample MCP client configs are in `examples/`:

| Client | File | Transport |
|---|---|---|
| Claude Desktop (stdio) | `examples/claude_desktop_config.json` | stdio |
| Claude Desktop (HTTP) | `examples/claude_desktop_streamable_http.json` | streamable-http |
| Cursor | `examples/cursor_mcp.json` | streamable-http |
| Windsurf | `examples/windsurf_mcp_config.json` | streamable-http |
| VSCode Continue | `examples/vscode_continue.json` | streamable-http |

For HTTP-based clients, point the URL at your running `tasker-mcp` server (default `http://127.0.0.1:8000/mcp`). For stdio, the binary is launched as a child process — adjust `command` / `args` for your install path.

If you enabled `--auth Name:Value`, your HTTP client must support custom request headers; some clients currently do not, in which case leave `--auth` empty and expose the server only on a trusted network.

---

Happy automation!

