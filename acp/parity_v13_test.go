package acp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luckystrker/ago/agent"
	"github.com/luckystrker/ago/events"
	"github.com/luckystrker/ago/stream"
)

// Parity with agent-client-protocol 1.0 as AG2 v1.3.2 uses it: message
// grouping by message_id, absolute session/new paths, uri/resource content
// blocks. The fake agent below is a raw wire peer — it sends exactly the
// session/update JSON the behavior names, ids and block shapes included.

const roleParity = "acp-parity-agent"

// parityUpdate is one agent_message_chunk of a fake turn: its message id and
// the content block it carries.
type parityUpdate struct {
	id    string
	block map[string]any
}

func ptext(id, text string) parityUpdate {
	return parityUpdate{id: id, block: map[string]any{"type": "text", "text": text}}
}

// parityBehaviors is what each behavior streams on session/prompt.
var parityBehaviors = map[string][]parityUpdate{
	// A preamble message, then the answer in two chunks of its own message.
	"messages": {ptext("m1", "Let me check."), ptext("m2", "Done: "), ptext("m2", "it works.")},
	// An agent that names no message: all chunks are one message.
	"no-ids": {ptext("", "a"), ptext("", "b"), ptext("", "c")},
	// An empty chunk of another message neither opens nor closes anything.
	"empty-chunk": {ptext("m1", "A"), ptext("m2", ""), ptext("m1", "B")},
	"uri-image": {
		ptext("", "see the chart"),
		{block: map[string]any{"type": "image", "uri": "https://example.com/chart.png", "mimeType": "image/png"}},
	},
	"resource": {
		ptext("", "see the file"),
		{block: map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///notes.txt", "text": "notes body"}}},
		{block: map[string]any{"type": "resource", "resource": map[string]any{
			"uri": "file:///a.pdf", "mimeType": "application/pdf", "blob": base64.StdEncoding.EncodeToString([]byte{1, 2, 3})}}},
		{block: map[string]any{"type": "resource", "resource": map[string]any{
			"uri": "file:///b.bin", "blob": base64.StdEncoding.EncodeToString([]byte{9})}}},
		{block: map[string]any{"type": "resource_link", "name": "Spec", "title": "The Spec", "uri": "https://x.test/spec"}},
		{block: map[string]any{"type": "resource_link", "title": "Only Title", "uri": "https://x.test/t"}},
		{block: map[string]any{"type": "resource_link", "uri": "https://x.test/u"}},
	},
	// An image with bytes is a file, an audio block without bytes is nothing.
	"media": {
		ptext("", "x"),
		{block: map[string]any{"type": "image", "uri": "https://x.test/i.png", "data": base64.StdEncoding.EncodeToString([]byte{7}), "mimeType": "image/png"}},
		{block: map[string]any{"type": "audio", "data": "", "mimeType": "audio/wav"}},
	},
}

// runParityAgent records session/new params into the file outFileEnv names
// and answers session/prompt with the updates of the behavior behaviorEnv
// selects.
func runParityAgent(in io.Reader, out io.Writer) {
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), scanBuffer)
	for sc.Scan() {
		var req childRequest
		if err := json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &req); err != nil || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			childReply(w, *req.ID, map[string]any{
				"protocolVersion": protocolVersion, "agentCapabilities": map[string]any{},
				"agentInfo": map[string]any{"name": "parity-agent", "version": "1"},
			})
		case "session/new":
			if f := os.Getenv(outFileEnv); f != "" {
				_ = os.WriteFile(f, req.Params, 0o644)
			}
			childReply(w, *req.ID, map[string]any{"sessionId": "sess-p"})
		case "session/prompt":
			for _, u := range parityBehaviors[os.Getenv(behaviorEnv)] {
				update := map[string]any{"sessionUpdate": "agent_message_chunk", "content": u.block}
				if u.id != "" {
					update["messageId"] = u.id
				}
				childNotify(w, "session/update", map[string]any{"sessionId": "sess-p", "update": update})
			}
			childReply(w, *req.ID, map[string]any{"stopReason": "end_turn"})
		default:
			childError(w, *req.ID, codeMethodNotFound, "method not implemented")
		}
	}
}

func parityConfig(t *testing.T, behavior string) *Config {
	t.Helper()
	cfg, _ := providerConfig(t, roleParity, behavior)
	cfg.Env[outFileEnv] = filepath.Join(t.TempDir(), "session-new.json")
	return cfg
}

