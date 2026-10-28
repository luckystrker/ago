# AG2 Spec: Tools / function calling (Python reference)

> Recon report from the AG2 Python clone (commit fa4e0fb, 2026-10-27). Package root: `C:\projects\ago2\ag2\ag2`.
> Tools are stream subscribers; everything a turn does is expressed as events on a `Stream`.

## 1. The Tool abstraction

### `Tool` ABC — `ag2\tools\tool.py` (17 lines)
```python
class Tool(ABC):
    name: str
    declared_in_code: ClassVar[bool] = True   # False for runtime-provided tools (MCP, remote)
    @abstractmethod
    async def schemas(self, context: "Context") -> Iterable[ToolSchema]: ...
    def register(self, stack: ExitStack, context: "Context", *, middleware=()) -> None: ...
```
Two-method contract: `schemas()` = what capability I contribute to the LLM request; `register()` = subscribe me on the event stream for calls matching my name (ADR 0002).

### Tool kinds
| Kind | File | Notes |
|---|---|---|
| `FunctionTool` | `tools\final\function_tool.py` | Wraps a `fast_depends.core.CallModel`; slots `(model, name, schema, timeout, _middleware)`; `source` = `function:{module}:{qualname}:{name}`; `with_middleware(*mw)` returns copy with prepended hooks; `ensure_tool(func)` static: pass-through if Tool else apply `@tool`. |
| `Toolkit` | `tools\final\toolkit.py` | Group: `__init__(*tools, name=None, middleware=())`; duplicate name → `ToolConflictError`; `__or__` merges; `schemas()` = concat; `register()` delegates; `_snapshot()` per turn. |
| `ClientTool` | `tools\final\client_tool.py` | Declared by a remote client (`declared_in_code=False`); `__call__` returns `ClientToolCallEvent` (execution on the client). |
| `SubagentTool(FunctionTool)` | `tools\subagents\subagent_tool.py` | Wraps an `Agent`; custom deepcopy keeps the target agent. |
| `_MCPProxyTool(Tool)` | `tools\toolkits\mcp_server\toolkit.py` | Per-remote-tool proxy (`declared_in_code=False`). |
| `ToolSearchTool` | `tools\builtin\tool_search.py` | Wraps tools as deferred/searchable. |

### `ToolSchema` — `ag2\tools\schemas.py`
```python
@dataclass(slots=True)
class ToolSchema:
    type: str
```
`FunctionToolSchema(ToolSchema)`: `type="function"`, `function: FunctionDefinition(name, description="", parameters: dict)`, `defer_loading: bool = False`; `__post_init__` pops `"title"` from parameters. Builtin tools define their own schema dataclasses (e.g. `WebSearchToolSchema` type="web_search").

### `ToolResult` — `ag2\events\tool_events.py`
```python
@dataclass(slots=True)
class ToolResult:
    parts: list[Input]
    final: bool = False      # kw_only, compare=False
    metadata: dict[str, Any] = {}
```
Ctor takes `*inputs` wrapped via `Input.ensure_input`.

**`final=True` semantics**: ends the model loop immediately. In `ToolExecutor.execute_tools`: exactly one part required (ValueError otherwise); TextInput → content string, DataInput → `serializer.encode(part.data).decode()`, other types → ValueError. Then sends `ModelResponse(message=ModelMessage(content, metadata=result.metadata), response_force=True)`. `_execute_turn` loops only while `message.tool_calls and not message.response_force`.

### Event shapes (`ag2\events\tool_events.py`)
- `ToolCallEvent` — `id` (uuid4), `name`, `arguments: str = "{}"` (raw JSON), `vendor_metadata: dict` (provider-native), `source: str | None` (set by the tool via `handled_by(source)`, e.g. `function:app.ops:deploy:deploy` or `mcp:host:get_weather`), lazy `serialized_arguments` (json.loads), `to_api()` → `{"id", "type": "function", "function": {"arguments", "name"}}`.
- `ToolCallsEvent` — `calls: list[ToolCallEvent]`, `to_api()` list.
- `ToolResultEvent` — `parent_id` (= call id), `name`, `result: ToolResult`; `from_call(call, result)`.
- `ToolErrorEvent(ToolResultEvent)` — `+ error: Exception`; result = ToolResult whose single part is the formatted traceback; `from_call` always populates result.
- `ToolNotFoundEvent(ToolErrorEvent)` — name not registered.
- `ClientToolCallEvent` — `id/name/arguments`; executor forwards to the remote client.
- `BuiltinToolCallEvent` / `BuiltinToolResultEvent` — provider-hosted tools.

