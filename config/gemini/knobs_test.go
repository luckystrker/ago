package gemini

// Tests for the request knobs added for D-035/D-022: cached content, the
// image-generation circuit (response_modalities + image_config, generated
// images into ModelResponse.Files), the raw thinking_config object and the
// per-input Gemini vendor_metadata.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/luckystrker/ago/config"
	"github.com/luckystrker/ago/events"
)

func TestGeminiCachedContentOnWire(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL, WithCachedContent("cachedContents/abc")))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	body := cap.JSON(t)
	if body["cachedContent"] != "cachedContents/abc" {
		t.Fatalf("cachedContent: %v", body["cachedContent"])
	}

	// Unset: the field stays off the wire.
	srv2, cap2 := captureServer(t, 200, `{"candidates":[]}`)
	cl = startClient(t, newTestConfig(srv2.URL))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := cap2.JSON(t)["cachedContent"]; ok {
		t.Fatal("an unset cached content must not be sent")
	}
}

// TestGeminiImageGenerationRequestAndFiles: the request asks for images and
// carries the image config; the scripted answer's inline images land in Files.
func TestGeminiImageGenerationRequestAndFiles(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[
		{"text":"here"},
		{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}},
		{"inlineData":{"mimeType":"image/jpeg","data":"/9j/4A=="}}
	]},"finishReason":"STOP"}]}`
	srv, cap := captureServer(t, 200, body)
	cl := startClient(t, newTestConfig(srv.URL,
		WithResponseModalities("TEXT", "IMAGE"),
		WithImageConfig(ImageConfig{AspectRatio: "16:9", ImageSize: "2K"}),
	))
	resp, err := cl.Call(context.Background(), nil, nil, config.CallOptions{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	gen := cap.JSON(t)["generationConfig"].(map[string]any)
	if want := []any{"TEXT", "IMAGE"}; !reflect.DeepEqual(gen["responseModalities"], want) {
		t.Fatalf("responseModalities = %#v, want %#v", gen["responseModalities"], want)
	}
	img, ok := gen["imageConfig"].(map[string]any)
	if !ok {
		t.Fatalf("imageConfig missing: %v", gen)
	}
	if img["aspectRatio"] != "16:9" || img["imageSize"] != "2K" {
		t.Fatalf("imageConfig = %#v", img)
	}
	if _, ok := img["personGeneration"]; ok {
		t.Fatalf("unset image fields must stay off the wire: %#v", img)
	}

	// Generated images become BinaryResults with the media type metadata the
	// reply.files consumers read (Python parity: _inline_data_to_binary).
	if len(resp.Files) != 2 {
		t.Fatalf("want 2 files, got %d", len(resp.Files))
	}
	if string(resp.Files[0].Data) != "\x89PNG\r\n\x1a\n" || resp.Files[0].Metadata["media_type"] != "image/png" {
		t.Fatalf("first file: %+v", resp.Files[0])
	}
	if resp.Files[1].Metadata["media_type"] != "image/jpeg" {
		t.Fatalf("second file: %+v", resp.Files[1])
	}
	if resp.Message == nil || resp.Message.Content != "here" {
		t.Fatalf("text part: %+v", resp.Message)
	}
}

// TestGeminiImageConfigAllFields pins the full ImageConfig mapping, including
// the Vertex-only fields, so no Python field is silently dropped.
func TestGeminiImageConfigAllFields(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	q := int64(80)
	cl := startClient(t, newTestConfig(srv.URL, WithImageConfig(ImageConfig{
		AspectRatio:              "1:1",
		ImageSize:                "4K",
		PersonGeneration:         "ALLOW_ADULT",
		ProminentPeople:          "ALLOW",
		OutputMimeType:           "image/webp",
		OutputCompressionQuality: &q,
		ImageOutputOptions:       map[string]any{"format": "webp"},
	})))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	img := cap.JSON(t)["generationConfig"].(map[string]any)["imageConfig"].(map[string]any)
	want := map[string]any{
		"aspectRatio":              "1:1",
		"imageSize":                "4K",
		"personGeneration":         "ALLOW_ADULT",
		"prominentPeople":          "ALLOW",
		"outputMimeType":           "image/webp",
		"outputCompressionQuality": float64(80),
		"imageOutputOptions":       map[string]any{"format": "webp"},
	}
	if !reflect.DeepEqual(img, want) {
		t.Fatalf("imageConfig = %#v, want %#v", img, want)
	}

	// An all-zero block is not a setting.
	srv2, cap2 := captureServer(t, 200, `{"candidates":[]}`)
	cl = startClient(t, newTestConfig(srv2.URL, WithImageConfig(ImageConfig{})))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, ok := cap2.JSON(t)["generationConfig"]; ok {
		t.Fatal("an empty imageConfig must not create a generationConfig block")
	}
}

// TestGeminiRawThinkingConfigWins: the raw object replaces the level/budget
// resolution entirely (Python parity: _resolve_thinking_config).
func TestGeminiRawThinkingConfigWins(t *testing.T) {
	raw := map[string]any{"thinkingLevel": "HIGH", "includeThoughts": true}
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL,
		WithThinkingLevel("low"), WithThinkingBudget(64), WithThinkingConfig(raw),
	))
	if _, err := cl.Call(context.Background(), nil, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	got := cap.JSON(t)["generationConfig"].(map[string]any)["thinkingConfig"]
	if !reflect.DeepEqual(got, map[string]any{"thinkingLevel": "HIGH", "includeThoughts": true}) {
		t.Fatalf("raw thinkingConfig must win untouched: %#v", got)
	}

	// Without the raw object the level/budget pair still resolves.
	if gen := generationConfigOf(t, WithThinkingLevel("low"), WithThinkingBudget(64)); !reflect.DeepEqual(
		gen["thinkingConfig"], map[string]any{"thinkingLevel": "low", "thinkingBudget": int64(64)},
	) {
		t.Fatalf("level/budget resolution regressed: %#v", gen["thinkingConfig"])
	}
}

// TestGeminiBinaryInputVendorMetadata: media_resolution, video_metadata and
// display_name reach the part in the REST shape (Python parity:
// _apply_vendor_metadata).
func TestGeminiBinaryInputVendorMetadata(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL))
	in := &events.BinaryInput{
		Data:      []byte{1, 2, 3},
		MediaType: "video/mp4",
		Kind:      events.BinaryVideo,
		VendorMetadata: map[string]any{
			"media_resolution": "MEDIA_RESOLUTION_HIGH",
			"video_metadata":   map[string]any{"start_offset": "1s", "end_offset": "2s", "fps": 2.0},
			"display_name":     "clip.mp4",
		},
	}
	req := &events.ModelRequest{Parts: []events.Input{in}}
	if _, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	parts := cap.JSON(t)["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	part := parts[0].(map[string]any)
	if !reflect.DeepEqual(part["mediaResolution"], map[string]any{"level": "MEDIA_RESOLUTION_HIGH"}) {
		t.Fatalf("mediaResolution: %#v", part["mediaResolution"])
	}
	if !reflect.DeepEqual(part["videoMetadata"], map[string]any{
		"startOffset": "1s", "endOffset": "2s", "fps": float64(2),
	}) {
		t.Fatalf("videoMetadata: %#v", part["videoMetadata"])
	}
	inline := part["inlineData"].(map[string]any)
	if inline["displayName"] != "clip.mp4" || inline["mimeType"] != "video/mp4" {
		t.Fatalf("inlineData: %#v", inline)
	}

	// An object-form media_resolution carries level and numTokens.
	srv2, cap2 := captureServer(t, 200, `{"candidates":[]}`)
	cl = startClient(t, newTestConfig(srv2.URL))
	in2 := &events.BinaryInput{
		Data: []byte{1}, MediaType: "image/png", Kind: events.BinaryImage,
		VendorMetadata: map[string]any{"media_resolution": map[string]any{"level": "MEDIA_RESOLUTION_LOW", "num_tokens": 64}},
	}
	req2 := &events.ModelRequest{Parts: []events.Input{in2}}
	if _, err := cl.Call(context.Background(), []events.Event{req2}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	part2 := cap2.JSON(t)["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(part2["mediaResolution"], map[string]any{"level": "MEDIA_RESOLUTION_LOW", "numTokens": float64(64)}) {
		t.Fatalf("object mediaResolution: %#v", part2["mediaResolution"])
	}
}

// TestGeminiVendorMetadataRejectsUnknown: values the typed Python Part cannot
// hold fail the call instead of reaching the wire verbatim; an unknown
// top-level metadata key is ignored, as Python's three-key lookup does.
func TestGeminiVendorMetadataRejectsUnknown(t *testing.T) {
	srv, cap := captureServer(t, 200, `{"candidates":[]}`)
	cl := startClient(t, newTestConfig(srv.URL))
	in := &events.BinaryInput{
		Data: []byte{1}, MediaType: "image/png", Kind: events.BinaryImage,
		VendorMetadata: map[string]any{"nope": 1},
	}
	req := &events.ModelRequest{Parts: []events.Input{in}}
	if _, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{}); err != nil {
		t.Fatalf("an unknown top-level metadata key must be ignored: %v", err)
	}
	part := cap.JSON(t)["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if _, ok := part["nope"]; ok {
		t.Fatalf("unknown metadata must not reach the wire: %#v", part)
	}

	cases := map[string]map[string]any{
		"bad resolution type":     {"media_resolution": 3},
		"unknown resolution key":  {"media_resolution": map[string]any{"quality": "high"}},
		"video metadata not map":  {"video_metadata": "1s"},
		"unknown video key":       {"video_metadata": map[string]any{"loop": true}},
		"display name not string": {"display_name": 7},
	}
	for name, md := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := captureServer(t, 200, `{"candidates":[]}`)
			cl := startClient(t, newTestConfig(srv.URL))
			in := &events.BinaryInput{Data: []byte{1}, MediaType: "image/png", Kind: events.BinaryImage, VendorMetadata: md}
			req := &events.ModelRequest{Parts: []events.Input{in}}
			_, err := cl.Call(context.Background(), []events.Event{req}, nil, config.CallOptions{})
			if !errors.Is(err, config.ErrUnsupportedInput) {
				t.Fatalf("want ErrUnsupportedInput, got %v", err)
			}
		})
	}
}

// TestGeminiCloneNewKnobs: the new fields survive Clone and clear on nil.
func TestGeminiCloneNewKnobs(t *testing.T) {
	base := NewGemini("k", "m",
		WithCachedContent("cachedContents/old"),
		WithResponseModalities("TEXT"),
		WithImageConfig(ImageConfig{AspectRatio: "1:1"}),
		WithThinkingConfig(map[string]any{"includeThoughts": true}),
	)
	cc, err := base.Clone(map[string]any{
		"cached_content":      "cachedContents/new",
		"response_modalities": []string{"IMAGE"},
		"image_config":        ImageConfig{AspectRatio: "9:16", ImageSize: "1K"},
		"thinking_config":     map[string]any{"thinkingBudget": 128},
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	got := cc.(*GeminiConfig)
	if got.cachedContent != "cachedContents/new" ||
		!reflect.DeepEqual(got.responseModalities, []string{"IMAGE"}) ||
		got.imageConfig == nil || got.imageConfig.AspectRatio != "9:16" || got.imageConfig.ImageSize != "1K" ||
		!reflect.DeepEqual(got.thinkingConfig, map[string]any{"thinkingBudget": 128}) {
		t.Fatalf("overrides not applied: %+v", got)
	}
	// The receiver is untouched (the image block and both maps are copies).
	if base.cachedContent != "cachedContents/old" || base.imageConfig.AspectRatio != "1:1" ||
		base.thinkingConfig["includeThoughts"] != true {
		t.Fatalf("base mutated: %+v", base)
	}
	got.imageConfig.AspectRatio = "2:3"
	if base.imageConfig.AspectRatio != "1:1" {
		t.Fatal("the cloned image block must be a copy")
	}

	cc2, err := base.Clone(map[string]any{
		"cached_content": "", "response_modalities": nil, "image_config": nil, "thinking_config": nil,
	})
	if err != nil {
		t.Fatalf("clone nil: %v", err)
	}
	got2 := cc2.(*GeminiConfig)
	if got2.cachedContent != "" || got2.responseModalities != nil || got2.imageConfig != nil || got2.thinkingConfig != nil {
		t.Fatalf("nil overrides must clear: %+v", got2)
	}
	for key, bad := range map[string]any{
		"cached_content":      3,
		"response_modalities": "IMAGE",
		"image_config":        "1:1",
		"thinking_config":     []any{"x"},
	} {
		if _, err := base.Clone(map[string]any{key: bad}); err == nil {
			t.Fatalf("type-mismatched override %q must fail", key)
		}
	}
}
