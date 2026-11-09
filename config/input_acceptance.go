package config

import (
	"github.com/luckystrker/ago/events"
)

// Provider-neutral media-input acceptance. Each dialect entry names the
// (position, source, kind) inputs that dialect's mapper accepts; text and data
// parts are accepted everywhere. The table describes mapper behavior, not
// model-specific ability, so a provider may still refuse a part the mapper
// forwards.
//
// Parity with Python AG2 ag2/config/input_acceptance.py (_MEDIA). Nothing in
// this package consults the table: the model clients fail inside their own
// mappers (ErrUnsupportedInput), and the framework surfaces query the table to
// decide what to forward or advertise.
//
// The table keys are wire dialects, not vendors: OpenAI's two protocols differ
// and share one Provider value. A config whose dialect the table does not
// describe (nil, the OpenAI Decisions config, a vendor without a mapper) is
// treated as accepting everything, because its mapper cannot be read from here.

// InputDialect names a mapper's wire dialect in the acceptance table. For every
// vendor but OpenAI it equals the vendor's Provider string.
type InputDialect string

// The dialects the table describes (Python parity: one entry per config class).
const (
	DialectOpenAICompletions InputDialect = "openai-completions"
	DialectOpenAIResponses   InputDialect = "openai-responses"
	DialectAnthropic         InputDialect = "anthropic"
	DialectBedrock           InputDialect = "bedrock"
	DialectDashScope         InputDialect = "dashscope"
	DialectGemini            InputDialect = "gemini"
	DialectVertexAI          InputDialect = "vertexai"
	DialectMistral           InputDialect = "mistral"
	DialectOllama            InputDialect = "ollama"
	DialectXAI               InputDialect = "xai"
	DialectZAI               InputDialect = "zai"
	DialectTypeSafe          InputDialect = "typesafe"
)

// InputPosition is where an input sits in the conversation: a user turn or a
// tool result.
type InputPosition string

// Input positions (Python parity: the "position" column of _MEDIA).
const (
	PositionUser InputPosition = "user"
	PositionTool InputPosition = "tool"
)

// InputSource is how a media part reaches the provider: inline bytes, a URL or
// a previously uploaded file id.
type InputSource string

// Input sources (Python parity: the "source" column of _MEDIA).
const (
	SourceData InputSource = "data"
	SourceURL  InputSource = "url"
	SourceFile InputSource = "file"
)

// InputDialectConfig is implemented by configs whose dialect the table cannot
// infer from Provider() because several protocols share one provider. Both
// OpenAI chat configs implement it; the OpenAI Decisions config deliberately
// does not, so the table treats it as undescribed like Python's `_described`.
type InputDialectConfig interface {
	InputDialect() InputDialect
}

// mediaKey is one row of the table. A file source carries no kind ("").
type mediaKey struct {
	position InputPosition
	source   InputSource
	kind     events.BinaryType
}

// mediaKeys expands positions × sources × kinds into rows.
func mediaKeys(positions []InputPosition, sources []InputSource, kinds ...events.BinaryType) []mediaKey {
	out := make([]mediaKey, 0, len(positions)*len(sources)*len(kinds))
	for _, p := range positions {
		for _, s := range sources {
			for _, k := range kinds {
				out = append(out, mediaKey{position: p, source: s, kind: k})
			}
		}
	}
	return out
}

func acceptanceSet(rows ...[]mediaKey) map[mediaKey]bool {
	out := make(map[mediaKey]bool, len(rows))
	for _, group := range rows {
		for _, k := range group {
			out[k] = true
		}
	}
	return out
}

// acceptanceTable is _MEDIA transcribed from Python. File sources carry the ""
// kind because a file id names content the provider resolves itself.
//
// AGO-DEVIATION: the openai-completions file row has no Go counterpart yet —
// the shared chat engine rejects FileIdInput until the Files API is ported
// (existing deviation on the mapper). The table keeps Python's row so the
// framework side can be ported against the reference behavior.
var acceptanceTable = map[InputDialect]map[mediaKey]bool{
	DialectOpenAICompletions: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser}, []InputSource{SourceData, SourceURL}, events.BinaryImage),
		mediaKeys([]InputPosition{PositionUser}, []InputSource{SourceData}, events.BinaryAudio, events.BinaryDocument),
		mediaKeys([]InputPosition{PositionUser}, []InputSource{SourceFile}),
	),
	DialectOpenAIResponses: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceData, SourceURL}, events.BinaryImage, events.BinaryDocument),
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceFile}),
	),
	DialectAnthropic: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceData, SourceURL}, events.BinaryImage, events.BinaryDocument),
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceFile}),
	),
	DialectBedrock: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceData}, events.BinaryImage, events.BinaryDocument, events.BinaryVideo),
	),
	DialectDashScope: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceData, SourceURL}, events.BinaryImage),
	),
	DialectGemini:   geminiAcceptance(),
	DialectVertexAI: geminiAcceptance(),
	DialectMistral: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceData, SourceURL}, events.BinaryImage, events.BinaryDocument),
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceFile}),
	),
	DialectOllama: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser}, []InputSource{SourceData}, events.BinaryImage),
	),
	DialectXAI: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser}, []InputSource{SourceData, SourceURL}, events.BinaryImage, events.BinaryDocument),
		mediaKeys([]InputPosition{PositionUser}, []InputSource{SourceFile}),
	),
	DialectZAI: acceptanceSet(), // Python: ZAIConfig accepts no media
	DialectTypeSafe: acceptanceSet(
		mediaKeys([]InputPosition{PositionUser, PositionTool}, []InputSource{SourceData}, events.BinaryImage),
	),
}

