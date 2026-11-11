package config_test

import (
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/config/anthropic"
	"github.com/luckystrker/ago/config/bedrock"
	"github.com/luckystrker/ago/config/dashscope"
	"github.com/luckystrker/ago/config/gemini"
	"github.com/luckystrker/ago/config/mistral"
	"github.com/luckystrker/ago/config/ollama"
	"github.com/luckystrker/ago/config/openai"
	"github.com/luckystrker/ago/config/typesafe"
	"github.com/luckystrker/ago/config/xai"
	"github.com/luckystrker/ago/config/zai"
	"github.com/luckystrker/ago/events"
)

// stubConfig stands in for a provider config the tree has no implementation
// for yet (Vertex AI) and for the provider-neutral lookups below.
type stubConfig struct {
	provider config.Provider
	dialect  config.InputDialect
}

func (s stubConfig) Provider() config.Provider { return s.provider }
func (s stubConfig) Model() string             { return "stub" }
func (s stubConfig) Create() (config.LLMClient, error) {
	return nil, config.ErrNotImplemented
}
func (s stubConfig) Clone(map[string]any) (config.ModelConfig, error) { return s, nil }
func (s stubConfig) InputDialect() config.InputDialect                { return s.dialect }

var _ config.ModelConfig = stubConfig{}

// providerConfigs maps each dialect of the Python table to a Go config. Vertex
// AI has no Go config yet, so it is queried through a stub carrying its
// provider value.
func providerConfigs() map[config.InputDialect]config.ModelConfig {
	return map[config.InputDialect]config.ModelConfig{
		config.DialectOpenAICompletions: openai.NewOpenAICompletions("gpt-test"),
		config.DialectOpenAIResponses:   openai.NewOpenAIResponses("gpt-test"),
		config.DialectAnthropic:         anthropic.NewAnthropic("claude-test"),
		config.DialectBedrock:           bedrock.NewBedrock("bedrock-test"),
		config.DialectDashScope:         dashscope.NewDashScope("qwen-test"),
		config.DialectGemini:            gemini.NewGemini("key", "gemini-test"),
		config.DialectVertexAI:          stubConfig{provider: config.ProviderVertexAI, dialect: config.DialectVertexAI},
		config.DialectMistral:           mistral.NewMistral("mistral-test"),
		config.DialectOllama:            ollama.NewOllama("llama-test"),
		config.DialectXAI:               xai.NewXAI("grok-test"),
		config.DialectZAI:               zai.NewZAI("glm-test"),
		config.DialectTypeSafe:          typesafe.NewTypeSafe("ts-test"),
	}
}

type tripleKey struct {
	position config.InputPosition
	source   config.InputSource
	kind     events.BinaryType
}

// add inserts every combination into set.
func add(set map[tripleKey]bool, positions []config.InputPosition, sources []config.InputSource, kinds ...events.BinaryType) {
	for _, p := range positions {
		for _, s := range sources {
			for _, k := range kinds {
				set[tripleKey{p, s, k}] = true
			}
		}
	}
}

// addFile records the provider-file rows: Python's ("file", None) entries. The
// kind is written out here on purpose — the oracle must not borrow the
// implementation's convention for a kind-less row, or a bug in that convention
// would be invisible to it.
func addFile(set map[tripleKey]bool, positions ...config.InputPosition) {
	for _, p := range positions {
		set[tripleKey{p, config.SourceFile, ""}] = true
	}
}

