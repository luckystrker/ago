# AG2 Spec: LLM configuration & model clients (Python reference)

> Recon report from the AG2 Python clone (commit fa4e0fb, 2026-10-27). Package root: `C:\projects\ago2\ag2\ag2`.
> The 2026 AG2 is async-first, event-sourced; configs are **typed dataclasses, not dicts**; no
> `config_list`, no `register_model_client` registry, **no response cache**. One `LLMClient` protocol.

## 1. The LLM config model

### Core abstractions — `ag2\config\config.py` (45 LOC)
- `ModelProvider(str, Enum)`: `anthropic, bedrock, dashscope, gemini, mistral, ollama, openai, typesafe, vertexai, xai, zai`.
- `ModelConfig` (Protocol): `provider -> ModelProvider`, `model -> str`, `copy(**overrides) -> Self` (dataclasses.replace), `create() -> LLMClient` (factory; fresh client each call), `create_files_client() -> FilesClient` (NotImplementedError when unsupported).

No runtime schema validation: typing + dataclass construction + deprecation warnings. `Omit`/`not_given` sentinels mean "don't send the field" (vs None = send null). Missing config raises `ConfigNotProvidedError` (`ag2\exceptions.py:189`).

### Per-provider config keys (dataclass fields, exact names and defaults)

**OpenAI** — `ag2\config\openai\config.py` (three configs):
- `OpenAICompletionsConfig` — Chat Completions incl. compatible endpoints. Required `model: ChatModel | str`; defaults: `api_key=None` (SDK reads env), `base_url=None`, ~30 sampling/verbosity fields `= omit` (temperature, top_p, max_tokens, max_completion_tokens, frequency_penalty, presence_penalty, seed, stop, n, user, logprobs, top_logprobs, tool_choice, parallel_tool_calls, reasoning_effort, logit_bias, metadata, modalities, prediction, prompt_cache_key, prompt_cache_options, safety_identifier, service_tier, store, verbosity, web_search_options), `streaming=False`, `timeout=not_given`, `max_retries=4`, `organization/project/websocket_base_url/default_headers/default_query/http_client/extra_body=None`.
- `OpenAIResponsesConfig` — Responses API: `store=True`, `parallel_tool_calls=True`, `max_output_tokens`, `max_tool_calls`, `reasoning`, `truncation`, `prompt_cache_diagnostics=False`, `user=""`. (`OpenAIConfig` = deprecated alias of Completions.)
- `OpenAIDecisionsConfig` — Decisions API: `model="gpt-6-luna"`, `boolean_threshold=0.5`, `descriptions`, `question_instructions`.

**Anthropic** — `config.py` (extra `anthropic`): `model` (required), `max_tokens=4096`, `api_key/base_url/metadata/service_tier/extra_body=None`, `streaming=False`, `stop_sequences=None`, `timeout=None`, `max_retries=2`, `prompt_caching=True`, `cache_diagnostics=False`, `speed: "standard"|"fast"|None`, `open_telemetry`, `http_client`. `temperature/top_p/top_k` deprecated (API 2.x rejects them).

**Gemini** — `gemini\config.py`: `GeminiConfig(api_key)` and `VertexAIConfig(credentials, project, location)` share `GeminiBaseConfig`: `model` (required), `temperature/top_p/top_k/max_output_tokens/stop_sequences/presence_penalty/frequency_penalty/seed/cached_content/response_modalities/image_config/thinking_config/thinking_level/thinking_budget/http_client` all `None`, `streaming=False`.

**Bedrock** — `bedrock\config.py` (Converse API): `model` (model id or inference-profile ARN), `aws_access_key_id/aws_secret_access_key/aws_session_token/profile_name/region_name/endpoint_url=None` (botocore chain), `max_tokens/temperature/top_p/stop_sequences=None`, `streaming=False`, `additional_model_request_fields/additional_model_response_field_paths/guardrail_config/performance_config/request_metadata`, `timeout=None`, `max_retries=None`.

**Mistral** — `mistral\config.py` (extra `mistralai`): `model`, `api_key` (env `MISTRAL_API_KEY`), `server_url`, `timeout_ms`, ~25 sampling/reasoning/guardrail fields, `prompt_mode`, `prompt_cache_key`, `safe_prompt`.