// geminiAcceptance builds the Gemini and Vertex AI rows, which are identical:
// the user turn takes every media kind, the tool result only images and
// documents, and the user turn additionally takes provider file ids.
func geminiAcceptance() map[mediaKey]bool {
	return acceptanceSet(
		mediaKeys([]InputPosition{PositionUser}, []InputSource{SourceData, SourceURL},
			events.BinaryImage, events.BinaryAudio, events.BinaryVideo, events.BinaryDocument),
		mediaKeys([]InputPosition{PositionTool}, []InputSource{SourceData, SourceURL},
			events.BinaryImage, events.BinaryDocument),
		mediaKeys([]InputPosition{PositionUser}, []InputSource{SourceFile}),
	)
}

// mediaTypeFilters narrows the binary kinds a dialect forwards to a fixed set
// of media types (Python parity: the allowlists inside accepts_input). Only
// inline data is filtered: a URL or file id names content the provider fetches
// and checks itself.
var mediaTypeFilters = map[InputDialect]map[events.BinaryType]map[string]bool{
	DialectBedrock: {
		events.BinaryImage: setOf("image/png", "image/jpeg", "image/gif", "image/webp"),
		events.BinaryDocument: setOf(
			"application/pdf",
			"text/csv",
			"application/msword",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"application/vnd.ms-excel",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			"text/html",
			"text/plain",
			"text/markdown",
		),
		events.BinaryVideo: setOf(
			"video/x-matroska",
			"video/quicktime",
			"video/mp4",
			"video/webm",
			"video/x-flv",
			"video/mpeg",
			"video/x-ms-wmv",
			"video/3gpp",
		),
	},
	DialectOpenAICompletions: {
		events.BinaryAudio: setOf("audio/wav", "audio/mpeg", "audio/mp3"),
	},
	DialectAnthropic: {
		events.BinaryImage:    setOf("image/jpeg", "image/png", "image/gif", "image/webp"),
		events.BinaryDocument: setOf("application/pdf", "text/plain"),
	},
}

