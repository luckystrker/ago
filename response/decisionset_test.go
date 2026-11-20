package response

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- fixtures (Python test_decision_set.py: TicketTriage, Department, Urgency) -----------

type ticketTriage struct {
	IsSpam     bool   `json:"is_spam" question:"Is this message spam?"`
	Department string `json:"department" question:"Which team should handle this ticket?" agschema:"enum=billing|technical"`
	Urgency    int    `json:"urgency" question:"How urgent is this ticket?" agschema:"enum=0|1"`
}

type churn struct {
	Likelihood float64 `json:"likelihood" question:"How likely is this customer to churn?" agschema:"min=0,max=1"`
}

type describedFallback struct {
	IsSpam bool `json:"is_spam" agschema:"description=Is this message spam?"`
}

type questionOutranksDescription struct {
	IsSpam bool `json:"is_spam" question:"From the question tag" agschema:"description=From the description"`
}

type commaQuestion struct {
	IsSpam bool `json:"is_spam" question:"Is this spam, or a scam?"`
}

type singleDataField struct {
	Data bool `json:"data" question:"Is this message spam?"`
}

type untaggedName struct {
	IsSpam bool `question:"Is this message spam?"`
}

type skipsHidden struct {
	IsSpam bool   `json:"is_spam" question:"Is this message spam?"`
	Hidden bool   `json:"-"`
	secret string //nolint:unused // exercises the unexported-field skip
}

type base struct {
	IsSpam bool `json:"is_spam" question:"Is this message spam?"`
}

type embedsBase struct {
	base
	Department string `json:"department" question:"Which team?" agschema:"enum=billing|technical"`
}

// embedsCollide has the embedded base's "is_spam" key and an outer field of its own under
// the same key; encoding/json would let the outer one shadow it silently.
type embedsCollide struct {
	base
	Dup bool `json:"is_spam" question:"Is this spam, again?"`
}

type leaf struct {
	IsCustomer bool `json:"is_customer" question:"Is the sender a customer?"`
}

type nested struct {
	Sender leaf `json:"sender" question:"Who sent it?"`
}

type nestedTime struct {
	At time.Time `json:"at" question:"When?"`
}

type pointerField struct {
	IsSpam *bool `json:"is_spam" question:"Is this message spam?"`
}

type embedsPointer struct {
	*base
}

// Base is exported so the embedded field is a field of its own once it carries a json name.
type Base struct {
	IsSpam bool `json:"is_spam" question:"Is this message spam?"`
}

type embedsTagged struct {
	Base `json:"base"`
}

type sliceField struct {
	Flags []bool `json:"flags" question:"Which flags apply?"`
}

type arrayField struct {
	Flags [2]bool `json:"flags" question:"Which flags apply?"`
}

type mapField struct {
	Flags map[string]bool `json:"flags" question:"Which flags apply?"`
}

type ifaceField struct {
	Any any `json:"any" question:"Anything?"`
}

type emptyStruct struct{}

type onlyHidden struct {
	Hidden bool `json:"-"`
}

type keyOfAnotherFieldName struct {
	IsSpam   bool `json:"IsUrgent" question:"Is this message spam?"`
	IsUrgent bool `question:"Is this ticket urgent?"`
}

type noQuestion struct {
	IsSpam bool `json:"is_spam"`
}

type blankQuestion struct {
	IsSpam bool `json:"is_spam" question:"   "`
}

type plainString struct {
	Note string `json:"note" question:"A note?"`
}

type plainInt struct {
	N int `json:"n" question:"How many?"`
}

type unboundedFloat struct {
	N float64 `json:"n" question:"How likely?"`
}

type badAgschema struct {
	IsSpam bool `json:"is_spam" question:"Spam?" agschema:"bogus=1"`
}

type gappedScore struct {
	Urgency int `json:"urgency" question:"How urgent?" agschema:"enum=1|3"`
}

func schemaOf[T any](description string) Proto { return NewSchema[T]("", description) }

func mustSet(t *testing.T, p Proto) *DecisionSet {
	t.Helper()
	d, err := NewDecisionSet(p)
	if err != nil {
		t.Fatalf("NewDecisionSet: %v", err)
	}
	return d
}

// --- positive cases -----------------------------------------------------------------------

