package a2a

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalPartStampsKind(t *testing.T) {
	// Constructed without any kind field by the caller: the discriminator is
	// structural and must land on the wire anyway.
	tests := []struct {
		name string
		part Part
		want string
	}{
		{"text", &TextPart{Text: "hello"}, `{"kind":"text","text":"hello"}`},
		{"data", &DataPart{Data: map[string]any{"k": "v"}}, `{"kind":"data","data":{"k":"v"}}`},
		{"file", &FilePart{File: FileBlock{Name: "f.txt", URI: "file:///f.txt"}}, `{"kind":"file","file":{"name":"f.txt","uri":"file:///f.txt"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.part)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if got := string(data); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
			if kind := tt.part.PartKind(); kind != tt.name {
				t.Fatalf("PartKind = %q, want %q", kind, tt.name)
			}
		})
	}
}

func TestMessagePartsRoundTrip(t *testing.T) {
	msg := &Message{
		Role:      RoleUser,
		Parts:     []Part{NewTextPart("hi"), NewDataPart(map[string]any{"n": 1.0}), NewFilePart(FileBlock{URI: "file:///x"})},
		MessageID: "m-1",
		ContextID: "c-1",
		TaskID:    "t-1",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Role != RoleUser || back.MessageID != "m-1" || back.ContextID != "c-1" || back.TaskID != "t-1" {
		t.Fatalf("scalar fields drifted: %+v", back)
	}
	if len(back.Parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(back.Parts))
	}
	text, ok := back.Parts[0].(*TextPart)
	if !ok || text.Text != "hi" {
		t.Fatalf("part 0 = %#v, want *TextPart{hi}", back.Parts[0])
	}
	datum, ok := back.Parts[1].(*DataPart)
	if !ok {
		t.Fatalf("part 1 = %T, want *DataPart", back.Parts[1])
	}
	if m, ok := datum.Data.(map[string]any); !ok || m["n"] != 1.0 {
		t.Fatalf("data payload = %#v", datum.Data)
	}
	file, ok := back.Parts[2].(*FilePart)
	if !ok || file.File.URI != "file:///x" {
		t.Fatalf("part 2 = %#v, want *FilePart with uri", back.Parts[2])
	}
}

func TestUnmarshalUnknownPartKind(t *testing.T) {
	var msg Message
	err := json.Unmarshal([]byte(`{"role":"user","parts":[{"kind":"weird","x":1}]}`), &msg)
	if err == nil {
		t.Fatal("want error for unknown part kind")
	}
	if !strings.Contains(err.Error(), "weird") {
		t.Fatalf("error should name the kind, got: %v", err)
	}
}

func TestTaskRoundTrip(t *testing.T) {
	task := &Task{
		ID:        "t-1",
		ContextID: "c-1",
		Status:    TaskStatus{State: TaskStateCompleted, Message: NewMessage(RoleAgent, NewTextPart("done"))},
		History:   []Message{*NewMessage(RoleUser, NewTextPart("go"))},
		Artifacts: []Artifact{{ArtifactID: "a-1", Name: "out", Parts: []Part{NewTextPart("payload")}}},
	}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Task
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Status.State != TaskStateCompleted || back.Status.Message == nil || len(back.Status.Message.Parts) != 1 {
		t.Fatalf("status drifted: %+v", back.Status)
	}
	if tp, ok := back.Status.Message.Parts[0].(*TextPart); !ok || tp.Text != "done" {
		t.Fatalf("status message part = %#v", back.Status.Message.Parts[0])
	}
	if len(back.History) != 1 || len(back.Artifacts) != 1 || len(back.Artifacts[0].Parts) != 1 {
		t.Fatalf("history/artifacts drifted: %+v", back)
	}
	if tp, ok := back.Artifacts[0].Parts[0].(*TextPart); !ok || tp.Text != "payload" {
		t.Fatalf("artifact part = %#v", back.Artifacts[0].Parts[0])
	}
}

func TestAgentCardWireShape(t *testing.T) {
	card := AgentCard{
		Name:            "helper",
		URL:             "http://localhost:8000",
		Version:         "1.0.0",
		ProtocolVersion: ProtocolVersionCurrent,
		SupportedInterfaces: []AgentInterface{
			{URL: "http://localhost:8000", ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: ProtocolVersionCurrent},
			{URL: "http://rest.example/t", ProtocolBinding: "HTTP+JSON", ProtocolVersion: ProtocolVersionCurrent, Tenant: "acme"},
		},
		Capabilities:       AgentCapabilities{Streaming: true, PushNotifications: false},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []AgentSkill{{ID: "s1", Name: "answer", Tags: []string{"qa"}}},
	}
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"name"`, `"url"`, `"protocolVersion"`, `"supportedInterfaces"`, `"protocolBinding"`, `"tenant"`,
		`"capabilities"`, `"defaultInputModes"`, `"defaultOutputModes"`, `"skills"`, `"streaming"`, `"pushNotifications"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("card JSON missing %s: %s", key, data)
		}
	}
	var back AgentCard
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Name != "helper" || !back.Capabilities.Streaming || len(back.Skills) != 1 {
		t.Fatalf("card drifted: %+v", back)
	}
	if back.ProtocolVersion != ProtocolVersionCurrent || len(back.SupportedInterfaces) != 2 {
		t.Fatalf("discovery fields drifted: %+v", back)
	}
	if iface := back.SupportedInterfaces[1]; iface.ProtocolBinding != "HTTP+JSON" || iface.Tenant != "acme" ||
		iface.ProtocolVersion != ProtocolVersionCurrent || iface.URL != "http://rest.example/t" {
		t.Fatalf("interface drifted: %+v", iface)
	}
}

// TestAgentCardDiscoveryKeysOnWire: the field names a 1.x client resolves the
// card through are camelCase and never omitted when set — the exact spelling
// the a2a-sdk proto JSON uses (supportedInterfaces[].protocolBinding).
func TestAgentCardDiscoveryKeysOnWire(t *testing.T) {
	data, err := json.Marshal(AgentCard{
		Name:            "helper",
		ProtocolVersion: ProtocolVersionCurrent,
		SupportedInterfaces: []AgentInterface{
			{URL: "http://x", ProtocolBinding: ProtocolBindingJSONRPC, ProtocolVersion: ProtocolVersionCurrent},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// snake_case spellings must not appear: the tags follow the A2A wire
	// spec, not the AG2 event convention.
	for _, bad := range []string{"protocol_version", "supported_interfaces", "protocol_binding"} {
		if strings.Contains(string(data), bad) {
			t.Fatalf("card JSON carries snake_case %s: %s", bad, data)
		}
	}
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"protocolVersion", "supportedInterfaces"} {
		if _, ok := probe[key]; !ok {
			t.Fatalf("card JSON missing top-level %q: %s", key, data)
		}
	}
}

func TestTerminalTaskState(t *testing.T) {
	for _, state := range []TaskState{TaskStateCompleted, TaskStateFailed, TaskStateCanceled} {
		if !terminalTaskState(state) {
			t.Errorf("%q should be terminal", state)
		}
	}
	for _, state := range []TaskState{TaskStateSubmitted, TaskStateWorking} {
		if terminalTaskState(state) {
			t.Errorf("%q should not be terminal", state)
		}
	}
}