// parityRun runs one agent turn against the parity fake and returns the reply
// with every event the run's stream carried.
func parityRun(t *testing.T, cfg *Config) (*agent.AgentReply, []events.Event) {
	t.Helper()
	a := agent.New("acp-agent", "", agent.WithConfig(cfg))
	run, err := a.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := &capture{}
	run.Stream().Subscribe(seen.add, stream.SubscribeOptions{})
	if err := run.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reply, err := run.Result(ctx)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	run.Close()
	return reply, seen.all()
}

// --- ACP-2: messages are grouped by message_id --------------------------------

// Python: test_messages_the_agent_names_apart_are_not_run_together — the reply
// is the last message, the earlier one is closed with its own ModelMessage.
func TestProviderMessagesNamedApartAreNotRunTogether(t *testing.T) {
	reply, evs := parityRun(t, parityConfig(t, "messages"))
	if got := reply.Body(); got != "Done: it works." {
		t.Fatalf("reply = %q, want the last message %q", got, "Done: it works.")
	}
	var closed []string
	for _, ev := range evs {
		if m, ok := events.AsValue(ev).(events.ModelMessage); ok {
			closed = append(closed, m.Content)
		}
	}
	if !reflect.DeepEqual(closed, []string{"Let me check."}) {
		t.Fatalf("ModelMessage events = %q, want only the closed first message", closed)
	}
}

// Python: test_each_message_is_closed_before_the_next_one_streams — consumers
// read a ModelMessage as the end of the chunks before it.
func TestProviderMessageIsClosedBeforeTheNextStreams(t *testing.T) {
	_, evs := parityRun(t, parityConfig(t, "messages"))
	var order []string
	for _, ev := range evs {
		switch e := events.AsValue(ev).(type) {
		case events.ModelMessageChunk:
			order = append(order, "chunk:"+e.Content)
		case events.ModelMessage:
			order = append(order, "message:"+e.Content)
		}
	}
	want := []string{"chunk:Let me check.", "message:Let me check.", "chunk:Done: ", "chunk:it works."}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("event order = %q, want %q", order, want)
	}
}

// Python: test_chunks_of_one_message_make_one_message.
func TestProviderChunksOfOneMessageMakeOneMessage(t *testing.T) {
	for _, behavior := range []string{"no-ids", "empty-chunk"} {
		t.Run(behavior, func(t *testing.T) {
			reply, evs := parityRun(t, parityConfig(t, behavior))
			want := map[string]string{"no-ids": "abc", "empty-chunk": "AB"}[behavior]
			if got := reply.Body(); got != want {
				t.Errorf("reply = %q, want %q", got, want)
			}
			for _, ev := range evs {
				if m, ok := events.AsValue(ev).(events.ModelMessage); ok {
					t.Errorf("unexpected closed message %q: the chunks are one message", m.Content)
				}
			}
		})
	}
}

// TurnResult.Text is the last message (Python: reply = ModelMessage(texts[-1]));
// Messages carries all of them.
func TestBridgeTurnResultTextIsTheLastMessage(t *testing.T) {
	t.Setenv(behaviorEnv, "messages")
	b := newRoleBridge(t, roleParity, nil)
	id, err := b.SessionNew(t.TempDir())
	if err != nil {
		t.Fatalf("SessionNew: %v", err)
	}
	tr, err := b.PromptTurn(id, "go")
	if err != nil {
		t.Fatalf("PromptTurn: %v", err)
	}
	if tr.Text != "Done: it works." {
		t.Errorf("Text = %q, want the last message", tr.Text)
	}
	if want := []string{"Let me check.", "Done: it works."}; !reflect.DeepEqual(tr.Messages, want) {
		t.Errorf("Messages = %q, want %q", tr.Messages, want)
	}
	if text, err := b.Prompt(id, "again"); err != nil || text != "Done: it works." {
		t.Errorf("Prompt = %q, %v; want the last message", text, err)
	}
}

// --- ACP-1: session/new carries absolute directories --------------------------

func readSessionNew(t *testing.T, cfg *Config) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(cfg.Env[outFileEnv])
	if err != nil {
		t.Fatalf("the agent never saw session/new: %v", err)
	}
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("session/new params %s: %v", raw, err)
	}
	return params
}