Round-trip: `ModelResponse.tool_calls` sent on stream by `_execute_turn` (agent.py:1831) → ToolExecutor consumes → emits `ToolResultsEvent(results)` → `_call_client` (subscribed to `ModelRequest | ToolResultsEvent`) calls LLM. History→API mapping (`openai\mappers.py:404-425`): each `ToolResultEvent` → `{"role": "tool", "tool_call_id": r.parent_id, "content": ...}`.

## 2. Schema generation

Library: **`fast-depends`** (with Pydantic v2). `ag2\utils.py:build_model(f, *, sync_to_thread=True, serialize_result=True)` → `fast_depends.core.build_call_model`. JSON schema via `fast_depends.pydantic.schema.get_schema(call_model, exclude=(CONTEXT_OPTION_NAME,))` (`CONTEXT_OPTION_NAME = "__ctx__"`). No-arg callable → `{"type": "null"}` normalized to `{"type": "object", "properties": {}}`. Explicit `schema=` kwarg bypasses generation. Description defaults to `__doc__`, name to `__name__`.

Example (test\tools\test_agent_tools.py:17): `def my_tool(a: str, b: int) -> str: """Tool description."""` →
```json
{"type": "function", "defer_loading": false,
 "function": {"name": "my_tool", "description": "Tool description.",
   "parameters": {"type": "object",
     "properties": {"a": {"title": "A", "type": "string"}, "b": {"title": "B", "type": "integer"}},
     "required": ["a", "b"]}}}
```
Provider mapping: `tool_to_api` (`openai\mappers.py:561`) → `{"type": "function", "function": {name, description, parameters}}` (`_ensure_object_schema` forces type=object, additionalProperties=False). `defer_loading=True` raises `UnsupportedToolError` on Chat Completions (Responses API only).

## 3. Registration flow

1. **`@tool` decorator** (`function_tool.py:203`): kwargs `name, description, schema, sync_to_thread=True, timeout, middleware`.
2. **Attach to Agent**: `Agent(..., tools=[...])` → `self.tools: list[Tool]` (through `ensure_tool`). Plugins contribute `plugin._tools`. Auto-injected tools (subtask toolkit `run_subtask`/`run_subtasks`; knowledge tool) → `self._additional_tools`, never reach spawned subtask agents.
3. **Per-turn** in `Agent._drive_scope` (agent.py:1473-1627): `all_tools = chain(self.tools, self._additional_tools, additional_tools)` → `resolve_tools(all_tools, context)` → schemas (ToolSearchToolSchema forced last) → `llm_call = partial(client, tools=schemas, response_schema=..., serializer=...)` → `self._tool_executor.register(stack, context, tools=resolved.tools, known_tools=resolved.known_tools, middleware=...)`.
4. **`resolve_tools`** (`tools\precedence.py`): `_collect_leaves` calls `tool.schemas(context)` (hook point where Toolkit/MCPToolkit discovers members), takes `_snapshot()` per turn. `_select` resolves collisions: declared-in-code tools override in declaration order (warning logged); runtime-provided (`declared_in_code=False`) never override, dropped; remote-vs-remote first wins. `_prune` drops losing members from toolkits for that turn. Output: `ResolvedTools(tools, schemas, known_tools)` — known_tools = set of function names (or builtin type strings) used for `ToolNotFoundEvent`.
5. Each `FunctionTool.register` subscribes `context.stream.where(ToolCallEvent.name == name).sub_scope(execute)`; the executor subscribes to batch-level `ToolCallsEvent` + fallback for unknown names.

## 4. Tool executor — `ag2\tools\executor.py`