**Ollama** — `ollama\config.py`: `model`, `host=OLLAMA_DEFAULT_HOST`, `api_key` (Ollama Cloud bearer / env `OLLAMA_API_KEY`), `http_client`, `temperature/top_p/max_tokens/stop/seed/frequency_penalty/presence_penalty=None`, `streaming=False`.

**DashScope** — `dashscope\config.py`: `model`, `base_url=DASHSCOPE_INTL_BASE_URL`, `api_key`, minimal sampling, `streaming=False`.

**xAI** — `xai\config.py` (gRPC SDK): `model`, `api_key`, `api_host="api.x.ai"`, `timeout`, `metadata/channel_options` (gRPC), `max_tokens`, `reasoning_effort`, `store_messages`, `previous_response_id`, `use_encrypted_content`, `max_turns`, `include`, `conversation_id`.

**Z.AI** — `zai\config.py` (extra `zai`): `model`, `api_key`, `base_url`, `max_retries=3`, `disable_token_cache=True`, `thinking`, `reasoning_effort`, `tool_stream`, `watermark_enabled`, `sensitive_word_check`, `extra/extra_headers/extra_body`.

**TypeSafe** — `typesafe\config.py`: `model=DEFAULT_MODEL` (from `typesafe_sdk`), `api_key/base_url/timeout/retry(RetryPolicy)/headers/http_client`, `boolean_threshold=0.5`, `criteria`. Not a chat model: one typed question per call from `response_schema`; tools rejected.

### Media-input capability table — `config\input_acceptance.py` (225 LOC)
Static table `_MEDIA`: per config class, which `(position, source, kind)` inputs its mapper accepts (`position`: `user`/`tool`; `source`: `data`/`url`/`file`; `kind`: IMAGE/AUDIO/VIDEO/DOCUMENT). API: `accepts_input(config, position, part)`, `input_modalities(config)`, `input_media_types(config)`, `binary_kind_of(media_type)`. Bedrock extra MIME allowlists; OpenAI audio wav/mp3; Anthropic images jpeg/png/gif/webp; `ZAIConfig: set()`.

## 2. The ModelClient abstraction

`ag2\config\client.py` (26 LOC) — the whole interface:

```python
@runtime_checkable
class LLMClient(Protocol):
    async def __call__(
        self,
        messages: Sequence[BaseEvent],          # full event history, replayed to the provider
        context: ConversationContext,           # .prompt: list[str] = system prompt lines; .send() publishes events
        *,
        tools: Iterable[ToolSchema],
        response_schema: ResponseProto | None,  # structured output contract
        serializer: SerializerProto,            # fast_depends serializer for DataInput payloads
    ) -> ModelResponse: ...
```

Return type `ModelResponse` (`ag2\events\types.py:104`): `message: ModelMessage | None`, `tool_calls: ToolCallsEvent` (list of `ToolCallEvent(id, name, arguments)`), `usage: Usage`, `response_force: bool`, `files: list[BinaryResult]`, tracing `model/provider/finish_reason/response_id`.

History consists of: `ModelRequest(parts)` (+ `DrainedModelRequest`), `ModelResponse`, `ToolResultsEvent` (each `ToolResultEvent.parent_id` = tool-call id, `.result.parts`), `CompactionSummary`, and provider-specific replay events marked `ProviderReplay` (e.g. `OpenAIReasoningEvent`, `AnthropicRedactedThinkingEvent`). Input subtypes: `TextInput`, `DataInput`, `BinaryInput(data, media_type, kind, vendor_metadata)`, `UrlInput(url, kind)`, `FileIdInput(file_id, filename?)`.

Structured output — `ResponseProto` (`ag2\response\proto.py`): `name/description/json_schema/system_prompt` + `async validate(response, context, provider) -> T`. Concrete: `ResponseSchema` (`response\schema.py`), `PromptedSchema`, `response_schema` decorator, `DecisionSpec`.

Tool schema — `ag2\tools\schemas.py`: `@dataclass class ToolSchema { type: str }`; `FunctionToolSchema` (`tools\final\function_tool.py`: `.function.name/.description/.parameters`, `.defer_loading`); builtin/hosted schemas in `tools\builtin\*` (shell, code_execution, web_search, web_fetch, file_search, image_generation, mcp_server, skills, tool_search).