// acceptedByPython is ag2/config/input_acceptance.py `_MEDIA`, transcribed
// verbatim. It is the oracle for the Go table: every entry and every absence
// is checked.
func acceptedByPython() map[config.InputDialect]map[tripleKey]bool {
	user := []config.InputPosition{config.PositionUser}
	tool := []config.InputPosition{config.PositionTool}
	both := []config.InputPosition{config.PositionUser, config.PositionTool}
	data := []config.InputSource{config.SourceData}
	dataURL := []config.InputSource{config.SourceData, config.SourceURL}
	table := make(map[config.InputDialect]map[tripleKey]bool)

	completions := make(map[tripleKey]bool)
	add(completions, user, data, events.BinaryImage)
	add(completions, user, []config.InputSource{config.SourceURL}, events.BinaryImage)
	add(completions, user, data, events.BinaryAudio, events.BinaryDocument)
	addFile(completions, config.PositionUser)
	table[config.DialectOpenAICompletions] = completions

	responses := make(map[tripleKey]bool)
	add(responses, both, dataURL, events.BinaryImage, events.BinaryDocument)
	addFile(responses, config.PositionUser, config.PositionTool)
	table[config.DialectOpenAIResponses] = responses

	anthropicTable := make(map[tripleKey]bool)
	add(anthropicTable, both, dataURL, events.BinaryImage, events.BinaryDocument)
	addFile(anthropicTable, config.PositionUser, config.PositionTool)
	table[config.DialectAnthropic] = anthropicTable

	bedrockTable := make(map[tripleKey]bool)
	add(bedrockTable, both, data, events.BinaryImage, events.BinaryDocument, events.BinaryVideo)
	table[config.DialectBedrock] = bedrockTable

	dashscopeTable := make(map[tripleKey]bool)
	add(dashscopeTable, both, dataURL, events.BinaryImage)
	table[config.DialectDashScope] = dashscopeTable

	geminiTable := make(map[tripleKey]bool)
	add(geminiTable, user, dataURL, events.BinaryImage, events.BinaryAudio, events.BinaryVideo, events.BinaryDocument)
	add(geminiTable, tool, dataURL, events.BinaryImage, events.BinaryDocument)
	addFile(geminiTable, config.PositionUser)
	table[config.DialectGemini] = geminiTable
	table[config.DialectVertexAI] = geminiTable

	mistralTable := make(map[tripleKey]bool)
	add(mistralTable, both, dataURL, events.BinaryImage, events.BinaryDocument)
	addFile(mistralTable, config.PositionUser, config.PositionTool)
	table[config.DialectMistral] = mistralTable

	ollamaTable := make(map[tripleKey]bool)
	add(ollamaTable, user, data, events.BinaryImage)
	table[config.DialectOllama] = ollamaTable

	xaiTable := make(map[tripleKey]bool)
	add(xaiTable, user, dataURL, events.BinaryImage, events.BinaryDocument)
	addFile(xaiTable, config.PositionUser)
	table[config.DialectXAI] = xaiTable

	table[config.DialectZAI] = map[tripleKey]bool{}

	typesafeTable := make(map[tripleKey]bool)
	add(typesafeTable, both, data, events.BinaryImage)
	table[config.DialectTypeSafe] = typesafeTable

	return table
}

// everyTriple enumerates the key space the table can describe. A data or URL
// source carries one of the binary kinds; a provider-file source carries none
// (Python's ("file", None)), so it has exactly one key per position.
func everyTriple() []tripleKey {
	positions := []config.InputPosition{config.PositionUser, config.PositionTool}
	mediaSources := []config.InputSource{config.SourceData, config.SourceURL}
	kinds := []events.BinaryType{events.BinaryImage, events.BinaryAudio, events.BinaryVideo, events.BinaryDocument}
	var out []tripleKey
	for _, p := range positions {
		for _, s := range mediaSources {
			for _, k := range kinds {
				out = append(out, tripleKey{p, s, k})
			}
		}
		out = append(out, tripleKey{p, config.SourceFile, ""})
	}
	return out
}

// partFor builds the part the key describes; the media type is the canonical
// one of the kind and is in every dialect's allowlist, so the per-dialect MIME
// filters do not interfere with the (position, source, kind) matrix.
func partFor(key tripleKey) events.Input {
	switch key.source {
	case config.SourceURL:
		return &events.UrlInput{URL: "https://example.com/x", Kind: key.kind}
	case config.SourceFile:
		return &events.FileIdInput{FileID: "file-1"}
	default:
		mediaType := map[events.BinaryType]string{
			events.BinaryImage:    "image/png",
			events.BinaryAudio:    "audio/wav",
			events.BinaryVideo:    "video/mp4",
			events.BinaryDocument: "application/pdf",
		}[key.kind]
		if mediaType == "" {
			mediaType = "image/png"
		}
		return &events.BinaryInput{Data: []byte{1}, MediaType: mediaType, Kind: key.kind}
	}
}

func TestAcceptsInputMatchesPythonTable(t *testing.T) {
	cfgs := providerConfigs()
	accepted := acceptedByPython()
	if len(cfgs) != len(accepted) {
		t.Fatalf("configs cover %d dialects, table describes %d", len(cfgs), len(accepted))
	}
	for dialect, cfg := range cfgs {
		want, ok := accepted[dialect]
		if !ok {
			t.Fatalf("dialect %q is not in the Python table", dialect)
		}
		for _, key := range everyTriple() {
			got := config.AcceptsInput(cfg, key.position, partFor(key))
			if got != want[key] {
				t.Errorf("%s: (%s, %s, %q) accepts=%v, want %v", dialect, key.position, key.source, key.kind, got, want[key])
			}
		}
	}
}