- `ToolExecutor(serializer)`; per-turn `register(stack, context, *, tools=(), known_tools=(), middleware=())`: subscribes `execute_tools` to `ToolCallsEvent`; calls `tool.register(...)` on each; fallback subscriber on `ToolCallEvent` → `_tool_not_found`: if name not in known_tools → `ToolNotFoundEvent.from_call(event, ToolNotFoundError(name))`.
- **Parallelism**: one task per call, `asyncio.gather`. On `HumanInputError` cancels siblings and re-raises (sync tools run in worker threads, cannot be cancelled).
- **Per-call flow** (`_execute_call`): opens `context.stream.get((ToolErrorEvent.parent_id == call.id) | (ToolResultEvent.parent_id == call.id) | (ClientToolCallEvent.id == call.id))`, sends the `ToolCallEvent`, awaits. Any Exception → `ToolErrorEvent.from_call(call, e)`; `HumanInputError` re-raised (failed HITL is not a tool result).
- **Settling**: client calls → `ModelResponse(tool_calls=..., response_force=True)` (preserving provider fields, middleware-approved name/arguments). Otherwise → `ToolResultsEvent(results)`. `final=True` short-circuits (section 1).
- **Middleware order**: tool-scoped `_middleware` wrapped reversed first, then agent-turn `on_tool_execution` hooks (agent middleware outermost). Chain: middleware → `FunctionTool.__call__` → `asyncio.wait_for(self._execute(...), timeout)` (timeout → ToolErrorEvent "Tool 'X' timed out after N seconds") → fast_depends solve.
- **DI resolution**: `self.model.asolve(**(event.serialized_arguments | {"__ctx__": replace(context, tool_call_id=event.id)}), stack, cache_dependencies={}, dependency_provider=context.dependency_provider)`. Custom fields (`ag2\annotations.py`): `Context` = injects conversation context under any param name; `Inject(real_name, *, default, default_factory, cast)` resolves from `ctx.dependencies` (materializes default on first use); `Variable(...)` same against `ctx.variables` (also resolved manually via `tools\builtin\_resolve.py:resolve_variable`).

## 5. MCP

### Client side — `ag2\tools\toolkits\mcp_server\toolkit.py`
`MCPToolkit(Toolkit)`: `__init__(server: str | MCPServerConfig | MCPStdioServerConfig, *, middleware=(), answering: MCPAnswerPolicy | None=None)`. URL string → `MCPServerConfig(server_url=...)`; name = `server_label` or `"mcp_toolkit"`.
- **Lazy discovery**: first `schemas(context)` (under lock) runs `_discover_tools`: resolves config (fields may be `Variable`s), `authorization_token` → Bearer header, opens session if config changed, lists tools with cursor pagination (`_MAX_TOOL_PAGES=1000`, repeated-cursor detection). Filters `allowed_tools`/`blocked_tools`; `tool_name_prefix` namespaces. One `_MCPProxyTool` per remote tool, cached by config; `_snapshot()` pins per turn.
- **Session**: stdio (`StdioServerParameters`) or streamable-HTTP; `session.initialize()` (or auto-era negotiation).
- **Calling**: `_MCPProxyTool.__call__` opens a fresh short-lived session per call, `session.call_tool(remote_name, arguments, allow_input_required=True)`. `InputRequiredResult` → `run_input_required_driver` loops dispatch/retry up to `answering.max_rounds`, echoing opaque `request_state`. `is_error` → `ToolErrorEvent`; success → `_extract_content`: `TextContent→TextInput`, `ImageContent/AudioContent→BinaryInput(base64)`, `ResourceLink→UrlInput`, `EmbeddedResource→TextInput/BinaryInput`, unknown → JSON text.
- Config types (`toolkits\mcp_server\types.py`): `MCPServerConfig(server_url, headers, authorization_token, server_label, description, allowed_tools, blocked_tools, tool_name_prefix, connection_timeout, proxy, verify, protocol_mode)` and `MCPStdioServerConfig(command, args, env, cwd, encoding, + policy fields)`. `MCPAnswerPolicy` (`answering.py:69`): `elicitation="decline"`, `sampling=False`, roots — all off by default.