func setOf(values ...string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

// typedMediaTypes is the ag2.types media vocabulary, in Python declaration
// order (images, audio, video, documents). KindOf and InputMediaTypes both
// read it, so their answers cannot drift apart.
var typedMediaTypes = []struct {
	mediaType string
	kind      events.BinaryType
}{
	{"image/jpeg", events.BinaryImage},
	{"image/png", events.BinaryImage},
	{"image/gif", events.BinaryImage},
	{"image/webp", events.BinaryImage},
	{"image/heic", events.BinaryImage},
	{"image/heif", events.BinaryImage},
	{"audio/wav", events.BinaryAudio},
	{"audio/mpeg", events.BinaryAudio},
	{"audio/ogg", events.BinaryAudio},
	{"audio/flac", events.BinaryAudio},
	{"audio/aiff", events.BinaryAudio},
	{"audio/aac", events.BinaryAudio},
	{"video/x-matroska", events.BinaryVideo},
	{"video/quicktime", events.BinaryVideo},
	{"video/mp4", events.BinaryVideo},
	{"video/webm", events.BinaryVideo},
	{"video/x-flv", events.BinaryVideo},
	{"video/mpeg", events.BinaryVideo},
	{"video/x-ms-wmv", events.BinaryVideo},
	{"video/3gpp", events.BinaryVideo},
	{"application/pdf", events.BinaryDocument},
	{"text/plain", events.BinaryDocument},
	{"text/csv", events.BinaryDocument},
	{"text/tsv", events.BinaryDocument},
	{"text/html", events.BinaryDocument},
	{"text/markdown", events.BinaryDocument},
	{"text/xml", events.BinaryDocument},
	{"text/css", events.BinaryDocument},
	{"text/javascript", events.BinaryDocument},
	{"text/calendar", events.BinaryDocument},
	{"text/vcard", events.BinaryDocument},
	{"text/srt", events.BinaryDocument},
	{"text/vtt", events.BinaryDocument},
	{"application/json", events.BinaryDocument},
	{"application/sql", events.BinaryDocument},
	{"message/rfc822", events.BinaryDocument},
	{"application/msword", events.BinaryDocument},
	{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", events.BinaryDocument},
	{"application/rtf", events.BinaryDocument},
	{"application/vnd.oasis.opendocument.text", events.BinaryDocument},
	{"application/vnd.ms-excel", events.BinaryDocument},
	{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", events.BinaryDocument},
	{"application/vnd.ms-powerpoint", events.BinaryDocument},
	{"application/vnd.openxmlformats-officedocument.presentationml.presentation", events.BinaryDocument},
}

// DescribedDialect returns the dialect the table describes cfg by. The second
// result is false when the table does not describe the config (nil, the
// Decisions config, a vendor the table does not know): no claim can be made
// about its mapper.
func DescribedDialect(cfg ModelConfig) (InputDialect, bool) {
	if cfg == nil {
		return "", false
	}
	if d, ok := cfg.(InputDialectConfig); ok {
		dialect := d.InputDialect()
		if _, known := acceptanceTable[dialect]; !known {
			return "", false
		}
		return dialect, true
	}
	if cfg.Provider() == ProviderOpenAI {
		// The three OpenAI configs share ProviderOpenAI; only the two that
		// implement InputDialectConfig are described.
		return "", false
	}
	dialect := InputDialect(cfg.Provider())
	if _, ok := acceptanceTable[dialect]; !ok {
		return "", false
	}
	return dialect, true
}

// AcceptsInput reports whether cfg's mapper accepts this part in this
// position. A config the table does not describe accepts everything, text and
// data parts are accepted everywhere, and inline binary data is additionally
// held to the dialect's media-type allowlist.
func AcceptsInput(cfg ModelConfig, position InputPosition, part events.Input) bool {
	dialect, described := DescribedDialect(cfg)
	if !described {
		return true
	}
	var (
		source InputSource
		kind   events.BinaryType
		binary *events.BinaryInput
	)
	switch p := events.AsValue(part).(type) {
	case events.BinaryInput:
		source, kind, binary = SourceData, p.Kind, &p
	case events.UrlInput:
		source, kind = SourceURL, p.Kind
	case events.FileIdInput:
		source = SourceFile // a file id carries no kind: the provider resolves it
	default:
		return true
	}
	if !acceptanceTable[dialect][mediaKey{position: position, source: source, kind: kind}] {
		return false
	}
	if binary != nil {
		if allowed, ok := mediaTypeFilters[dialect][binary.Kind]; ok && !allowed[binary.MediaType] {
			return false
		}
	}
	return true
}

// InputModalities reports which media kinds cfg's mapper accepts in user
// messages, keyed by the AG-UI capability vocabulary ("image", "audio",
// "video", "pdf"). A config the table does not describe declares nothing and
// gets an empty map: an undeclared modality says nothing, where false would
// promise what cannot be checked.
func InputModalities(cfg ModelConfig) map[string]bool {
	dialect, described := DescribedDialect(cfg)
	if !described {
		return map[string]bool{}
	}
	accepted := acceptanceTable[dialect]
	out := make(map[string]bool, 4)
	for _, m := range []struct {
		name string
		kind events.BinaryType
	}{
		{"image", events.BinaryImage},
		{"audio", events.BinaryAudio},
		{"video", events.BinaryVideo},
		{"pdf", events.BinaryDocument},
	} {
		out[m.name] = false
		for key := range accepted {
			if key.position == PositionUser && key.kind == m.kind {
				out[m.name] = true
				break
			}
		}
	}
	return out
}

// InputMediaTypes lists the media types cfg's mapper accepts as inline user
// data, in ag2.types order. It asks AcceptsInput about each typed media type,
// so it follows the same per-dialect rules as the mapper. A config the table
// does not describe accepts every typed media type.
func InputMediaTypes(cfg ModelConfig) []string {
	out := make([]string, 0, len(typedMediaTypes))
	for _, mt := range typedMediaTypes {
		part := events.BinaryInput{Data: nil, MediaType: mt.mediaType, Kind: mt.kind}
		if AcceptsInput(cfg, PositionUser, part) {
			out = append(out, mt.mediaType)
		}
	}
	return out
}

// BinaryKindOf returns the binary kind of a typed media type (image/png →
// BinaryImage). The second result is false for a media type ag2.types does not
// list, mirroring Python's None.
func BinaryKindOf(mediaType string) (events.BinaryType, bool) {
	for _, mt := range typedMediaTypes {
		if mt.mediaType == mediaType {
			return mt.kind, true
		}
	}
	return "", false
}
