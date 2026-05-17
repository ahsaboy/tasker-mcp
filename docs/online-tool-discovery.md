# Online Tool Discovery Protocol

> Applies to `tasker-mcp` v1.4.0+

CLI 启动时（以及每次 SIGHUP / mtime 触发热重载时），优先向 Tasker 端要一份"在线"工具表，失败再 fallback 到本地 `--tools` 文件。这样改完 Tasker 工程后**不再**需要导出 XML、跑 `xml-to-tools.js` 重新生成 `toolDescriptions.json`，只要在 Tasker 那一侧改完，`kill -HUP <pid>`（Linux）或重启 CLI（Windows）即可生效。

零新 CLI flag。本协议**完全可选**——不实现也不会影响其它功能。

---

## 协议契约

### 请求

CLI 用与普通工具调用一样的 `POST /run_task` 端点，向 Tasker 发：

```http
POST /run_task HTTP/1.1
Host: <tasker-host>:<tasker-port>
Content-Type: application/json
Authorization: Bearer <tasker-api-key>

{"name":"mcp_list_tools","arguments":{}}
```

- 超时：**5 秒**（写死，独立于 `--tasker-timeout`，因为这是启动期发现）
- 鉴权头：当 `--tasker-api-key` 非空时附带

### 期望响应

```http
HTTP/1.1 200 OK
Content-Type: application/json

[
  {
    "tasker_name": "Send SMS",
    "name": "tasker_send_sms",
    "description": "Send an SMS via Tasker.",
    "inputSchema": {
      "type": "object",
      "properties": {
        "number": {"type": "string", "description": "Recipient phone number"},
        "text":   {"type": "string", "description": "Message body"}
      },
      "required": ["number", "text"]
    }
  },
  ...
]
```

**body schema 与 `toolDescriptions.json` 完全一致**——`xml-to-tools.js` 的输出格式即可。

### 失败语义（一律 fallback）

下列任意一种情况，CLI 会记一条 `slog.Warn` 并降级到 `--tools` 文件：

| 情况 | 行为 |
|---|---|
| 连接被拒绝 / Tasker 端没起 | fallback |
| 超时（> 5s） | fallback |
| HTTP 非 200（含 404 / 500 / 任意状态码） | fallback |
| body 不是合法 JSON | fallback |
| body 是合法 JSON 但不是数组 | fallback |
| body 是空数组 `[]` | fallback（视为"配置出错"而非"零工具"，保守起见） |
| 本地文件也失败 | CLI `os.Exit(1)` 退出 |

---

## Tasker 端如何实现 `mcp_list_tools`

Tasker 端要新增一个 task，命名**严格**为 `mcp_list_tools`（与现有 MCP-enabled task 命名规范无关，CLI 写死匹配这个名字）。

最直接的实现思路：

1. **Task 名**：`mcp_list_tools`
2. **无参数**（不需要 TaskVariable）
3. **首行**：与其它 MCP-enabled task 不同，**不要**放 `MCP#parse_args` 这个 action（这里没有 arguments 要解析）
4. **唯一一步**：返回工具表 JSON 字符串。两种推荐做法：

   **做法 A — 静态嵌入**（最简单）：
   - 用 Tasker 的 **Variable Set** action，把整段 JSON 文本作为 task 的最后 return value（Tasker HTTP server 把最后一个 `%return` 或 task 表达式结果作为 body 回返）
   - 缺点：JSON 内容硬编码在 task 里，工具改动时要手动同步

   **做法 B — 动态生成**（推荐长期方案）：
   - 用 Tasker 的 **Read File** action 从设备某个路径（如 `/sdcard/Tasker/mcp_tools.json`）读 JSON 字符串作为 return value
   - 那个文件的更新可以靠另一个 task 定期跑 `xml-to-tools.js` 等价物（Tasker 内置 JavaScriptlet 也能做）
   - 此时改完 Tasker 工程的真实工具后，更新一下这个 JSON 文件即可

5. **导出**：确保新的 mcp_list_tools task 也跟着 project XML 一起导出（默认会），并被 Tasker HTTP server 暴露