func TestProviderSessionNewDirectoriesAreAbsolute(t *testing.T) {
	cfg := parityConfig(t, "no-ids")
	cfg.CWD = "."
	cfg.AdditionalDirectories = []string{"extra/one", "./extra/two", "/already/abs", "../up"}
	parityRun(t, cfg)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	params := readSessionNew(t, cfg)
	if params["cwd"] != wd {
		t.Errorf("cwd = %v, want %q", params["cwd"], wd)
	}
	want := []any{
		filepath.Join(wd, "extra/one"), filepath.Join(wd, "extra/two"), "/already/abs", filepath.Dir(wd) + "/up",
	}
	if got, _ := params["additionalDirectories"].([]any); !reflect.DeepEqual(got, want) {
		t.Errorf("additionalDirectories = %v, want %v", got, want)
	}
}

// goneCWDEnv marks the re-exec'd test binary that runs the unresolvable-path
// scenario; its value names the scenario.
const goneCWDEnv = "AGO_ACP_TEST_GONE_CWD"

// A path that cannot be made absolute is an error naming it, not a relative
// path on the wire. filepath.Abs fails only when the process's own working
// directory is gone, so each scenario runs in a child process (this test
// binary re-exec'd) that removes its own working directory; the test process
// keeps its own.
func TestProviderRefusesPathsThatCannotBeResolved(t *testing.T) {
	if scenario := os.Getenv(goneCWDEnv); scenario != "" {
		runGoneCWDScenario(t, scenario)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	for _, scenario := range []string{"cwd", "additional directories"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestProviderRefusesPathsThatCannotBeResolved$", "-test.count=1")
			cmd.Env = append(os.Environ(), goneCWDEnv+"="+scenario)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child process failed: %v\n%s", err, out)
			}
		})
	}
}

// runGoneCWDScenario runs inside the child: it deletes the process's working
// directory and asks an agent to start with a relative path.
func runGoneCWDScenario(t *testing.T, scenario string) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg := parityConfig(t, "no-ids")
	switch scenario {
	case "cwd":
		cfg.CWD = "rel"
	case "additional directories":
		cfg.CWD = orig
		cfg.AdditionalDirectories = []string{"extra"}
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}

	gone, err := os.MkdirTemp("", "gone-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(gone); err != nil {
		t.Fatal(err)
	}
	os.Setenv("PWD", "") // os.Getwd would otherwise trust $PWD
	if err := os.Remove(gone); err != nil {
		t.Fatalf("cannot remove the working directory: %v", err)
	}

	a := agent.New("acp-agent", "", agent.WithConfig(cfg))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = a.Ask(ctx, "go")
	if err == nil {
		t.Fatal("Ask succeeded; want an error naming the unresolvable path")
	}
	if !errors.Is(err, ErrWorkspacePath) {
		t.Errorf("err = %v, want it to wrap ErrWorkspacePath", err)
	}
	// The cause filepath.Abs reported stays reachable: the working directory is
	// gone, which is fs.ErrNotExist whatever the platform's error type is.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap the underlying fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), map[string]string{"cwd": `"rel"`, "additional directories": `"extra"`}[scenario]) {
		t.Errorf("err = %v, want it to name the path", err)
	}
	if _, statErr := os.Stat(cfg.Env[outFileEnv]); statErr == nil {
		t.Error("the agent was started and saw a session/new anyway")
	}
}

// --- ACP-3: uri-only image, resource, resource_link -----------------------------

// Python: test_an_image_that_only_points_at_a_uri_is_its_link_not_a_file.
func TestProviderUriOnlyImageReadsAsALink(t *testing.T) {
	reply, _ := parityRun(t, parityConfig(t, "uri-image"))
	if got, want := reply.Body(), "see the chart[image] https://example.com/chart.png"; got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
	if n := len(reply.Files()); n != 0 {
		t.Errorf("files = %d, want none: a uri is a link, not bytes", n)
	}
}