Files client — `ag2\files\protocol.py`: `FilesClient[P]` protocol `upload(data, filename, purpose) -> UploadedFile`, `read(file_id) -> FileContent`, `list()`, `delete(file_id)`; wrapper `FilesAPI` (`files\api.py`).

### Request lifecycle (framework side)
`Agent.ask/run` → `_prepare_turn` (agent.py:1352) resolves `config or self.config` (fails `ConfigNotProvidedError`), stamps into dependencies, seeds `context.prompt`, calls `config.create()`. `_drive_scope` (~1489) wraps the client as `llm_call = partial(client, tools=schemas, response_schema=..., serializer=...)` and composes middlewares (`mw.on_llm_call`). `_call_client` (1563) subscribes to `ModelRequest | ToolResultsEvent`, converts full history, awaits the client, emits `UsageEvent(kind="model_call", ...)` + `ModelResponse`. Turn loop `_execute_turn` (1807): while tool_calls → send ToolCallsEvent → executor answers ToolResultsEvent → re-fire; else drain pending input and return.

## 3. Providers and provider-specific code volume

Each provider dir = `config.py` + `*_client.py` (LLMClient impl) + `mappers.py` (history→wire, tool schema→wire, usage normalization) + optional `events.py` (provider replay/builtin-tool events) + `files.py`.