func TestAcceptanceMediaTypeFilters(t *testing.T) {
	cfgs := providerConfigs()
	cases := []struct {
		name      string
		dialect   config.InputDialect
		kind      events.BinaryType
		mediaType string
		want      bool
	}{
		{"openai audio wav", config.DialectOpenAICompletions, events.BinaryAudio, "audio/wav", true},
		{"openai audio mpeg", config.DialectOpenAICompletions, events.BinaryAudio, "audio/mpeg", true},
		{"openai audio mp3", config.DialectOpenAICompletions, events.BinaryAudio, "audio/mp3", true},
		{"openai audio ogg", config.DialectOpenAICompletions, events.BinaryAudio, "audio/ogg", false},
		{"openai document any", config.DialectOpenAICompletions, events.BinaryDocument, "application/x-thing", true},
		{"anthropic image webp", config.DialectAnthropic, events.BinaryImage, "image/webp", true},
		{"anthropic image heic", config.DialectAnthropic, events.BinaryImage, "image/heic", false},
		{"anthropic document pdf", config.DialectAnthropic, events.BinaryDocument, "application/pdf", true},
		{"anthropic document html", config.DialectAnthropic, events.BinaryDocument, "text/html", false},
		{"bedrock image png", config.DialectBedrock, events.BinaryImage, "image/png", true},
		{"bedrock image heif", config.DialectBedrock, events.BinaryImage, "image/heif", false},
		{"bedrock document csv", config.DialectBedrock, events.BinaryDocument, "text/csv", true},
		{"bedrock document json", config.DialectBedrock, events.BinaryDocument, "application/json", false},
		{"bedrock video mp4", config.DialectBedrock, events.BinaryVideo, "video/mp4", true},
		{"bedrock video hevc", config.DialectBedrock, events.BinaryVideo, "video/hevc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			part := &events.BinaryInput{Data: []byte{1}, MediaType: tc.mediaType, Kind: tc.kind}
			if got := config.AcceptsInput(cfgs[tc.dialect], config.PositionUser, part); got != tc.want {
				t.Fatalf("accepts %s/%s = %v, want %v", tc.kind, tc.mediaType, got, tc.want)
			}
		})
	}
}

func TestAcceptsInputUndescribedConfigs(t *testing.T) {
	for name, cfg := range map[string]config.ModelConfig{
		"nil":       nil,
		"decisions": openai.NewOpenAIDecisions(),
	} {
		for _, key := range everyTriple() {
			if !config.AcceptsInput(cfg, key.position, partFor(key)) {
				t.Fatalf("%s: an undescribed config must accept every part", name)
			}
		}
	}
}

func TestAcceptsInputTextAndDataAlways(t *testing.T) {
	zai := zai.NewZAI("glm-test")
	for _, part := range []events.Input{
		&events.TextInput{Content: "hi"},
		&events.DataInput{Data: map[string]any{"k": 1}},
	} {
		if !config.AcceptsInput(zai, config.PositionUser, part) {
			t.Fatalf("text and data are accepted everywhere, got false for %T", part)
		}
	}
}