func TestDecisionSetScalarIsOneQuestionNamedAnswer(t *testing.T) {
	// Python: test_scalar_schema_is_one_question_named_answer.
	cases := []struct {
		name   string
		p      Proto
		kind   DecisionKind
		render string
		value  any
	}{
		{"bool", NewSchema[bool]("B", "Is it a refund?"), DecisionPredicate, `{"data":true}`, true},
		{"choice", RawSchema(map[string]any{"type": "string", "enum": []any{"billing", "technical"}}, WithRawName("D"), WithRawDescription("Which team?")), DecisionChoice, `"billing"`, "billing"},
		{"question schema", QuestionSchema(Question{Text: "How bad?", Options: []DecisionOption{{Value: 0}, {Value: 1}}}), DecisionScore, `{"data":1}`, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := mustSet(t, c.p)
			if !d.IsScalar() || d.Len() != 1 || !reflect.DeepEqual(d.Names(), []string{"answer"}) {
				t.Fatalf("scalar=%v names=%v", d.IsScalar(), d.Names())
			}
			spec, ok := d.Spec(AnswerKey)
			if !ok || spec.Kind != c.kind {
				t.Fatalf("spec = %+v, %v", spec, ok)
			}
			got, err := d.Render(map[string]any{"answer": c.value})
			if err != nil || got != c.render {
				t.Fatalf("Render = %q, %v; want %q", got, err, c.render)
			}
			// The scalar path reads exactly what FromResponse reads.
			want, err := FromResponse(c.p)
			if err != nil || !reflect.DeepEqual(*want, spec) {
				t.Fatalf("FromResponse = %+v, %v; DecisionSet spec = %+v", want, err, spec)
			}
		})
	}
}

func TestDecisionSetScalarNotADecisionKeepsSentinel(t *testing.T) {
	for name, p := range map[string]Proto{
		"nil":          nil,
		"plain string": NewSchema[string]("S", "text"),
		"list":         NewSchema[[]string]("L", "list"),
		"time":         NewSchema[time.Time]("T", "when"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewDecisionSet(p); !errors.Is(err, ErrNotADecision) {
				t.Fatalf("err = %v, want ErrNotADecision", err)
			}
		})
	}
}

func TestDecisionSetStructOneQuestionPerField(t *testing.T) {
	// Python: test_object_schema_is_one_question_per_field.
	d := mustSet(t, schemaOf[ticketTriage](""))
	if d.IsScalar() || d.Len() != 3 {
		t.Fatalf("scalar=%v len=%d", d.IsScalar(), d.Len())
	}
	want := []struct {
		name     string
		kind     DecisionKind
		question string
	}{
		{"is_spam", DecisionPredicate, "Is this message spam?"},
		{"department", DecisionChoice, "Which team should handle this ticket?"},
		{"urgency", DecisionScore, "How urgent is this ticket?"},
	}
	qs := d.Questions()
	if len(qs) != len(want) {
		t.Fatalf("questions = %+v", qs)
	}
	for i, w := range want {
		q := qs[i]
		if q.Name != w.name || q.Spec.Kind != w.kind || q.Spec.Question != w.question || q.Spec.Embedded {
			t.Errorf("question %d = %+v, want %+v", i, q, w)
		}
	}
	if !qs[0].Spec.Boolean {
		t.Error("a bool field must be a boolean predicate")
	}
	if got := qs[1].Spec.Options; len(got) != 2 || got[0].Value != "billing" || got[1].Value != "technical" {
		t.Errorf("choice options = %+v", got)
	}
	if got := qs[2].Spec.Options; len(got) != 2 || !qs[2].Spec.IsRubric() {
		t.Errorf("score levels = %+v", got)
	}
}

func TestDecisionSetSingleField(t *testing.T) {
	d := mustSet(t, schemaOf[describedFallback](""))
	qs := d.Questions()
	if d.IsScalar() || len(qs) != 1 || qs[0].Name != "is_spam" || qs[0].Spec.Question != "Is this message spam?" {
		t.Fatalf("set = scalar %v, %+v", d.IsScalar(), qs)
	}
}