| Provider | Dir | SDK / transport | Approx LOC | Key differences |
|---|---|---|---|---|
| OpenAI (3 clients) | `config\openai\` | `openai` SDK ≥3.37 | ~2,900 | Completions + Responses + Decisions; hosted tools (web_search, file_search, shell, code_interpreter, image_gen, MCP, skills), containers, prompt-cache diagnostics, strict JSON schema coercion (`_strictify_schema`) |
| Anthropic | `config\anthropic\` | `anthropic` SDK | ~1,960 | `inject_cache_breakpoint` prompt caching, beta headers per feature, container skills, `output_config` structured output, thinking/redacted-thinking replay events |
| Gemini | `config\gemini\` | `google-genai` | ~1,150 | function_declarations, thinking_config/level/budget, cached_content, image output → BinaryResult, VertexAI auth path |
| Mistral | `config\mistral\` | `mistralai` SDK | ~975 | guardrails, prediction, prompt_mode, safe_prompt, logprobs |
| xAI | `config\xai\` | `xai-sdk` (gRPC) | ~815 | gRPC transport, server-side conversation, ToolMode |
| Z.AI | `config\zai\` | `zai` SDK (httpx) | ~795 | GLM thinking flags, token-cache opt-out, sensitive-word check, Files API via retrieval |
| Bedrock | `config\bedrock\` | aiobotocore (Converse) | ~790 | AWS session/credentials, guardrails, MIME allowlist |
| DashScope | `config\dashscope\` | `dashscope` SDK | ~445 | Qwen via DashScope REST; images only |
| Ollama | `config\ollama\` | `ollama` SDK (httpx) | ~445 | num_predict, JSON format only, images data-only |
| TypeSafe | `config\typesafe\` | `typesafe-sdk` | ~415 | one typed question per call; tools rejected |
| Core | `config.py`, `client.py`, `input_acceptance.py` | — | ~390 | Protocol + provider enum + media table |

Total `ag2\config\` ≈ 11,084 LOC. `__init__.py` wires each config with `missing_optional_dependency` fallback (extras strictly optional).

## 4. Caching

**No framework-level response cache.** Only provider-side prompt caching knobs (OpenAI `prompt_cache_key/prompt_cache_options`, OpenAI Responses `prompt_cache_diagnostics`, Anthropic `prompt_caching=True` + `inject_cache_breakpoint` + `cache_diagnostics`, Gemini `cached_content`) and usage accounting for cache tokens (`Usage.cache_read_input_tokens` / `cache_creation_input_tokens`). `ag2\_replay.py` ensures a cut event tail can be re-sent to the provider (replay integrity, not caching).

## 5. Usage / cost

- `Usage` (`events\types.py:23`): `prompt_tokens, completion_tokens, total_tokens, cache_read_input_tokens, cache_creation_input_tokens, thinking_tokens` (all `float | None`; None = not reported; field-wise `+`).
- Every provider mapper has `normalize_usage(...)` (e.g. `openai\mappers.py:795` from `CompletionUsage` with `prompt_tokens_details.cached_tokens`/`cache_write_tokens` and `reasoning_tokens`; `anthropic\mappers.py:876` from raw dict; `gemini\mappers.py:385` adds `tool_use_prompt_token_count` into prompt, folds `thoughts_token_count` into completion).
- Event-sourced accounting — `ag2\usage.py` (117 LOC): `UsageEvent` emitted at token-spend time; `UsageReport.from_events(events)` aggregates `total`, `records`, `by_model`, `by_provider`, `by_kind`. Sub-agent per-call events stay on the child stream; only the rolled-up `"subtask"` event reaches the parent — no double counting.
- **No pricing table / dollar-cost calculation.** Token budgets only: `eval\scorers\cost.py` `token_budget(max_tokens)`, `policies\token_budget.py`, `observers\token_monitor.py`, `events\sizing.py` `estimated_tokens(event, chars_per_token=4)` (image=1000, audio=1000, video=2000, document=2000 flat).

## 6. Requests: messages, tools, streaming, retries, structured output

- **Messages**: built per-call from event history (`convert_messages` / `events_to_responses_input` per `mappers.py`). System prompt = `"\n".join(context.prompt)` (+ optional `response_schema.system_prompt`). Mapping: `ModelRequest`→user (parts → text/base64 image data-URL/audio/document/file-id), `ModelResponse`→assistant (content + replayed `tool_calls` with `json.dumps(call.serialized_arguments)`), `ToolResultsEvent`→tool messages keyed by `parent_id`, `CompactionSummary`→user text `[Summary of earlier conversation]`. Provider replay events round-trip via SDK dump. Unsupported part/tool → `UnsupportedInputError` / `UnsupportedToolError` (fail fast, never silently dropped).
- **Tool calling**: `ToolSchema` → provider tool params (`tool_to_api`); OpenAI enforces object schemas (`_ensure_object_schema`); Anthropic auto-injects `CodeExecutionToolSchema` when skills present; results come back as `ToolCallEvent(id, name, arguments)` inside `ModelResponse.tool_calls`; executor loop re-calls model with `ToolResultsEvent`.
- **Streaming**: per-config `streaming: bool = False`. Stream path: SSE chunks accumulate content (emitting transient `ModelMessageChunk` / `ModelReasoning` via `context.send`), merge tool-call deltas by index, usage from final chunk, then one synthesized `ModelResponse`.
- **Retries**: transport-level only (SDK `max_retries`: OpenAI 4, Anthropic 2, Z.AI 3). Framework doesn't retry LLM calls; only `AgentReply.content(retries=N)` re-asks on schema-validation failure with the error appended.
- **Structured output**: OpenAI `response_format` (strict JSON schema via `_strictify_schema`), Anthropic `output_config`, Gemini/Bedrock/Ollama/DashScope response-schema/format helpers, Mistral JSON schema. Validation framework-side via `ResponseProto.validate`.

## 7. Essential vs optional; tests

**Essential**: `config\config.py` + `client.py` (~70 LOC); event vocabulary; `response\proto.py` + `schema.py`; `tools\schemas.py` + `tools\final\function_tool.py`; one provider (best reference: `config\openai\` Completions path — `openai_client.py` 269 LOC + ~440 of mappers.py + config.py); framework plumbing in `agent.py` (`_prepare_turn`, `_call_client`, `_execute_turn`) and `context.py`.

**Optional**: all other providers (extras: `openai, anthropic, gemini, xai, zai, mistral, ollama, bedrock, dashscope, typesafe`); Files API; hosted/builtin tools; `input_acceptance.py` (capability reporting); containers, prompt-cache diagnostics, decisions client; MCP sampling pseudo-client (`ag2\mcp\sampling.py` implements both `ModelConfig` and `LLMClient`).

Tests: `test\config\<provider>\` (config tests, `test_convert_messages.py`, `test_usage.py`, `test_response_schema_to_api.py`, streaming/tool-call fidelity, files); `test\providers\{openai,anthropic,gemini,typesafe,agent}\` (live/network smoke); `test\test_usage.py`. Fixtures: `test\config\conftest.py`.
