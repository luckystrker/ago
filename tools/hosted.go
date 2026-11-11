// Hosted (builtin) tools: schema carriers for capabilities the provider
// executes server-side. A carrier contributes its ToolSchema impl to the model
// request (the provider mappers translate it into the provider's wire format)
// and never executes anything itself: Register is a no-op.
//
// Parity with Python AG2 ag2/tools/builtin/ (web_search, web_fetch,
// code_execution, file_search, image_generation, shell, memory, mcp_server,
// tool_search). The schema fields carry the Python dataclass fields; the
// constructor options carry the tool kwargs.
//
// Per-turn Variable kwargs (Python: builtin/_resolve.py resolve_variable):
// every carrier takes its kwargs as concrete values at construction, and a
// parameter can instead be bound to a Variable with the carrier's
// With<Carrier>Variable option. Schemas then resolves that parameter from the
// conversation variables on each turn.
//
// AGO-DEVIATION: Python types each kwarg as `value | Variable`; Go's option
// funcs take concrete values, so the Variable binding is a separate option
// keyed by the kwarg's wire name (the schema's JSON field name), and an
// unknown name or a mistyped variable value surfaces as a Schemas error. A
// Variable's default is its HasDefault value (no default_factory). See
// docs/DECISIONS.md.
package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	agocontext "github.com/luckystrker/ago/context"
	"github.com/luckystrker/ago/middleware"
	"github.com/luckystrker/ago/stream"
)

// Provider-neutral tool-type names (Python parity: the *_TOOL_NAME constants).
// A hosted schema's SchemaType is one of these; the executor's not-found
// fallback treats them as known names.
const (
	WebSearchToolName       = "web_search"
	WebFetchToolName        = "web_fetch"
	CodeExecutionToolName   = "code_execution"
	FileSearchToolName      = "file_search"
	ImageGenerationToolName = "image_generation"
	ShellToolName           = "shell"
	MemoryToolName          = "memory"
	MCPServerToolName       = "mcp_server"
	ToolSearchToolName      = "tool_search"
	RetrievalToolName       = "retrieval"
)

// Version literals the schemas default to (Python parity: the Literal defaults).
const (
	DefaultWebSearchVersion     = "web_search_20250305"
	DefaultWebFetchVersion      = "web_fetch_20250910"
	DefaultCodeExecutionVersion = "code_execution_20250825"
	ShellVersion                = "bash_20250124"
	MemoryVersion               = "memory_20250818"
)

// --- web search -------------------------------------------------------------------

// UserLocation biases web search toward one location. A nil field is not sent
// and the API's own default applies (Python parity: UserLocation dataclass).
type UserLocation struct {
	City     *string `json:"city,omitempty"`
	Region   *string `json:"region,omitempty"`
	Country  *string `json:"country,omitempty"`
	Timezone *string `json:"timezone,omitempty"`
}

// WebSearchToolSchema is the provider-neutral web_search capability. Anthropic
// maps every field but SearchContextSize (Python parity: the Anthropic mapper
// does not send it); the OpenAI Responses mapping is not ported.
type WebSearchToolSchema struct {
	SearchContextSize string        `json:"search_context_size,omitempty"` // "low" | "medium" | "high"
	MaxUses           *int          `json:"max_uses,omitempty"`
	UserLocation      *UserLocation `json:"user_location,omitempty"`
	AllowedDomains    []string      `json:"allowed_domains,omitempty"`
	BlockedDomains    []string      `json:"blocked_domains,omitempty"`
	WebSearchVersion  string        `json:"web_search_version"`
}

// SchemaType returns the provider-neutral tool type.
func (WebSearchToolSchema) SchemaType() string { return WebSearchToolName }

// WebSearchTool is the hosted web_search capability (Python parity:
// builtin/web_search.py WebSearchTool).
type WebSearchTool struct {
	vars              hostedVars
	searchContextSize *string
	maxUses           *int
	userLocation      *UserLocation
	allowedDomains    []string
	blockedDomains    []string
	version           *string
}

// WebSearchOption configures NewWebSearch.
type WebSearchOption func(*WebSearchTool)

// WithWebSearchVariable binds one schema parameter, named by its JSON field (the Python kwarg
// name), to a Variable resolved from the conversation variables on every turn
// (Python parity: passing a Variable instead of a value). An empty Variable
// name defaults to the parameter name; the variable's default applies when the
// conversation has none. An unknown parameter fails Schemas.
func WithWebSearchVariable(param string, v Variable) WebSearchOption {
	return func(t *WebSearchTool) { t.vars.bind(param, v) }
}

// WithSearchContextSize sets the retrieval effort: "low", "medium" or "high".
func WithSearchContextSize(size string) WebSearchOption {
	return func(t *WebSearchTool) { t.searchContextSize = &size }
}

// WithWebSearchMaxUses caps the number of searches per response.
func WithWebSearchMaxUses(n int) WebSearchOption {
	return func(t *WebSearchTool) { t.maxUses = &n }
}

// WithUserLocation biases results toward one location.
func WithUserLocation(loc UserLocation) WebSearchOption {
	return func(t *WebSearchTool) { t.userLocation = &loc }
}

// WithWebSearchAllowedDomains restricts search to the given domains (nil = API
// default; an empty slice is sent as an empty restriction).
func WithWebSearchAllowedDomains(domains []string) WebSearchOption {
	return func(t *WebSearchTool) { t.allowedDomains = domains }
}

// WithWebSearchBlockedDomains excludes the given domains from search.
func WithWebSearchBlockedDomains(domains []string) WebSearchOption {
	return func(t *WebSearchTool) { t.blockedDomains = domains }
}

// WithWebSearchVersion selects the tool version literal.
func WithWebSearchVersion(version string) WebSearchOption {
	return func(t *WebSearchTool) { t.version = &version }
}