func TestDescribedDialect(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.ModelConfig
		want config.InputDialect
		ok   bool
	}{
		{"completions", openai.NewOpenAICompletions("gpt-test"), config.DialectOpenAICompletions, true},
		{"responses", openai.NewOpenAIResponses("gpt-test"), config.DialectOpenAIResponses, true},
		{"decisions", openai.NewOpenAIDecisions(), "", false},
		{"nil", nil, "", false},
		{"anthropic", anthropic.NewAnthropic("claude-test"), config.DialectAnthropic, true},
		{"bedrock", bedrock.NewBedrock("b"), config.DialectBedrock, true},
		{"dashscope", dashscope.NewDashScope("q"), config.DialectDashScope, true},
		{"gemini", gemini.NewGemini("key", "g"), config.DialectGemini, true},
		{"mistral", mistral.NewMistral("m"), config.DialectMistral, true},
		{"ollama", ollama.NewOllama("o"), config.DialectOllama, true},
		{"xai", xai.NewXAI("x"), config.DialectXAI, true},
		{"zai", zai.NewZAI("z"), config.DialectZAI, true},
		{"typesafe", typesafe.NewTypeSafe("t"), config.DialectTypeSafe, true},
		{"vertexai stub", stubConfig{provider: config.ProviderVertexAI, dialect: config.DialectVertexAI}, config.DialectVertexAI, true},
		{"unknown dialect stub", stubConfig{dialect: "nope"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := config.DescribedDialect(tc.cfg)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("DescribedDialect = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestInputModalities(t *testing.T) {
	cfgs := providerConfigs()
	want := map[config.InputDialect]map[string]bool{
		config.DialectOpenAICompletions: {"image": true, "audio": true, "video": false, "pdf": true},
		config.DialectOpenAIResponses:   {"image": true, "audio": false, "video": false, "pdf": true},
		config.DialectAnthropic:         {"image": true, "audio": false, "video": false, "pdf": true},
		config.DialectBedrock:           {"image": true, "audio": false, "video": true, "pdf": true},
		config.DialectDashScope:         {"image": true, "audio": false, "video": false, "pdf": false},
		config.DialectGemini:            {"image": true, "audio": true, "video": true, "pdf": true},
		config.DialectVertexAI:          {"image": true, "audio": true, "video": true, "pdf": true},
		config.DialectMistral:           {"image": true, "audio": false, "video": false, "pdf": true},
		config.DialectOllama:            {"image": true, "audio": false, "video": false, "pdf": false},
		config.DialectXAI:               {"image": true, "audio": false, "video": false, "pdf": true},
		config.DialectZAI:               {"image": false, "audio": false, "video": false, "pdf": false},
		config.DialectTypeSafe:          {"image": true, "audio": false, "video": false, "pdf": false},
	}
	for dialect, cfg := range cfgs {
		got := config.InputModalities(cfg)
		if len(got) != len(want[dialect]) {
			t.Fatalf("%s: modalities = %v, want %v", dialect, got, want[dialect])
		}
		for name, accepted := range want[dialect] {
			if got[name] != accepted {
				t.Fatalf("%s: modality %q = %v, want %v", dialect, name, got[name], accepted)
			}
		}
	}
	// A config the table does not describe declares nothing: an empty map is
	// not the same promise as four false values.
	if got := config.InputModalities(openai.NewOpenAIDecisions()); len(got) != 0 {
		t.Fatalf("undescribed config must declare no modalities, got %v", got)
	}
}

func TestInputMediaTypes(t *testing.T) {
	all := []string{
		"image/jpeg", "image/png", "image/gif", "image/webp", "image/heic", "image/heif",
		"audio/wav", "audio/mpeg", "audio/ogg", "audio/flac", "audio/aiff", "audio/aac",
		"video/x-matroska", "video/quicktime", "video/mp4", "video/webm",
		"video/x-flv", "video/mpeg", "video/x-ms-wmv", "video/3gpp",
		"application/pdf", "text/plain", "text/csv", "text/tsv", "text/html", "text/markdown",
		"text/xml", "text/css", "text/javascript", "text/calendar", "text/vcard",
		"text/srt", "text/vtt", "application/json", "application/sql", "message/rfc822",
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/rtf", "application/vnd.oasis.opendocument.text",
		"application/vnd.ms-excel",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
	}
	completions := []string{
		"image/jpeg", "image/png", "image/gif", "image/webp", "image/heic", "image/heif",
		"audio/wav", "audio/mpeg",
	}
	completions = append(completions, all[20:]...)

	cases := []struct {
		name string
		cfg  config.ModelConfig
		want []string
	}{
		{"undescribed", openai.NewOpenAIDecisions(), all},
		{"openai completions honours the audio filter", openai.NewOpenAICompletions("gpt"), completions},
		{"anthropic omits video and exotic audio", anthropic.NewAnthropic("claude"), []string{
			"image/jpeg", "image/png", "image/gif", "image/webp",
			"application/pdf", "text/plain",
		}},
		{"zai accepts no media", zai.NewZAI("glm"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := config.InputMediaTypes(tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("media types = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("media types = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestBinaryKindOf(t *testing.T) {
	cases := []struct {
		mediaType string
		want      events.BinaryType
		ok        bool
	}{
		{"image/png", events.BinaryImage, true},
		{"audio/mpeg", events.BinaryAudio, true},
		{"video/mp4", events.BinaryVideo, true},
		{"application/pdf", events.BinaryDocument, true},
		{"message/rfc822", events.BinaryDocument, true},
		{"application/octet-stream", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := config.BinaryKindOf(tc.mediaType)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("BinaryKindOf(%q) = (%q, %v), want (%q, %v)", tc.mediaType, got, ok, tc.want, tc.ok)
		}
	}
}

// TestInputDialectConfigContract keeps the table keys and the Provider values
// aligned: every dialect that names a vendor must equal that vendor's string.
func TestInputDialectConfigContract(t *testing.T) {
	pairs := map[config.InputDialect]config.Provider{
		config.DialectAnthropic: config.ProviderAnthropic,
		config.DialectBedrock:   config.ProviderBedrock,
		config.DialectDashScope: config.ProviderDashScope,
		config.DialectGemini:    config.ProviderGemini,
		config.DialectVertexAI:  config.ProviderVertexAI,
		config.DialectMistral:   config.ProviderMistral,
		config.DialectOllama:    config.ProviderOllama,
		config.DialectXAI:       config.ProviderXAI,
		config.DialectZAI:       config.ProviderZAI,
		config.DialectTypeSafe:  config.ProviderTypeSafe,
	}
	for dialect, provider := range pairs {
		if string(dialect) != string(provider) {
			t.Fatalf("dialect %q does not match provider %q", dialect, provider)
		}
	}
}
