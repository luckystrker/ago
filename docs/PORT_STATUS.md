# Port status

Per-package porting status against `docs/ARCHITECTURE.md`. Green = `go build ./...`,
`go vet ./...` and `go test ./<pkg>/...` pass with no skipped tests.

| Package | Status | Notes |
|---|---|---|
| events | green | wave 0 |
| stream | green | wave 0 |
| context | green | wave 0 |
| hitl | green | wave 1 |
| middleware | green | wave 1 (D-013) |
| response | green | wave 1 (D-015) |
| tools | green | wave 1: Schema/Tool/ToolSchema, FunctionTool (reflection binding, DI, timeout, middleware), Toolkit, ClientTool, Executor (parallel, hitl abandon, final=true), ResolveTools precedence (D-016) |
| usage | green | wave 1 (D-014) |
| task | green | wave 1 (D-014) |
| testing | green | wave 1: TestConfig (scripted ModelConfig+LLMClient, shared cursor, recorded call snapshots) + TrackingConfig (recording wrapper, shared recorder across clones) (D-018) |
| config | green | wave 1: core (Provider, ModelConfig, LLMClient, CallOptions, sentinels) + shared OpenAI-compatible engine `openaicompat.go` (BuildMessages, ToolToAPI, EnsureObjectSchema, NormalizeUsage, RunChatCompat: non-streaming, SSE streaming with tool-call delta merge, strict response_format) + `internal/httpx` (JSON POST, retries 429/5xx + Retry-After) + `internal/sse` (D-017) |
| config/openai | green | wave 1: OpenAICompletionsConfig (constructor defaults, env key, org/project headers, Clone with Go+snake_case keys) + Client over RunChatCompat + StrictifySchema/ResponseFormat (strips title/examples/default) (D-019) |
| config/anthropic | green | wave 1: AnthropicConfig (constructor defaults, env key ANTHROPIC_API_KEY, Clone) + Client over POST /v1/messages (x-api-key, anthropic-version 2023-06-01): ConvertMessages (tool_use/tool_result replay, orphan repair, CompactionSummary), ConvertTools, prompt-cache_control on last system block + last tool, native SSE streaming (content_block_delta → chunks, input_json_delta merge), usage incl. cache tokens (D-020) |
| config/gemini | green | wave 1 (D-022): GeminiConfig (options, env GEMINI_API_KEY, Clone), generateContent REST + streamGenerateContent?alt=sse, contents mapping (text/inlineData/fileData/functionCall/functionResponse), functionDeclarations with additionalProperties stripping, usageMetadata normalization, tool round-trip |
| config/ollama | green | wave 1 (D-022): OllamaConfig (options, env OLLAMA_API_KEY, Clone), /api/chat REST non-streaming (streaming → ErrUnsupportedInput per brief), messages mapping (plain-string content, base64 images, object tool-call arguments), options num_predict, usage prompt_eval/eval counts, tool round-trip |
| agent | pending | wave 2 (build tag `agowave2` on the in-progress draft) |
| knowledge / policies / compact / aggregate / observers / watch / spec / network / mcp | pending | wave 3 |
| config/zai, config/mistral, config/dashscope | green | wave 1 (D-021): ZAIConfig/MistralConfig/DashScopeConfig (constructor defaults, env keys ZAI_API_KEY/MISTRAL_API_KEY/DASHSCOPE_API_KEY, Clone with case-insensitive keys) + chatClient binding onto config.RunChatCompat (/chat/completions; mistral safe_prompt via Extra) |
| config/xai, config/bedrock, config/typesafe | green | wave 1 (D-021, D-008): stub configs (XAIConfig, BedrockConfig, TypeSafeConfig with defaults per spec §1), Create() → config.ErrNotImplemented naming the provider; missing model → ErrNotProvided first |