// NewWebSearch builds the hosted web_search tool.
func NewWebSearch(opts ...WebSearchOption) *WebSearchTool {
	t := &WebSearchTool{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *WebSearchTool) Name() string { return WebSearchToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *WebSearchTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *WebSearchTool) Schemas(cx *agocontext.Context) ([]any, error) {
	schema, err := t.vars.resolve(WebSearchToolName, cx, WebSearchToolSchema{
		SearchContextSize: derefString(t.searchContextSize),
		MaxUses:           t.maxUses,
		UserLocation:      t.userLocation,
		AllowedDomains:    copyStrings(t.allowedDomains),
		BlockedDomains:    copyStrings(t.blockedDomains),
		WebSearchVersion:  derefString(t.version, DefaultWebSearchVersion),
	})
	if err != nil {
		return nil, err
	}
	return []any{schema}, nil
}

// Register subscribes nothing: the tool runs at the provider. Python enters a
// no-op subscriber on the builtin-tool-call event; there is nothing to keep
// alive across a turn here, so the subscription is simply omitted.
func (t *WebSearchTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- web fetch --------------------------------------------------------------------

// ToolSources states what one url_sources entry may contribute: everything
// ("all"), nothing ("none"), or a named subset of tools ("only"/"except", with
// Tools naming them). Python splits the named forms into the OnlyTools and
// ExceptTools dataclasses; Kind carries the API's own discriminator.
type ToolSources struct {
	Kind  string   `json:"type"` // "all" | "none" | "only" | "except"
	Tools []string `json:"tools,omitempty"`
}

// AllSources lets a source contribute everything it supplies.
func AllSources() ToolSources { return ToolSources{Kind: "all"} }

// NoneSources lets a source contribute nothing.
func NoneSources() ToolSources { return ToolSources{Kind: "none"} }

// OnlySources restricts a source to the named tools' contributions.
func OnlySources(toolNames []string) ToolSources {
	return ToolSources{Kind: "only", Tools: copyStrings(toolNames)}
}

// ExceptSources excludes the named tools' contributions from a source.
func ExceptSources(toolNames []string) ToolSources {
	return ToolSources{Kind: "except", Tools: copyStrings(toolNames)}
}

// UrlSources controls which sources may supply the URLs web fetch may fetch. A
// field left empty is not sent and the API's own default applies. This is a
// policy control: it filters by who proposed the URL, which AllowedDomains
// (filtering by host) cannot express.
type UrlSources struct {
	// UserInput is what user messages may contribute: "all" or "none".
	UserInput string `json:"user_input,omitempty"`
	// ClientToolResults and ServerToolResults are what the respective tool
	// results may contribute: all, none, or a named subset.
	ClientToolResults *ToolSources `json:"client_tool_results,omitempty"`
	ServerToolResults *ToolSources `json:"server_tool_results,omitempty"`
}

// WebFetchToolSchema is the provider-neutral web_fetch capability. Anthropic
// honours it; Gemini ignores every field and enables its url_context tool.
type WebFetchToolSchema struct {
	MaxUses           *int        `json:"max_uses,omitempty"`
	AllowedDomains    []string    `json:"allowed_domains,omitempty"`
	BlockedDomains    []string    `json:"blocked_domains,omitempty"`
	Citations         *bool       `json:"citations,omitempty"`
	MaxContentTokens  *int        `json:"max_content_tokens,omitempty"`
	Strict            *bool       `json:"strict,omitempty"`
	UseCache          *bool       `json:"use_cache,omitempty"`
	ResponseInclusion string      `json:"response_inclusion,omitempty"` // "full" | "excluded"
	UrlSources        *UrlSources `json:"url_sources,omitempty"`
	WebFetchVersion   string      `json:"web_fetch_version"`
}

// SchemaType returns the provider-neutral tool type.
func (WebFetchToolSchema) SchemaType() string { return WebFetchToolName }

// WebFetchTool is the hosted web_fetch tool (Python parity: builtin/web_fetch.py
// WebFetchTool).
type WebFetchTool struct {
	vars              hostedVars
	maxUses           *int
	allowedDomains    []string
	blockedDomains    []string
	citations         *bool
	maxContentTokens  *int
	strict            *bool
	useCache          *bool
	responseInclusion *string
	urlSources        *UrlSources
	version           *string
}

// WebFetchOption configures NewWebFetch.
type WebFetchOption func(*WebFetchTool)

// WithWebFetchVariable binds one schema parameter, named by its JSON field (the Python kwarg
// name), to a Variable resolved from the conversation variables on every turn
// (Python parity: passing a Variable instead of a value). An empty Variable
// name defaults to the parameter name; the variable's default applies when the
// conversation has none. An unknown parameter fails Schemas.
func WithWebFetchVariable(param string, v Variable) WebFetchOption {
	return func(t *WebFetchTool) { t.vars.bind(param, v) }
}

// WithWebFetchMaxUses caps the number of fetches per response.
func WithWebFetchMaxUses(n int) WebFetchOption {
	return func(t *WebFetchTool) { t.maxUses = &n }
}

// WithWebFetchAllowedDomains restricts fetches to the given domains.
func WithWebFetchAllowedDomains(domains []string) WebFetchOption {
	return func(t *WebFetchTool) { t.allowedDomains = domains }
}

// WithWebFetchBlockedDomains excludes the given domains from fetches.
func WithWebFetchBlockedDomains(domains []string) WebFetchOption {
	return func(t *WebFetchTool) { t.blockedDomains = domains }
}

// WithWebFetchCitations enables citation generation for fetched content.
func WithWebFetchCitations(enabled bool) WebFetchOption {
	return func(t *WebFetchTool) { t.citations = &enabled }
}

// WithWebFetchMaxContentTokens caps the tokens fetched content may occupy.
func WithWebFetchMaxContentTokens(n int) WebFetchOption {
	return func(t *WebFetchTool) { t.maxContentTokens = &n }
}

// WithWebFetchStrict enables strict URL validation.
func WithWebFetchStrict(strict bool) WebFetchOption {
	return func(t *WebFetchTool) { t.strict = &strict }
}

// WithWebFetchUseCache enables the fetch cache (needs version web_fetch_20260309
// or later on Anthropic).
func WithWebFetchUseCache(useCache bool) WebFetchOption {
	return func(t *WebFetchTool) { t.useCache = &useCache }
}

// WithWebFetchResponseInclusion sets how fetched content appears in the response:
// "full" or "excluded" (needs version web_fetch_20260318 or later on Anthropic).
func WithWebFetchResponseInclusion(inclusion string) WebFetchOption {
	return func(t *WebFetchTool) { t.responseInclusion = &inclusion }
}

// WithWebFetchURLSources sets the url_sources policy.
func WithWebFetchURLSources(sources UrlSources) WebFetchOption {
	return func(t *WebFetchTool) { t.urlSources = &sources }
}

// WithWebFetchVersion selects the tool version literal.
func WithWebFetchVersion(version string) WebFetchOption {
	return func(t *WebFetchTool) { t.version = &version }
}

// NewWebFetch builds the hosted web_fetch tool.
func NewWebFetch(opts ...WebFetchOption) *WebFetchTool {
	t := &WebFetchTool{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *WebFetchTool) Name() string { return WebFetchToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *WebFetchTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *WebFetchTool) Schemas(cx *agocontext.Context) ([]any, error) {
	schema, err := t.vars.resolve(WebFetchToolName, cx, WebFetchToolSchema{
		MaxUses:           t.maxUses,
		AllowedDomains:    copyStrings(t.allowedDomains),
		BlockedDomains:    copyStrings(t.blockedDomains),
		Citations:         t.citations,
		MaxContentTokens:  t.maxContentTokens,
		Strict:            t.strict,
		UseCache:          t.useCache,
		ResponseInclusion: derefString(t.responseInclusion),
		UrlSources:        t.urlSources,
		WebFetchVersion:   derefString(t.version, DefaultWebFetchVersion),
	})
	if err != nil {
		return nil, err
	}
	return []any{schema}, nil
}

// Register subscribes nothing: the tool runs at the provider (see
// WebSearchTool.Register).
func (t *WebFetchTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- code execution -----------------------------------------------------------------

// CodeExecutionToolSchema is the provider-neutral code_execution capability
// flag. Anthropic maps it to its code_execution server tool; Gemini to its
// codeExecution tool; the OpenAI Responses code_interpreter mapping is not
// ported.
type CodeExecutionToolSchema struct {
	Version string `json:"version"`
}

// SchemaType returns the provider-neutral tool type.
func (CodeExecutionToolSchema) SchemaType() string { return CodeExecutionToolName }

// CodeExecutionOption configures NewCodeExecution.
type CodeExecutionOption func(*CodeExecutionTool)

// WithCodeExecutionVersion selects the tool version literal.
func WithCodeExecutionVersion(version string) CodeExecutionOption {
	return func(t *CodeExecutionTool) { t.version = &version }
}

// CodeExecutionTool is the hosted code execution capability (Python parity:
// builtin/code_execution.py CodeExecutionTool).
type CodeExecutionTool struct {
	version *string
}

// NewCodeExecution builds the hosted code_execution tool.
func NewCodeExecution(opts ...CodeExecutionOption) *CodeExecutionTool {
	t := &CodeExecutionTool{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *CodeExecutionTool) Name() string { return CodeExecutionToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *CodeExecutionTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *CodeExecutionTool) Schemas(_ *agocontext.Context) ([]any, error) {
	return []any{CodeExecutionToolSchema{
		Version: derefString(t.version, DefaultCodeExecutionVersion),
	}}, nil
}

// Register subscribes nothing: the tool runs at the provider (see
// WebSearchTool.Register).
func (t *CodeExecutionTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- file search --------------------------------------------------------------------

// FileSearchToolSchema is the provider-neutral file_search capability over
// vector stores. VectorStoreIDs and Filters are OpenAI-Responses-only and the
// Responses mapping is not ported; Gemini maps StoreNames (its FileSearchStore
// resource names), MaxNumResults (top_k) and MetadataFilter (an AIP-160 filter
// string) and rejects the rest.
type FileSearchToolSchema struct {
	VectorStoreIDs []string       `json:"vector_store_ids"`
	StoreNames     []string       `json:"store_names"`
	MaxNumResults  *int           `json:"max_num_results,omitempty"`
	MetadataFilter string         `json:"metadata_filter,omitempty"`
	Filters        map[string]any `json:"filters,omitempty"`
	IncludeResults bool           `json:"include_results,omitempty"`
}

// SchemaType returns the provider-neutral tool type.
func (FileSearchToolSchema) SchemaType() string { return FileSearchToolName }

// FileSearchOption configures NewFileSearch.
type FileSearchOption func(*FileSearchTool)

// WithFileSearchVariable binds one schema parameter, named by its JSON field (the Python kwarg
// name), to a Variable resolved from the conversation variables on every turn
// (Python parity: passing a Variable instead of a value). An empty Variable
// name defaults to the parameter name; the variable's default applies when the
// conversation has none. An unknown parameter fails Schemas.
func WithFileSearchVariable(param string, v Variable) FileSearchOption {
	return func(t *FileSearchTool) { t.vars.bind(param, v) }
}

// WithVectorStoreIDs names the OpenAI vector stores to search.
func WithVectorStoreIDs(ids []string) FileSearchOption {
	return func(t *FileSearchTool) { t.vectorStoreIDs = ids }
}

// WithFileSearchStoreNames names the Gemini file search stores to search.
func WithFileSearchStoreNames(names []string) FileSearchOption {
	return func(t *FileSearchTool) { t.storeNames = names }
}

// WithMaxNumResults caps the retrieved chunks.
func WithMaxNumResults(n int) FileSearchOption {
	return func(t *FileSearchTool) { t.maxNumResults = &n }
}

// WithMetadataFilter sets the Gemini AIP-160 filter string.
func WithMetadataFilter(filter string) FileSearchOption {
	return func(t *FileSearchTool) { t.metadataFilter = filter }
}

// WithFileSearchFilters sets the OpenAI comparison/compound filter object.
func WithFileSearchFilters(filters map[string]any) FileSearchOption {
	return func(t *FileSearchTool) { t.filters = filters }
}

// WithIncludeResults asks the API to return the raw retrieved chunks
// (OpenAI Responses include=["file_search_call.results"]).
func WithIncludeResults(include bool) FileSearchOption {
	return func(t *FileSearchTool) { t.includeResults = include }
}

// FileSearchTool is the hosted vector-store retrieval tool (Python parity:
// builtin/file_search.py FileSearchTool).
type FileSearchTool struct {
	vars           hostedVars
	vectorStoreIDs []string
	storeNames     []string
	maxNumResults  *int
	metadataFilter string
	filters        map[string]any
	includeResults bool
}

// NewFileSearch builds the hosted file_search tool.
func NewFileSearch(opts ...FileSearchOption) *FileSearchTool {
	t := &FileSearchTool{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *FileSearchTool) Name() string { return FileSearchToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *FileSearchTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema. VectorStoreIDs and StoreNames keep
// Python's field(default_factory=list): a store list the tool never received
// marshals as [] rather than null.
func (t *FileSearchTool) Schemas(cx *agocontext.Context) ([]any, error) {
	schema, err := t.vars.resolve(FileSearchToolName, cx, FileSearchToolSchema{
		VectorStoreIDs: stringsOrEmpty(t.vectorStoreIDs),
		StoreNames:     stringsOrEmpty(t.storeNames),
		MaxNumResults:  t.maxNumResults,
		MetadataFilter: t.metadataFilter,
		Filters:        t.filters,
		IncludeResults: t.includeResults,
	})
	if err != nil {
		return nil, err
	}
	return []any{schema}, nil
}

// Register subscribes nothing: the tool runs at the provider (see
// WebSearchTool.Register).
func (t *FileSearchTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- image generation -----------------------------------------------------------------

// ImageGenerationToolSchema is the provider-neutral image_generation capability
// (OpenAI Responses API only; not mapped by any ported provider).
type ImageGenerationToolSchema struct {
	Quality           string `json:"quality,omitempty"` // "low" | "medium" | "high" | "auto"
	Size              string `json:"size,omitempty"`    // e.g. "1024x1024", "auto"
	Background        string `json:"background,omitempty"`
	OutputFormat      string `json:"output_format,omitempty"` // "png" | "jpeg" | "webp"
	OutputCompression *int   `json:"output_compression,omitempty"`
	PartialImages     *int   `json:"partial_images,omitempty"`
}

// SchemaType returns the provider-neutral tool type.
func (ImageGenerationToolSchema) SchemaType() string { return ImageGenerationToolName }

// ImageGenerationOption configures NewImageGeneration.
type ImageGenerationOption func(*ImageGenerationTool)

// WithImageGenerationVariable binds one schema parameter, named by its JSON field (the Python kwarg
// name), to a Variable resolved from the conversation variables on every turn
// (Python parity: passing a Variable instead of a value). An empty Variable
// name defaults to the parameter name; the variable's default applies when the
// conversation has none. An unknown parameter fails Schemas.
func WithImageGenerationVariable(param string, v Variable) ImageGenerationOption {
	return func(t *ImageGenerationTool) { t.vars.bind(param, v) }
}

// WithImageQuality sets the image quality: "low", "medium", "high" or "auto".
func WithImageQuality(quality string) ImageGenerationOption {
	return func(t *ImageGenerationTool) { t.quality = &quality }
}

// WithImageSize sets the image dimensions, e.g. "1024x1024" or "auto".
func WithImageSize(size string) ImageGenerationOption {
	return func(t *ImageGenerationTool) { t.size = &size }
}

// WithImageBackground sets the background type: "transparent", "opaque" or "auto".
func WithImageBackground(background string) ImageGenerationOption {
	return func(t *ImageGenerationTool) { t.background = &background }
}

// WithImageOutputFormat sets the output format: "png", "jpeg" or "webp".
func WithImageOutputFormat(format string) ImageGenerationOption {
	return func(t *ImageGenerationTool) { t.outputFormat = &format }
}

// WithImageOutputCompression sets the compression level 0-100 (webp/jpeg only).
func WithImageOutputCompression(n int) ImageGenerationOption {
	return func(t *ImageGenerationTool) { t.outputCompression = &n }
}

// WithPartialImages sets the number of partial images to stream (1-3).
func WithPartialImages(n int) ImageGenerationOption {
	return func(t *ImageGenerationTool) { t.partialImages = &n }
}

// ImageGenerationTool is the hosted image generation tool (Python parity:
// builtin/image_generation.py ImageGenerationTool).
type ImageGenerationTool struct {
	vars              hostedVars
	quality           *string
	size              *string
	background        *string
	outputFormat      *string
	outputCompression *int
	partialImages     *int
}

// NewImageGeneration builds the hosted image_generation tool.
func NewImageGeneration(opts ...ImageGenerationOption) *ImageGenerationTool {
	t := &ImageGenerationTool{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *ImageGenerationTool) Name() string { return ImageGenerationToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *ImageGenerationTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *ImageGenerationTool) Schemas(cx *agocontext.Context) ([]any, error) {
	schema, err := t.vars.resolve(ImageGenerationToolName, cx, ImageGenerationToolSchema{
		Quality:           derefString(t.quality),
		Size:              derefString(t.size),
		Background:        derefString(t.background),
		OutputFormat:      derefString(t.outputFormat),
		OutputCompression: t.outputCompression,
		PartialImages:     t.partialImages,
	})
	if err != nil {
		return nil, err
	}
	return []any{schema}, nil
}

// Register subscribes nothing: the tool runs at the provider (see
// WebSearchTool.Register).
func (t *ImageGenerationTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- shell ---------------------------------------------------------------------------

// NetworkPolicy is the outbound network allowlist of a hosted shell container.
type NetworkPolicy struct {
	AllowedDomains []string `json:"allowed_domains"`
}

// ContainerAuto selects the container the provider provisions and manages.
type ContainerAuto struct {
	NetworkPolicy *NetworkPolicy `json:"network_policy,omitempty"`
}

// ContainerReference names an existing container by ID; the network policy was
// fixed when the container was created.
type ContainerReference struct {
	ContainerID string `json:"container_id"`
}

// ShellEnvironment selects where a hosted shell executes: Auto for a
// provider-managed container or Reference for an existing one. Python carries
// the same union as ContainerAutoEnvironment | ContainerReferenceEnvironment.
type ShellEnvironment struct {
	Auto      *ContainerAuto      `json:"auto,omitempty"`
	Reference *ContainerReference `json:"reference,omitempty"`
}

// ShellToolSchema is the provider-neutral shell capability. Only the OpenAI
// Responses API executes shell server-side (mapping not ported); Anthropic's
// bash tool is client-side and every ported mapper rejects this schema.
type ShellToolSchema struct {
	Version     string            `json:"version"`
	Environment *ShellEnvironment `json:"environment,omitempty"`
}

// SchemaType returns the provider-neutral tool type.
func (ShellToolSchema) SchemaType() string { return ShellToolName }

// ShellOption configures NewShell.
type ShellOption func(*ShellTool)

// WithShellVariable binds one schema parameter, named by its JSON field (the Python kwarg
// name), to a Variable resolved from the conversation variables on every turn
// (Python parity: passing a Variable instead of a value). An empty Variable
// name defaults to the parameter name; the variable's default applies when the
// conversation has none. An unknown parameter fails Schemas.
func WithShellVariable(param string, v Variable) ShellOption {
	return func(t *ShellTool) { t.vars.bind(param, v) }
}

// WithShellEnvironment sets the container the shell runs in.
func WithShellEnvironment(env ShellEnvironment) ShellOption {
	return func(t *ShellTool) { t.environment = &env }
}

// WithShellVersion selects the tool version literal.
func WithShellVersion(version string) ShellOption {
	return func(t *ShellTool) { t.version = &version }
}

// ShellTool is the provider-executed shell tool (Python parity: builtin/shell.py
// ShellTool).
type ShellTool struct {
	vars        hostedVars
	environment *ShellEnvironment
	version     *string
}

// NewShell builds the hosted shell tool.
func NewShell(opts ...ShellOption) *ShellTool {
	t := &ShellTool{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *ShellTool) Name() string { return ShellToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *ShellTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *ShellTool) Schemas(cx *agocontext.Context) ([]any, error) {
	schema, err := t.vars.resolve(ShellToolName, cx, ShellToolSchema{
		Version:     derefString(t.version, ShellVersion),
		Environment: t.environment,
	})
	if err != nil {
		return nil, err
	}
	return []any{schema}, nil
}

// Register subscribes nothing: the tool runs at the provider (see
// WebSearchTool.Register).
func (t *ShellTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- memory --------------------------------------------------------------------------

// MemoryToolSchema is the provider-neutral memory capability. The memory tool
// is client-side: the application implements the commands (view, create,
// str_replace, insert, delete, rename); the schema only declares the tool.
type MemoryToolSchema struct {
	Version string `json:"version"`
}

// SchemaType returns the provider-neutral tool type.
func (MemoryToolSchema) SchemaType() string { return MemoryToolName }

// MemoryOption configures NewMemory.
type MemoryOption func(*MemoryTool)

// WithMemoryVersion selects the tool version literal.
func WithMemoryVersion(version string) MemoryOption {
	return func(t *MemoryTool) { t.version = &version }
}

// MemoryTool is the memory tool carrier (Python parity: builtin/memory.py
// MemoryTool).
type MemoryTool struct {
	version *string
}

// NewMemory builds the memory tool carrier.
func NewMemory(opts ...MemoryOption) *MemoryTool {
	t := &MemoryTool{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *MemoryTool) Name() string { return MemoryToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *MemoryTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *MemoryTool) Schemas(_ *agocontext.Context) ([]any, error) {
	return []any{MemoryToolSchema{
		Version: derefString(t.version, MemoryVersion),
	}}, nil
}

// Register subscribes nothing (see WebSearchTool.Register).
func (t *MemoryTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- MCP server ----------------------------------------------------------------------

// MCPServerToolSchema declares one remote MCP server the provider connects to.
// The headers travel with the (unported) OpenAI Responses mapping; Anthropic
// maps the server into its mcp_servers request parameter plus an mcp_toolset
// tools[] entry.
type MCPServerToolSchema struct {
	ServerURL          string            `json:"server_url"`
	ServerLabel        string            `json:"server_label"`
	AuthorizationToken string            `json:"authorization_token,omitempty"`
	Description        string            `json:"description,omitempty"`
	AllowedTools       []string          `json:"allowed_tools,omitempty"`
	BlockedTools       []string          `json:"blocked_tools,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
}

// SchemaType returns the provider-neutral tool type.
func (MCPServerToolSchema) SchemaType() string { return MCPServerToolName }

// mcpSecretMask replaces secret values in a serialized hosted MCP schema (the
// same convention as the push-secret mask in the a2a package).
const mcpSecretMask = "***"

// MarshalJSON renders the schema's diagnostic form: the bearer token and every
// header value are masked, so an event or log that serializes the schema never
// carries the MCP server's credentials. Header keys stay visible — they are what
// diagnostics need; the values are where credentials ride, so all of them are
// masked, not only obviously secret names. The provider wire request is
// unaffected: the provider mappers read the struct fields directly and never
// marshal the schema.
//
// AGO-DEVIATION: Python serializes this schema with plaintext secrets; AGo masks
// them because schemas travel as opaque values that any event/log surface may
// serialize. See docs/DECISIONS.md.
func (s MCPServerToolSchema) MarshalJSON() ([]byte, error) {
	// Alias type so json.Marshal does not recurse into this method.
	type plain MCPServerToolSchema
	out := plain(s)
	if s.AuthorizationToken != "" {
		out.AuthorizationToken = mcpSecretMask
	}
	if s.Headers != nil {
		masked := make(map[string]string, len(s.Headers))
		for k := range s.Headers {
			masked[k] = mcpSecretMask
		}
		out.Headers = masked
	}
	return json.Marshal(out)
}

// MCPServerOption configures NewMCPServer.
type MCPServerOption func(*MCPServerTool)

// WithMCPServerVariable binds one schema parameter, named by its JSON field (the Python kwarg
// name), to a Variable resolved from the conversation variables on every turn
// (Python parity: passing a Variable instead of a value). An empty Variable
// name defaults to the parameter name; the variable's default applies when the
// conversation has none. An unknown parameter fails Schemas.
func WithMCPServerVariable(param string, v Variable) MCPServerOption {
	return func(t *MCPServerTool) { t.vars.bind(param, v) }
}

// WithAuthorizationToken sets the bearer token sent to the server.
func WithAuthorizationToken(token string) MCPServerOption {
	return func(t *MCPServerTool) { t.authorizationToken = token }
}

// WithMCPServerDescription sets the server description shown to the model.
func WithMCPServerDescription(desc string) MCPServerOption {
	return func(t *MCPServerTool) { t.description = desc }
}

// WithAllowedTools restricts the server to the named tools.
func WithAllowedTools(toolNames []string) MCPServerOption {
	return func(t *MCPServerTool) { t.allowedTools = toolNames }
}

// WithBlockedTools excludes the named tools from the server.
func WithBlockedTools(toolNames []string) MCPServerOption {
	return func(t *MCPServerTool) { t.blockedTools = toolNames }
}

// WithMCPServerHeaders sets extra headers sent to the server.
func WithMCPServerHeaders(headers map[string]string) MCPServerOption {
	return func(t *MCPServerTool) { t.headers = headers }
}

// MCPServerTool is the remote-MCP-server carrier (Python parity:
// builtin/mcp_server.py MCPServerTool). Server URL and label are required.
type MCPServerTool struct {
	vars               hostedVars
	serverURL          string
	serverLabel        string
	authorizationToken string
	description        string
	allowedTools       []string
	blockedTools       []string
	headers            map[string]string
}

// NewMCPServer builds the MCP server carrier; url and label are required
// (Python parity: the constructor's required kwargs).
func NewMCPServer(serverURL, serverLabel string, opts ...MCPServerOption) (*MCPServerTool, error) {
	t := &MCPServerTool{serverURL: serverURL, serverLabel: serverLabel}
	for _, o := range opts {
		o(t)
	}
	// A parameter bound to a Variable is resolved per turn, so it may be empty
	// here (Python parity: server_url and server_label accept a Variable).
	if serverURL == "" && !t.vars.has("server_url") {
		return nil, fmt.Errorf("tools: mcp_server tool requires a server url")
	}
	if serverLabel == "" && !t.vars.has("server_label") {
		return nil, fmt.Errorf("tools: mcp_server tool requires a server label")
	}
	return t, nil
}

// Name returns the tool type name.
func (t *MCPServerTool) Name() string { return MCPServerToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *MCPServerTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *MCPServerTool) Schemas(cx *agocontext.Context) ([]any, error) {
	schema, err := t.vars.resolve(MCPServerToolName, cx, MCPServerToolSchema{
		ServerURL:          t.serverURL,
		ServerLabel:        t.serverLabel,
		AuthorizationToken: t.authorizationToken,
		Description:        t.description,
		AllowedTools:       copyStrings(t.allowedTools),
		BlockedTools:       copyStrings(t.blockedTools),
		Headers:            t.headers,
	})
	if err != nil {
		return nil, err
	}
	return []any{schema}, nil
}

// Register subscribes nothing: the provider connects to the server (see
// WebSearchTool.Register).
func (t *MCPServerTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- tool search -----------------------------------------------------------------------

// ToolSearchToolSchema is the server-side tool_search capability over a set of
// deferred tools. Mode selects Anthropic's variant ("regex" or "bm25"); OpenAI
// exposes a single tool-search tool and ignores the mode.
type ToolSearchToolSchema struct {
	Mode string `json:"mode"`
}

// SchemaType returns the provider-neutral tool type.
func (ToolSearchToolSchema) SchemaType() string { return ToolSearchToolName }

// ToolSearchTool defers its member tools behind the server-side tool_search
// tool: their schemas are contributed with defer_loading set, so the model
// discovers them on demand instead of the framework sending full definitions
// upfront (Python parity: builtin/tool_search.py ToolSearchTool).
//
// The carrier is immutable after construction and safe for concurrent use.
type ToolSearchTool struct {
	mode   string
	tools  []Tool
	byName map[string]Tool
}

// NewToolSearch defers the given tools behind tool search; at least one tool is
// required and member names must be unique (Python parity: ValueError and
// ToolConflictError). mode is "regex" or "bm25" (Anthropic's variants; other
// values travel as written and map to the regex variant).
func NewToolSearch(mode string, ts ...Tool) (*ToolSearchTool, error) {
	if len(ts) == 0 {
		return nil, fmt.Errorf("tools: tool_search requires at least one tool to defer; " +
			"pass the tools to search over, e.g. NewToolSearch(\"regex\", toolA, toolB)")
	}
	t := &ToolSearchTool{mode: mode, byName: map[string]Tool{}}
	for _, m := range ts {
		if m == nil {
			return nil, fmt.Errorf("tools: tool_search member must not be nil")
		}
		if _, dup := t.byName[m.Name()]; dup {
			return nil, fmt.Errorf("%w: could not add tool `%s`: tool with such name already registered", ErrConflict, m.Name())
		}
		t.byName[m.Name()] = m
		t.tools = append(t.tools, m)
	}
	return t, nil
}

// Tools returns the deferred members in declaration order (Python parity: the
// tools property).
func (t *ToolSearchTool) Tools() []Tool { return append([]Tool(nil), t.tools...) }

// Name returns the tool type name.
func (t *ToolSearchTool) Name() string { return ToolSearchToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *ToolSearchTool) DeclaredInCode() bool { return true }

// Schemas returns the tool_search schema followed by the members' schemas: a
// function schema is copied with DeferLoading set (the member's own schema is
// never mutated); any other schema passes through unchanged (Python parity).
func (t *ToolSearchTool) Schemas(cx *agocontext.Context) ([]any, error) {
	out := make([]any, 0, 1+len(t.tools))
	out = append(out, ToolSearchToolSchema{Mode: t.mode})
	for _, m := range t.tools {
		ss, err := m.Schemas(cx)
		if err != nil {
			return nil, fmt.Errorf("tools: tool_search member %s: %w", m.Name(), err)
		}
		for _, s := range ss {
			switch sch := s.(type) {
			case Schema:
				sch.DeferLoading = true
				out = append(out, sch)
			case *Schema:
				if sch == nil {
					return nil, fmt.Errorf("tools: tool_search member %s: nil function schema", m.Name())
				}
				cp := *sch
				cp.DeferLoading = true
				out = append(out, cp)
			default:
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// Register registers the deferred members (their calls are answered client-side
// once the provider expands a match) and subscribes nothing for itself: the
// search runs at the provider (see WebSearchTool.Register).
func (t *ToolSearchTool) Register(cx *agocontext.Context, mw []middleware.Middleware) ([]stream.SubID, error) {
	var subs []stream.SubID
	for _, m := range t.tools {
		ids, err := m.Register(cx, mw)
		if err != nil {
			for _, id := range subs {
				cx.Stream.Unsubscribe(id)
			}
			return nil, fmt.Errorf("tools: register tool_search member %s: %w", m.Name(), err)
		}
		subs = append(subs, ids...)
	}
	return subs, nil
}

// --- retrieval ---------------------------------------------------------------------

// RetrievalToolSchema is the provider-neutral knowledge-base retrieval
// capability. Only Z.AI executes it server-side (Python parity: the Z.AI
// mapper); every other provider rejects the schema.
type RetrievalToolSchema struct {
	// KnowledgeID names the Z.AI knowledge base to search.
	KnowledgeID string `json:"knowledge_id"`
	// PromptTemplate overrides the template that frames the retrieved
	// passages; empty is not sent and the API's own default applies.
	// AGO-DEVIATION: Python's field is `str | None`, which distinguishes
	// None from ""; the Go string cannot, and an empty template is not
	// meaningful to the API, so "" means unset.
	PromptTemplate string `json:"prompt_template,omitempty"`
}

// SchemaType returns the provider-neutral tool type.
func (RetrievalToolSchema) SchemaType() string { return RetrievalToolName }

// RetrievalOption configures NewRetrieval.
type RetrievalOption func(*RetrievalTool)

// WithRetrievalVariable binds one schema parameter, named by its JSON field (the Python kwarg
// name), to a Variable resolved from the conversation variables on every turn
// (Python parity: passing a Variable instead of a value). An empty Variable
// name defaults to the parameter name; the variable's default applies when the
// conversation has none. An unknown parameter fails Schemas.
func WithRetrievalVariable(param string, v Variable) RetrievalOption {
	return func(t *RetrievalTool) { t.vars.bind(param, v) }
}

// WithRetrievalPromptTemplate sets the template that frames the retrieved
// passages in the prompt.
func WithRetrievalPromptTemplate(template string) RetrievalOption {
	return func(t *RetrievalTool) { t.promptTemplate = template }
}

// RetrievalTool is the Z.AI knowledge-base retrieval tool (Python parity:
// builtin/retrieval.py RetrievalTool; Z.AI-specific, unsupported by other
// providers).
type RetrievalTool struct {
	vars           hostedVars
	knowledgeID    string
	promptTemplate string
}

// NewRetrieval builds the hosted retrieval tool for one knowledge base.
func NewRetrieval(knowledgeID string, opts ...RetrievalOption) *RetrievalTool {
	t := &RetrievalTool{knowledgeID: knowledgeID}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *RetrievalTool) Name() string { return RetrievalToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *RetrievalTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *RetrievalTool) Schemas(cx *agocontext.Context) ([]any, error) {
	schema, err := t.vars.resolve(RetrievalToolName, cx, RetrievalToolSchema{
		KnowledgeID:    t.knowledgeID,
		PromptTemplate: t.promptTemplate,
	})
	if err != nil {
		return nil, err
	}
	return []any{schema}, nil
}

// Register subscribes nothing: the tool runs at the provider (see
// WebSearchTool.Register).
func (t *RetrievalTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// --- google maps ---------------------------------------------------------------------

// GoogleMapsToolName is the provider-neutral tool type of Maps grounding.
const GoogleMapsToolName = "google_maps"

// GoogleMapsToolSchema is the provider-neutral Google Maps grounding
// capability. Only Gemini (3 family) executes it server-side: Latitude and
// Longitude bias results through the request's tool_config.retrieval_config,
// LanguageCode localises them, EnableWidget requests the interactive Maps
// widget context token. A nil field is not sent and the API's own default
// applies; every other provider rejects the schema.
type GoogleMapsToolSchema struct {
	Latitude     *float64 `json:"latitude,omitempty"`
	Longitude    *float64 `json:"longitude,omitempty"`
	LanguageCode string   `json:"language_code,omitempty"`
	EnableWidget bool     `json:"enable_widget"`
}

// SchemaType returns the provider-neutral tool type.
func (GoogleMapsToolSchema) SchemaType() string { return GoogleMapsToolName }

// GoogleMapsTool is the Gemini Maps-grounding carrier (Python parity:
// builtin/google_maps.py GoogleMapsTool).
type GoogleMapsTool struct {
	vars         hostedVars
	latitude     *float64
	longitude    *float64
	languageCode string
	enableWidget bool
}

// GoogleMapsOption configures NewGoogleMaps.
type GoogleMapsOption func(*GoogleMapsTool)

// WithGoogleMapsLocation biases results to a latitude/longitude. Gemini applies
// the bias only when both are present.
func WithGoogleMapsLocation(latitude, longitude float64) GoogleMapsOption {
	return func(t *GoogleMapsTool) { t.latitude, t.longitude = &latitude, &longitude }
}

// WithGoogleMapsLanguageCode localises results (an IETF language code).
func WithGoogleMapsLanguageCode(code string) GoogleMapsOption {
	return func(t *GoogleMapsTool) { t.languageCode = code }
}

// WithGoogleMapsWidget requests the interactive Maps widget context token.
func WithGoogleMapsWidget(enabled bool) GoogleMapsOption {
	return func(t *GoogleMapsTool) { t.enableWidget = enabled }
}

// WithGoogleMapsVariable binds one parameter ("latitude", "longitude",
// "language_code" or "enable_widget") to a Variable resolved on every turn (see
// WithWebSearchVariable).
func WithGoogleMapsVariable(param string, v Variable) GoogleMapsOption {
	return func(t *GoogleMapsTool) { t.vars.bind(param, v) }
}

// NewGoogleMaps builds the hosted Maps-grounding tool.
func NewGoogleMaps(opts ...GoogleMapsOption) *GoogleMapsTool {
	t := &GoogleMapsTool{}
	for _, o := range opts {
		o(t)
	}
	return t
}

// Name returns the tool type name.
func (t *GoogleMapsTool) Name() string { return GoogleMapsToolName }

// DeclaredInCode is always true: hosted tools come from the program.
func (t *GoogleMapsTool) DeclaredInCode() bool { return true }

// Schemas returns the tool's single schema.
func (t *GoogleMapsTool) Schemas(cx *agocontext.Context) ([]any, error) {
	schema, err := t.vars.resolve(GoogleMapsToolName, cx, GoogleMapsToolSchema{
		Latitude:     copyFloat(t.latitude),
		Longitude:    copyFloat(t.longitude),
		LanguageCode: t.languageCode,
		EnableWidget: t.enableWidget,
	})
	if err != nil {
		return nil, err
	}
	return []any{schema}, nil
}

// Register subscribes nothing: the tool runs at the provider (see
// WebSearchTool.Register).
func (t *GoogleMapsTool) Register(_ *agocontext.Context, _ []middleware.Middleware) ([]stream.SubID, error) {
	return nil, nil
}

// copyFloat clones an optional float so a schema snapshot cannot alias the option.
func copyFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// --- per-turn Variable kwargs -------------------------------------------------------

// hostedVars binds schema parameters to conversation Variables. The zero value
// is ready to use; carriers embed it.
type hostedVars struct {
	bound map[string]Variable
}

// bind records that param (the schema's JSON field name, e.g. "max_uses") is
// resolved from v on every Schemas call.
func (h *hostedVars) bind(param string, v Variable) {
	if h.bound == nil {
		h.bound = map[string]Variable{}
	}
	h.bound[param] = v
}

// has reports whether param is bound to a Variable.
func (h *hostedVars) has(param string) bool {
	_, ok := h.bound[param]
	return ok
}

// resolve returns schema (a struct value) with every bound parameter replaced
// by its resolved variable (Python parity: resolve_variable — the variable's
// name, or the parameter name when the Variable has none; then its default;
// otherwise an error). A nil context resolves through defaults only.
func (h *hostedVars) resolve(tool string, cx *agocontext.Context, schema any) (any, error) {
	if len(h.bound) == 0 {
		return schema, nil
	}
	rv := reflect.ValueOf(schema)
	out := reflect.New(rv.Type()).Elem()
	out.Set(rv)
	params := make([]string, 0, len(h.bound))
	for p := range h.bound {
		params = append(params, p)
	}
	sort.Strings(params)
	for _, param := range params {
		field, ok := fieldByJSONName(out, param)
		if !ok {
			return nil, fmt.Errorf("tools: %s: variable bound to unknown parameter %q", tool, param)
		}
		v := h.bound[param]
		key := v.Name
		if key == "" {
			key = param
		}
		var (
			val   any
			found bool
		)
		if cx != nil {
			val, found = cx.Variable(key)
		}
		if !found {
			if !v.HasDefault {
				return nil, fmt.Errorf("%w: tool %q: variable %q for parameter %q is not provided and has no default",
					ErrNotFound, tool, key, param)
			}
			val = v.Default
		}
		// Convert through JSON so a variable holding any equivalent shape (a
		// decoded map, a plain string) lands in the schema field's own type.
		b, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("tools: %s: variable %q for parameter %q: %w", tool, key, param, err)
		}
		dst := reflect.New(field.Type())
		if err := json.Unmarshal(b, dst.Interface()); err != nil {
			return nil, fmt.Errorf("tools: %s: variable %q for parameter %q has the wrong type: %w", tool, key, param, err)
		}
		field.Set(dst.Elem())
	}
	return out.Interface(), nil
}

// fieldByJSONName finds the settable struct field whose JSON name is name.
func fieldByJSONName(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if tag == name && v.Field(i).CanSet() {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// --- small helpers ----------------------------------------------------------------

// derefString returns *p when set, or def when p is nil (or "" — an unset
// optional string and an empty one are the same for these schemas).
func derefString(p *string, def ...string) string {
	if p != nil && *p != "" {
		return *p
	}
	if len(def) > 0 {
		return def[0]
	}
	if p != nil {
		return *p
	}
	return ""
}

// copyStrings clones a slice so a stored option cannot be mutated through the
// schema snapshot (nil stays nil: unset and empty are distinct).
func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

// stringsOrEmpty clones a slice, mapping nil to the empty list (Python parity:
// field(default_factory=list) — a list field is always present, never null).
func stringsOrEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return append([]string(nil), in...)
}