func TestDecisionSetQuestionSources(t *testing.T) {
	cases := []struct {
		name string
		p    Proto
		want string
	}{
		{"question tag", schemaOf[ticketTriage](""), "Is this message spam?"},
		{"agschema description fallback", schemaOf[describedFallback](""), "Is this message spam?"},
		{"question outranks description", schemaOf[questionOutranksDescription](""), "From the question tag"},
		{"question tag may contain commas", schemaOf[commaQuestion](""), "Is this spam, or a scam?"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustSet(t, c.p).Questions()[0].Spec.Question; got != c.want {
				t.Fatalf("question = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDecisionSetProbabilityFieldIsRawPredicate(t *testing.T) {
	// Python: test_probability_field_is_returned_raw.
	d := mustSet(t, schemaOf[churn](""))
	spec := d.Questions()[0].Spec
	if spec.Kind != DecisionPredicate || spec.Boolean {
		t.Fatalf("spec = %+v", spec)
	}
	if got, err := d.Render(map[string]any{"likelihood": 0.37}); err != nil || got != `{"likelihood":0.37}` {
		t.Fatalf("Render = %q, %v", got, err)
	}
}

func TestDecisionSetSingleFieldNamedDataIsAStruct(t *testing.T) {
	// Python: test_single_field_named_data_is_a_class.
	d := mustSet(t, schemaOf[singleDataField](""))
	if d.IsScalar() || !reflect.DeepEqual(d.Names(), []string{"data"}) {
		t.Fatalf("scalar=%v names=%v", d.IsScalar(), d.Names())
	}
	if got, err := d.Render(map[string]any{"data": true}); err != nil || got != `{"data":true}` {
		t.Fatalf("Render = %q, %v", got, err)
	}
}

func TestDecisionSetFieldOrderAndKeys(t *testing.T) {
	cases := []struct {
		name string
		p    Proto
		want []string
	}{
		{"declaration order", schemaOf[ticketTriage](""), []string{"is_spam", "department", "urgency"}},
		{"untagged field uses the Go name", schemaOf[untaggedName](""), []string{"IsSpam"}},
		{"json dash and unexported are skipped", schemaOf[skipsHidden](""), []string{"is_spam"}},
		{"embedded struct is flattened in place", schemaOf[embedsBase](""), []string{"is_spam", "department"}},
		{"pointer to the struct is the same set", NewSchema[*ticketTriage]("", ""), []string{"is_spam", "department", "urgency"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustSet(t, c.p).Names(); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("names = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDecisionSetRenderIsInFieldOrderAndValidates(t *testing.T) {
	// Python: test_object_schema_is_one_question_per_field (render) — answers given in a
	// different order come out in field order, and the result validates as the struct.
	p := schemaOf[ticketTriage]("")
	d := mustSet(t, p)
	got, err := d.Render(map[string]any{"urgency": 1, "is_spam": false, "department": "billing", "extra": "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"is_spam":false,"department":"billing","urgency":1}`; got != want {
		t.Fatalf("Render = %s, want %s", got, want)
	}
	v, err := p.Validate(got, nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if want := (ticketTriage{IsSpam: false, Department: "billing", Urgency: 1}); v != want {
		t.Fatalf("validated = %+v, want %+v", v, want)
	}
}

func TestDecisionSetRenderMissingAnswerNamesTheField(t *testing.T) {
	// Python: test_a_missing_answer_names_the_field (mapper level; here at Render).
	d := mustSet(t, schemaOf[ticketTriage](""))
	_, err := d.Render(map[string]any{"is_spam": true, "department": "billing"})
	if !errors.Is(err, ErrMissingAnswer) || !strings.Contains(err.Error(), `"urgency"`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := mustSet(t, NewSchema[bool]("B", "Q?")).Render(map[string]any{}); !errors.Is(err, ErrMissingAnswer) {
		t.Fatalf("scalar err = %v", err)
	}
}

func TestDecisionSetIsImmutable(t *testing.T) {
	d := mustSet(t, schemaOf[ticketTriage](""))
	names := d.Names()
	names[0] = "mutated"
	qs := d.Questions()
	qs[1].Spec.Options[0].Value = "mutated"
	qs[1].Name = "mutated"
	spec, _ := d.Spec("department")
	spec.Options[1].Value = "mutated"

	if d.Names()[0] != "is_spam" {
		t.Error("Names leaked the internal slice")
	}
	fresh := d.Questions()[1]
	if fresh.Name != "department" || fresh.Spec.Options[0].Value != "billing" || fresh.Spec.Options[1].Value != "technical" {
		t.Errorf("Questions/Spec leaked internal state: %+v", fresh)
	}
	if _, ok := d.Spec("nope"); ok {
		t.Error("Spec found an unknown question")
	}
}

func TestDecisionSetConcurrentUse(t *testing.T) {
	d := mustSet(t, schemaOf[ticketTriage](""))
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				_ = d.Questions()
				_, _ = d.Render(map[string]any{"is_spam": true, "department": "billing", "urgency": 0})
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// --- rejections (Python test_decision_set.py + test_decision_set_providers.py) ---------------

func TestDecisionSetRejections(t *testing.T) {
	cases := []struct {
		name    string
		p       Proto
		contain string // names the field or the reason
	}{
		// Python TestRejections.test_field_that_is_not_a_decision: Nested, Nullable, Listed, Defaulted.
		{"nested struct", schemaOf[nested](""), "Sender"},
		{"nested time.Time", schemaOf[nestedTime](""), "At"},
		{"pointer (Python Optional)", schemaOf[pointerField](""), "IsSpam"},
		{"embedded pointer", schemaOf[embedsPointer](""), "base"},
		{"embedded struct under a json name is nested", schemaOf[embedsTagged](""), "nested struct"},
		{"slice (Python list)", schemaOf[sliceField](""), "Flags"},
		{"array", schemaOf[arrayField](""), "Flags"},
		{"map", schemaOf[mapField](""), "Flags"},
		{"interface", schemaOf[ifaceField](""), "Any"},
		// Python test_class_without_fields.
		{"empty struct", schemaOf[emptyStruct](""), "no fields"},
		{"only skipped fields", schemaOf[onlyHidden](""), "no fields"},
		// Python test_fields_sharing_an_input_key_are_rejected / test_fields_validated_from_one_key.
		{"json key equal to another field's promoted name", schemaOf[keyOfAnotherFieldName](""), `"IsUrgent"`},
		{"embedded field collides with an outer field", schemaOf[embedsCollide](""), `"is_spam"`},
		// Python test_field_without_a_question / test_blank_question_is_no_question.
		{"no question", schemaOf[noQuestion](""), "no question"},
		{"blank question", schemaOf[blankQuestion](""), "no question"},
		// Python test_class_takes_no_question_of_its_own ("description" case).
		{"schema description on a struct", schemaOf[ticketTriage]("Triage this ticket."), "no description of its own"},
		// Field types that are not a predicate, choice or score.
		{"plain string", schemaOf[plainString](""), "Note"},
		{"plain int", schemaOf[plainInt](""), "N"},
		{"float without 0..1 bounds", schemaOf[unboundedFloat](""), "N"},
		{"bad agschema directive", schemaOf[badAgschema](""), "bogus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewDecisionSet(c.p)
			if !errors.Is(err, ErrInvalidDecisionSet) {
				t.Fatalf("err = %v, want ErrInvalidDecisionSet", err)
			}
			if !errors.Is(err, ErrNotADecision) {
				t.Fatalf("err = %v, want it to match ErrNotADecision too", err)
			}
			if !strings.Contains(err.Error(), c.contain) {
				t.Fatalf("err = %q, want it to mention %q", err, c.contain)
			}
		})
	}
}

func TestDecisionSetRejectsTwoFieldsOnOneJSONKey(t *testing.T) {
	// go vet refuses a literal struct with a repeated json tag, so build the type at run time.
	typ := reflect.StructOf([]reflect.StructField{
		{Name: "IsSpam", Type: reflect.TypeOf(true), Tag: `json:"flag" question:"Is this message spam?"`},
		{Name: "IsUrgent", Type: reflect.TypeOf(true), Tag: `json:"flag" question:"Is this ticket urgent?"`},
	})
	var fields []decisionField
	err := collectDecisionFields(typ, nil, &fields, map[string]string{})
	if !errors.Is(err, ErrInvalidDecisionSet) || !strings.Contains(err.Error(), `"flag"`) ||
		!strings.Contains(err.Error(), "IsSpam") || !strings.Contains(err.Error(), "IsUrgent") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecisionSetRejectsAnObjectSchemaWithoutAGoStruct(t *testing.T) {
	// Python's extra-properties and from_schema cases have no Go analogue: a hand-built
	// object schema has no field order, so it is refused instead of guessed.
	p := RawSchema(map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "boolean"}, "b": map[string]any{"type": "boolean"}},
	}, WithRawName("O"))
	if _, err := NewDecisionSet(p); !errors.Is(err, ErrNotADecision) || !strings.Contains(err.Error(), "not built from a Go struct") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecisionSetScoreNotNumberedFromZeroIsReadableHere(t *testing.T) {
	// Python test_score_field_not_numbered_from_zero is a provider rule (the API numbers
	// levels by position); the set itself carries the levels so the provider can refuse.
	// The provider-side rejection is tested in config/openai.
	spec := mustSet(t, schemaOf[gappedScore]("")).Questions()[0].Spec
	if spec.Kind != DecisionScore || spec.IsRubric() {
		t.Fatalf("spec = %+v", spec)
	}
}
