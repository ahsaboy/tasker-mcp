# Tasker MCP

This document will guide you through setting up and running the Tasker MCP integration, including instructions for installing dependencies, preparing servers, and updating tasks.

---

## Usage Guide

### Step 1: Import the Tasker Profile

- Import `dist/mcp_server.prj.xml` into your Tasker app.
- After importing, run the `MCP generate_api_key` task to generate an API key for secure access.

### Step 2: Select and Run Your Server

**CLI Server:**

- From the `dist/` folder, select the correct CLI server binary for your device's architecture, such as `tasker-mcp-server-cli-aarch64`.
- Copy both the binary and the `toolDescriptions.json` file to your device (phone or PC).
- Rename the binary to `mcp-server` after copying.

**Example:**

Using `scp`:

```bash
scp dist/tasker-mcp-server-cli-aarch64 user@phone_ip:/data/data/com.termux/files/home/mcp-server
```

Using `adb push`:

```bash
adb push dist/tasker-mcp-server-cli-aarch64 /data/data/com.termux/files/home/mcp-server
```

- Run the server in recommended **HTTP mode** with:

```bash
./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_... --mode http --mcp-path /mcp --health-path /healthz
```

- Header auth is optional and disabled by default. It only turns on when both `--auth-header-name` and `--auth-header-value` are provided, so existing network deployments are unchanged unless you opt in.

- Run HTTP mode with header auth enabled:

```bash
./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_... --mode http --mcp-path /mcp --health-path /healthz --auth-header-name X-Tasker-Token --auth-header-value your-secret
```

- HTTP mode request examples:

```bash
# Missing header -> 401
curl -i -X POST http://127.0.0.1:8000/mcp -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

# Correct header -> MCP response
curl -i -X POST http://127.0.0.1:8000/mcp -H 'Content-Type: application/json' -H 'X-Tasker-Token: your-secret' -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

# Health endpoint stays public
curl -i http://127.0.0.1:8000/healthz
```

- `stdio` transport is unaffected by header auth:

```bash
payload='{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": { "name": "tasker_flash_text", "arguments": { "text": "Hi" }  } }'
echo $payload | ./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_...
```

- SSE is still available as a compatibility mode during migration, but HTTP mode is the recommended transport:

```bash
./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_... --mode sse
```

#### Command-Line Flags

The `tasker-mcp-server-cli` application accepts the following flags:

- `--tools`: Path to JSON file with Tasker tool definitions.
- `--host`: Host address to listen on for network server (default: `0.0.0.0`).
- `--port`: Port to listen on for network server (default: `8000`).
- `--mode`: Transport mode: `http`, `streamable-http`, `sse`, or `stdio` (default: `stdio`).
- `--mcp-path`: HTTP MCP endpoint path in HTTP mode (default: `/mcp`).
- `--health-path`: HTTP health endpoint path in HTTP mode (default: `/healthz`).
- `--tasker-host`: Tasker server host (default: `0.0.0.0`).
- `--tasker-port`: Tasker server port (default: `1821`).
- `--tasker-api-key`: The Tasker API Key.
- `--auth-header-name`: Header name required to access network MCP endpoints. Example: `X-Tasker-Token`.
- `--auth-header-value`: Expected value for `--auth-header-name`.

**Header auth behavior:**
- Header auth is disabled by default.
- Header auth is enabled only when both `--auth-header-name` and `--auth-header-value` are non-empty.
- If only one auth flag is set, auth stays disabled and the server logs a warning.
- Protection scope is only network MCP endpoints (`--mcp-path` in HTTP mode, `/sse` and `/message` in SSE compatibility mode).
- Health endpoint (`--health-path`) remains public.
- `--mode stdio` is unaffected.

### Step 3: Connect Your MCP-enabled App

- Connect your MCP-enabled application by pointing it to the running server.

#### HTTP migration note (SSE -> HTTP)

If you currently run SSE (`--mode sse`), migrate to HTTP by switching startup flags:

```bash
# Before (compatibility mode)
./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_... --mode sse --host 0.0.0.0 --port 8000

# After (recommended)
./mcp-server --tools /path/to/toolDescriptions.json --tasker-api-key=tk_... --mode http --host 0.0.0.0 --port 8000 --mcp-path /mcp --health-path /healthz
```

Then connect MCP clients to the HTTP endpoint path (`/mcp` by default).

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

---

Happy automation!

