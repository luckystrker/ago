package response

import (
	"errors"
	"reflect"
	"testing"
)

func TestFromResponseBoolPredicate(t *testing.T) {
	spec, err := FromResponse(NewSchema[bool]("B", "Is it a refund?"))
	if err != nil {
		t.Fatalf("FromResponse error: %v", err)
	}
	if spec.Kind != DecisionPredicate || !spec.Boolean || !spec.Embedded {
		t.Errorf("spec = %+v, want an embedded boolean predicate", spec)
	}
	if spec.Question != "Is it a refund?" {
		t.Errorf("Question = %q", spec.Question)
	}
	if spec.PredicateValue(0.4, 0.5) != false || spec.PredicateValue(0.6, 0.5) != true {
		t.Errorf("PredicateValue wrong: %+v", spec.PredicateValue(0.6, 0.5))
	}
	if v, err := spec.Render(true); err != nil || v != `{"data":true}` {
		t.Errorf("Render(true) = %q, %v", v, err)
	}
}

func TestFromResponseQuestionSchema(t *testing.T) {
	p := QuestionSchema(Question{Text: "Refund?"})
	spec, err := FromResponse(p)
	if err != nil {
		t.Fatalf("FromResponse error: %v", err)
	}
	if spec.Kind != DecisionPredicate || !spec.Boolean || !spec.Embedded || spec.Question != "Refund?" {
		t.Errorf("spec = %+v", spec)
	}
	v, err := p.Validate(`{"data":true}`, nil)
	if err != nil || v != true {
		t.Errorf("Validate = %v, %v", v, err)
	}
	if _, err := p.Validate(`{"data":"yes"}`, nil); err == nil {
		t.Error("Validate accepted a string answer for a boolean question")
	}
}

func TestFromResponseChoice(t *testing.T) {
	p := RawSchema(map[string]any{"type": "string", "enum": []any{"a", "b"}}, WithRawName("C"))
	spec, err := FromResponse(p)
	if err != nil {
		t.Fatalf("FromResponse error: %v", err)
	}
	if spec.Kind != DecisionChoice || spec.Boolean || spec.Embedded {
		t.Errorf("spec = %+v, want an unwrapped choice", spec)
	}
	want := []DecisionOption{{Value: "a", Name: "a"}, {Value: "b", Name: "b"}}
	if !reflect.DeepEqual(spec.Options, want) {
		t.Errorf("Options = %+v, want %+v", spec.Options, want)
	}
	if v, err := spec.Render("a"); err != nil || v != `"a"` {
		t.Errorf("Render = %q, %v", v, err)
	}
}

func TestFromResponseScore(t *testing.T) {
	// JSON-decoded schemas carry integers as float64.
	p := RawSchema(map[string]any{"type": "integer", "enum": []any{float64(2), float64(0), float64(1)}})
	spec, err := FromResponse(p)
	if err != nil {
		t.Fatalf("FromResponse error: %v", err)
	}
	if spec.Kind != DecisionScore {
		t.Errorf("Kind = %q, want score", spec.Kind)
	}
	want := []DecisionOption{{Value: int64(0), Name: "0"}, {Value: int64(1), Name: "1"}, {Value: int64(2), Name: "2"}}
	if !reflect.DeepEqual(spec.Options, want) {
		t.Errorf("Options = %+v, want %+v (sorted by value)", spec.Options, want)
	}
	if !spec.IsRubric() {
		t.Error("IsRubric() = false for levels 0..2")
	}
	if got := spec.SnapScore(2.6); got != 2 {
		t.Errorf("SnapScore(2.6) = %d, want 2 (clamped)", got)
	}
	if got := spec.SnapScore(1.4); got != 1 {
		t.Errorf("SnapScore(1.4) = %d, want 1", got)
	}
	if got := spec.SnapScore(-5); got != 0 {
		t.Errorf("SnapScore(-5) = %d, want 0", got)
	}
}

func TestFromResponseProbability(t *testing.T) {
	p := RawSchema(map[string]any{"type": "number", "minimum": float64(0), "maximum": float64(1)})
	spec, err := FromResponse(p)
	if err != nil {
		t.Fatalf("FromResponse error: %v", err)
	}
	if spec.Kind != DecisionPredicate || spec.Boolean {
		t.Errorf("spec = %+v, want a probability predicate", spec)
	}
	if v := spec.PredicateValue(0.7, 0.5); v != 0.7 {
		t.Errorf("PredicateValue = %v, want the raw probability", v)
	}
}

func TestFromResponseEnvelopeScore(t *testing.T) {
	p := RawSchema(wrapDataSchema(map[string]any{"type": "integer", "enum": []int64{2, 0, 1}}))
	spec, err := FromResponse(p)
	if err != nil {
		t.Fatalf("FromResponse error: %v", err)
	}
	if spec.Kind != DecisionScore || !spec.Embedded || !spec.IsRubric() {
		t.Errorf("spec = %+v", spec)
	}
	if v, err := spec.Render(1); err != nil || v != `{"data":1}` {
		t.Errorf("Render(1) = %q, %v", v, err)
	}
}

func TestFromResponseNotADecision(t *testing.T) {
	tests := []struct {
		name string
		p    Proto
	}{
		{"nil", nil},
		{"no schema", RawSchema(nil)},
		{"object schema", NewSchema[point]("P", "")},
		{"plain string", RawSchema(map[string]any{"type": "string"})},
		{"bool with wrong enum", RawSchema(map[string]any{"type": "boolean", "enum": []any{true}})},
		{"float enum", RawSchema(map[string]any{"type": "number", "enum": []any{0.5, 1.5}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := FromResponse(tt.p); !errors.Is(err, ErrNotADecision) {
				t.Errorf("FromResponse error = %v, want ErrNotADecision", err)
			}
		})
	}
}

func TestQuestionSchemaChoice(t *testing.T) {
	q := Question{Text: "Department?", Options: []DecisionOption{
		{Value: "billing", Name: "Billing"},
		{Value: "sales"},
	}}
	p := QuestionSchema(q)
	spec, err := FromResponse(p)
	if err != nil {
		t.Fatalf("FromResponse error: %v", err)
	}
	if spec.Kind != DecisionChoice || spec.Question != "Department?" || !spec.Embedded {
		t.Errorf("spec = %+v", spec)
	}
	if len(spec.Options) != 2 || spec.Options[0].Value != "billing" || spec.Options[1].Value != "sales" {
		t.Errorf("Options = %+v", spec.Options)
	}
	if v, err := p.Validate(`{"data":"billing"}`, nil); err != nil || v != "billing" {
		t.Errorf("Validate = %v, %v", v, err)
	}
	if _, err := p.Validate(`{"data":"nope"}`, nil); err == nil {
		t.Error("Validate accepted a value outside the enum")
	}
}

func TestQuestionSchemaScore(t *testing.T) {
	q := Question{Text: "Rate 0-2", Options: []DecisionOption{{Value: 0}, {Value: 1}, {Value: 2}}}
	p := QuestionSchema(q)
	spec, err := FromResponse(p)
	if err != nil {
		t.Fatalf("FromResponse error: %v", err)
	}
	if spec.Kind != DecisionScore || !spec.IsRubric() {
		t.Errorf("spec = %+v", spec)
	}
	if v, err := p.Validate(`{"data":1}`, nil); err != nil || v != 1 {
		t.Errorf("Validate = %v, %v", v, err)
	}
	if _, err := p.Validate(`{"data":3}`, nil); err == nil {
		t.Error("Validate accepted a level outside the enum")
	}
}