### 验证 Tasker 端实现

不启动 CLI，先用 curl 直接打 Tasker：

```bash
curl -X POST http://<tasker-ip>:1821/run_task \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer tk_...' \
  -d '{"name":"mcp_list_tools","arguments":{}}'
```

预期回包是合法 JSON 数组（同上面 schema）。如果是字符串包着 JSON、或者 body 是 Tasker 的 status report，需要在 task 里调整 return value 的形态。

---

## CLI 端行为

### 启动时

```
INFO  msg="tools source" source=tasker count=27
```
或失败时：
```
WARN  msg="tasker online discovery unavailable, falling back to file" err="..."
INFO  msg="tools source" source=file count=27
```

### 用户改完 Tasker 工程后刷新

| 触发方式 | 行为 |
|---|---|
| `kill -HUP <pid>`（Linux） | 立即重试 online → fallback file |
| 修改 `--tools` 文件时间戳（mtime） | 2 秒内 watcher 检测到，重试 online → fallback file |
| Windows | 无 SIGHUP；改完 Tasker 后**重启 CLI**，或者修改一下 `--tools` 文件触发 mtime 路径 |

重载成功后，上游 `mcp-go` 调用 `s.SetTools(...)` 自动向所有活跃 MCP 客户端广播 `notifications/tools/list_changed`——**客户端会立刻看到新工具列表，无需 reconnect**。

### 与 `--tools` 文件的关系

`--tools` 是**可选**的。行为根据是否设置它而变：

| `--tools` 给了？ | online 成功？ | 行为 |
|---|---|---|
| 是 | 是 | 用 online，`--tools` 文件内容被忽略 |
| 是 | 否 | `WARN` + 读 `--tools` 文件；file 也失败才退出 |
| 否 | 是 | 用 online，**无警告**（静默） |
| 否 | 否 | `ERROR` + 立即退出（没有 fallback 可用） |

推荐做法之一：始终保留 `--tools` 指向**最近一次导出**的 `toolDescriptions.json`，作为安全网。如果你的 Tasker 端 `mcp_list_tools` 已经稳定，也可以**完全不传 `--tools`**——CLI 会强依赖 online 路径，发现 Tasker 端配置出错时立即 fail-fast 退出。

---

## 失败排查

| 现象 | 可能原因 |
|---|---|
| `source=file` 始终，从来不到 `source=tasker` | 1) Tasker 端没新建 `mcp_list_tools` task；2) Tasker HTTP server 没起或端口不对；3) `--tasker-api-key` 错；4) Tasker 端的 task return value 不是合法 JSON 数组 |
| `source=tasker` 但工具数量比预期少 | Tasker 端的 mcp_list_tools task 返回的数组里少了条目——检查 task 实现（做法 A 的硬编码漏了，或做法 B 的文件没更新） |
| SIGHUP 后日志没变化 | Tasker 端那边 task return value 没改；或者 Tasker HTTP server 缓存了响应（极少见） |
| Windows 上 SIGHUP 无效 | 设计如此（Windows 无 SIGHUP）。改 `--tools` 文件 mtime 触发，或重启 CLI |

---

## 设计权衡

- **为什么不上 SSE/WebSocket 让 Tasker 主动推**？Tasker 端是 HTTP server 不是 client，让它主动推会强迫用户在 Tasker 工程里加复杂的 polling/recursion 逻辑。`mcp_list_tools` + SIGHUP / mtime 让用户体感"主动 pull"足够好。
- **为什么超时硬编码 5s 而不是复用 `--tasker-timeout`**？启动期的发现失败应该尽快降级；运行期工具调用允许更长超时是合理的（30s 默认）。两个语义不该混用。
- **为什么空数组也当失败**？经验上，"工具表清空"通常是用户配置出错的征兆，而不是真的想跑零工具的服务。保守 fallback 给一次机会用旧文件。如果用户真要清空，删本地文件 + Tasker 返回空数组 → 启动失败 → 用户能注意到。
