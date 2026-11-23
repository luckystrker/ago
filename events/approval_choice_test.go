package events

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestApprovalChoicesRoundTrip: a request offering choices and the answer that
// picks one survive Marshal/Unmarshal, keeping a pick apart from a word.
func TestApprovalChoicesRoundTrip(t *testing.T) {
	req := &ToolApprovalRequest{
		HumanInputRequest: HumanInputRequest{
			ID: "r1", Content: "git status",
			Choices: []ApprovalChoice{{Value: "once-1", Label: "Allow once"}, {Value: "y", Label: "Reject"}},
		},
		ToolCallID: "c1",
	}
	pick := NewApprovalChoiceMessage("r1", "y")
	word := &HumanMessage{ParentID: "r1", Content: "y"}

	for _, ev := range []Event{req, pick, word} {
		raw, err := MarshalEvent(ev)
		if err != nil {
			t.Fatalf("MarshalEvent(%T): %v", ev, err)
		}
		got, err := UnmarshalEvent(raw)
		if err != nil {
			t.Fatalf("UnmarshalEvent(%s): %v", raw, err)
		}
		if !reflect.DeepEqual(got, ev) {
			t.Errorf("round trip of %T = %+v, want %+v", ev, got, ev)
		}
	}

	raw, _ := json.Marshal(req)
	if !strings.Contains(string(raw), `"choices":[{"value":"once-1","label":"Allow once"}`) {
		t.Errorf("request JSON = %s, want Python's choices/value/label names", raw)
	}
	rawWord, _ := json.Marshal(word)
	if strings.Contains(string(rawWord), "choice") {
		t.Errorf("a word answer must not carry the pick marker: %s", rawWord)
	}
	rawPick, _ := json.Marshal(pick)
	if !strings.Contains(string(rawPick), `"choice":true`) {
		t.Errorf("a picked answer must say so on the wire: %s", rawPick)
	}
	rawPlain, _ := json.Marshal(&ToolApprovalRequest{HumanInputRequest: HumanInputRequest{ID: "r"}})
	if strings.Contains(string(rawPlain), "choices") {
		t.Errorf("a request without choices must not emit the key (additive): %s", rawPlain)
	}
}

// TestApprovalChoiceIsNotAWord: IsChoice tells the two apart even when the
// content is identical.
func TestApprovalChoiceIsNotAWord(t *testing.T) {
	if NewApprovalChoiceMessage("r", "y").IsChoice() != true {
		t.Error("a pick must report IsChoice")
	}
	if (&HumanMessage{Content: "y"}).IsChoice() {
		t.Error("a word must not report IsChoice")
	}
}