### Server side — `ag2\mcp\` (19 files)
MCP-server product: expose AG2 agents/tools over MCP. `MCPServer` (`server.py`) ASGI3 app on `mcp.server.lowlevel.Server` + Starlette, stdio + streamable-HTTP transports, bearer/auth routes, session store, paused/resumable runs, MCP Apps UI extension, resources/prompts/sampling/elicitation providers, security. `AgentExecutor` (`executor.py:80`) bridges MCP `tools/call` on a generated `ask` tool to one `Agent.ask` turn. `mcp\tools.py` `mcp_tool`/`MCPFunctionTool` for deterministic tools.

### Go port of the client — minimum
1. JSON-RPC MCP client over stdio (subprocess) and streamable-HTTP.
2. Initialize handshake.
3. `tools/list` with cursor pagination, allow/block filters, `tool_name_prefix`, discovery cache keyed by resolved config.
4. `tools/call` with `is_error` handling and content-block mapping → typed result parts.
5. Optional `input_required` round-trip (callbacks, echo `request_state`, bounded rounds).
6. Wrapping each remote tool with `declared_in_code=False` so name-precedence (code > remote) holds; per-call session lifetime.

## 6. Builtin / hosted tools (names)

`ag2\tools\builtin\`: `AnthropicBashTool`, `CodeExecutionTool`, `FileSearchTool`, `GoogleMapsTool`, `ImageGenerationTool`, `MCPServerTool`, `MemoryTool`, `RetrievalTool`, `ShellTool` (+ ContainerAutoEnvironment, ContainerReferenceEnvironment, NetworkPolicy), `Skill`/`SkillsTool`, `ToolSearchTool`, `WebFetchTool`, `WebSearchTool` (+ UserLocation), `XSearchTool`. Others: `search\` (DuckDuckSearchTool, PerplexitySearchToolkit, TavilySearchTool), `sandbox\`+`code\` (SandboxCodeTool, LocalEnvironment), `shell\` (SandboxShellTool), `toolkits\` (FilesystemToolkit, MCPToolkit), `skills\` (SkillsToolkit, SkillSearchToolkit, SkillPlugin, MemorySkill), `subagents\` (subagent_tool, background_agent_tool, persistent_stream, run_subtask/run_subtasks), `dynamic\factory.py` (dynamic_agent, name `create_and_run_agent`).

Pattern: builtin tools are schema-only carriers — `schemas()` returns a provider-neutral dataclass (params may be `Variable` markers resolved at schema time); `register()` subscribes to `BuiltinToolCallEvent.name == name` with a no-op handler (the provider executes and returns `BuiltinToolResultEvent`).

## 7. ESSENTIAL vs optional; tests

**Essential**: `tools\tool.py`, `tools\schemas.py`, `tools\final\function_tool.py`, `tools\final\toolkit.py`, `tools\final\client_tool.py`, `tools\executor.py`, `tools\precedence.py`; `annotations.py` (DI fields), `utils.py` (`build_model`, CONTEXT_OPTION_NAME); `events\tool_events.py`, `events\input_events.py`, `events\types.py`; `middleware\base.py` (`on_tool_execution`); `agent.py` `_drive_scope` + `_execute_turn`; `openai\mappers.py` `tool_to_api` + tool-result history mapping.

**Optional**: `builtin\` except tool_search; `search\`, `sandbox\`, `shell\`, `code\`, `skills\`, `subagents\`, `dynamic\`, `toolkits\filesystem.py`, `toolkits\mcp_server\` (needs `mcp` extra), `ag2\mcp\` (server product), `builtin\mcp_server.py` (hosted-MCP type).

**Tests**: `test\tools\` (test_agent_tools.py — schema assertions incl. the example above, test_execution.py, test_executor_builtin_passthrough.py, test_precedence.py, test_toolkit.py, test_naming.py, test_timeout.py, test_client_tool.py, test_defer_loading.py, test_tool_search_tool.py, test_resolve.py, test_public_exports.py, test_mcp*.py; subdirs fixtures/, sandbox/, search/, skills/); `test\mcp\` (server-side); `test\config\<provider>\tools\`.