func TestProviderResourceBlocks(t *testing.T) {
	reply, evs := parityRun(t, parityConfig(t, "resource"))
	want := "see the file" + "notes body" +
		"[resource] Spec (https://x.test/spec)" + "[resource] Only Title (https://x.test/t)" + "[resource] https://x.test/u (https://x.test/u)"
	if got := reply.Body(); got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
	files := reply.Files()
	if len(files) != 2 {
		t.Fatalf("files = %+v, want the two blob resources", files)
	}
	if string(files[0].Data) != "\x01\x02\x03" || files[0].Metadata["mimeType"] != "application/pdf" {
		t.Errorf("file 0 = %+v", files[0])
	}
	if string(files[1].Data) != "\x09" || files[1].Metadata["mimeType"] != "application/octet-stream" {
		t.Errorf("file 1 = %+v, want the default MIME type", files[1])
	}
	var streamed string
	for _, ev := range evs {
		if c, ok := events.AsValue(ev).(events.ModelMessageChunk); ok {
			streamed += c.Content
		}
	}
	if streamed != want {
		t.Errorf("streamed text = %q, want the same %q", streamed, want)
	}
}

// Python: test_an_audio_block_with_no_data_is_not_a_file; an image with bytes
// is a file and adds no text.
func TestProviderMediaBlocks(t *testing.T) {
	reply, _ := parityRun(t, parityConfig(t, "media"))
	if reply.Body() != "x" {
		t.Errorf("reply = %q, want %q", reply.Body(), "x")
	}
	files := reply.Files()
	if len(files) != 1 || string(files[0].Data) != "\x07" {
		t.Errorf("files = %+v, want only the image with bytes", files)
	}
}

// Serve side: only resource_link is read (as a named link); the declared
// prompt capabilities stay false.
func TestServeReadsResourceLinkAsText(t *testing.T) {
	var seen []string
	cfg := ServeConfig{
		Handle: func(ctx context.Context, sid, text string, emit *Emitter) (string, error) {
			seen = append(seen, text)
			return "", nil
		},
	}
	fc := startServe(t, func(in io.Reader, out io.Writer) error { return serveIO(in, out, cfg) })
	if _, rpcErr := fc.call("initialize", map[string]any{"protocolVersion": protocolVersion}); rpcErr != nil {
		t.Fatalf("initialize: %v", rpcErr)
	}
	res, rpcErr := fc.call("session/new", map[string]any{"cwd": t.TempDir()})
	if rpcErr != nil {
		t.Fatalf("session/new: %v", rpcErr)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		prompt []map[string]any
		want   string
	}{
		{"link after text", []map[string]any{
			{"type": "text", "text": "read"},
			{"type": "resource_link", "name": "Spec", "uri": "https://x.test/spec"},
		}, "read\n[resource] Spec (https://x.test/spec)"},
		{"label falls back to title then uri", []map[string]any{
			{"type": "resource_link", "title": "T", "uri": "https://x.test/t"},
			{"type": "resource_link", "uri": "https://x.test/u"},
		}, "[resource] T (https://x.test/t)\n[resource] https://x.test/u (https://x.test/u)"},
		{"text blocks still join as before", []map[string]any{
			{"type": "text", "text": "a"}, {"type": "text", "text": "b"},
			{"type": "image", "uri": "https://x.test/i.png"},
			{"type": "resource", "resource": map[string]any{"uri": "file:///f", "text": "body"}},
		}, "ab"},
	} {
		seen = nil
		if _, rpcErr := fc.call("session/prompt", map[string]any{"sessionId": created.SessionID, "prompt": tc.prompt}); rpcErr != nil {
			t.Fatalf("%s: session/prompt: %v", tc.name, rpcErr)
		}
		if len(seen) != 1 || seen[0] != tc.want {
			t.Errorf("%s: handler saw %q, want %q", tc.name, seen, tc.want)
		}
	}
}

// --- ACP-4: env_var is not an auth method of ACP 1.0 ----------------------------

// agent-client-protocol 1.0 dropped EnvVarAuthMethod from the AuthMethod union
// (AG2 v1.3.2: AuthMethod = TerminalAuthMethod | AuthMethodAgent). Nothing here
// may declare an "env_var" method: not a kind, not the field that carried its
// variables.
func TestAuthMethodHasNoEnvVarKind(t *testing.T) {
	typ := reflect.TypeOf(AuthMethod{})
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "vars" || tag == "env_var" {
			t.Errorf("AuthMethod.%s (json %q) still models the removed env_var method", typ.Field(i).Name, tag)
		}
	}
	for _, m := range []AuthMethod{
		NewAgentAuthMethod("a", "A", ""),
		NewTerminalAuthMethod("t", "T", "", "login"),
	} {
		if k := m.Kind(); k != AuthKindAgent && k != AuthKindTerminal {
			t.Errorf("%s: kind %q is not one of the 1.0 kinds", m.ID, k)
		}
	}
}
