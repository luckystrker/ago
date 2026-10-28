package response

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// DecisionKind classifies a response schema read as one typed question (Python
// DecisionKind). Decision-only providers (TypeSafe, OpenAI Decisions) answer the
// question instead of generating text.
type DecisionKind string

const (
	DecisionPredicate DecisionKind = "predicate"
	DecisionChoice    DecisionKind = "choice"
	DecisionScore     DecisionKind = "score"
)

// ErrNotADecision is returned by FromResponse when the schema is not a yes/no, choice
// or score question (Python NotADecisionError).
var ErrNotADecision = errors.New("response: schema is not a decision type")

// DecisionOption is one answer of a choice or one level of a score.
//
// AGO-DEVIATION: Python derives Name from Enum member names and Description from member
// docstrings or Question markers; Go has no equivalent introspection, so Name defaults
// to the value as text and Description stays empty unless a caller sets it. Option
// names do not travel through the JSON schema either — FromResponse reconstructs
// options from the enum values.
type DecisionOption struct {
	Value       any // the schema enum value: string or integer
	Name        string
	Description string
}

// Question describes a decision question (Python's Annotated Question marker). In Go it
// is passed to QuestionSchema, which builds the boolean/enum schema for it.
type Question struct {
	Text    string
	Options []DecisionOption
}

// QuestionSchema builds a Proto that asks the model q: a boolean predicate when q has
// no options, an integer score when every option value is an integer, and a string
// choice otherwise. The schema uses the {"data": ...} envelope, so
// DecisionSpec.FromResponse reads it back. The schema is named "Question".
func QuestionSchema(q Question) Proto {
	if len(q.Options) == 0 {
		return newSchemaFromMap[bool]("Question", q.Text, wrapDataSchema(questionNode("boolean", nil, q.Text)))
	}
	if values, ok := intOptionValues(q.Options); ok {
		return newSchemaFromMap[int]("Question", q.Text, wrapDataSchema(questionNode("integer", values, q.Text)))
	}
	values := make([]string, 0, len(q.Options))
	for _, o := range q.Options {
		values = append(values, fmt.Sprint(o.Value))
	}
	return newSchemaFromMap[string]("Question", q.Text, wrapDataSchema(questionNode("string", values, q.Text)))
}

func questionNode(typ string, enum any, question string) map[string]any {
	node := map[string]any{"type": typ}
	if enum != nil {
		node["enum"] = enum
	}
	if question != "" {
		node["description"] = question
	}
	return node
}

func intOptionValues(opts []DecisionOption) ([]int64, bool) {
	values := make([]int64, 0, len(opts))
	for _, o := range opts {
		n, ok := asInt(o.Value)
		if !ok {
			return nil, false
		}
		values = append(values, n)
	}
	return values, true
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	case float64:
		if n == math.Trunc(n) {
			return int64(n), true
		}
	}
	return 0, false
}

// DecisionSpec is a response schema read as one typed question; decision-only providers
// translate it into their request format (Python DecisionSpec).
type DecisionSpec struct {
	Kind     DecisionKind
	Question string
	Options  []DecisionOption
	Boolean  bool // a predicate answering bool rather than a raw probability
	Embedded bool // the schema wrapped the value as {"data": ...}
}

// FromResponse reads p as a decision question; the returned error wraps
// ErrNotADecision when p is not one.
func FromResponse(p Proto) (*DecisionSpec, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: a response schema is required", ErrNotADecision)
	}
	root := p.JSONSchema()
	if len(root) == 0 {
		return nil, fmt.Errorf("%w: schema %q has no JSON schema", ErrNotADecision, p.Name())
	}
	node, embedded := decisionNode(root)

	question := p.Description()
	if question == "" {
		question, _ = node["description"].(string)
	}

	values := enumAny(node["enum"])
	typ, _ := node["type"].(string)
	isBool := typ == "boolean" && (values == nil || isBoolPair(values))
	minimum, hasMin := numberAt(node, "minimum")
	maximum, hasMax := numberAt(node, "maximum")
	isProbability := typ == "number" && hasMin && hasMax && minimum == 0 && maximum == 1
	if isBool || isProbability {
		return &DecisionSpec{
			Kind:     DecisionPredicate,
			Question: question,
			Boolean:  isBool,
			Embedded: embedded,
		}, nil
	}
	if len(values) > 0 && allStrings(values) {
		return &DecisionSpec{
			Kind:     DecisionChoice,
			Question: question,
			Embedded: embedded,
			Options:  optionsFromValues(values),
		}, nil
	}
	if ints, ok := allInts(values); ok && len(ints) > 0 {
		sort.Slice(ints, func(i, j int) bool { return ints[i] < ints[j] })
		return &DecisionSpec{
			Kind:     DecisionScore,
			Question: question,
			Embedded: embedded,
			Options:  optionsFromInts(ints),
		}, nil
	}
	return nil, fmt.Errorf("%w: schema %q is not a predicate, choice or score", ErrNotADecision, p.Name())
}

// decisionNode locates the schema node the decision is about and whether it sat in the
// {"data": ...} envelope (Python _decision_node). The envelope's description is
// framework boilerplate, not the question. Generated schemas carry no $defs, but
// hand-built ones may; a top-level "#/$defs/..." reference is resolved like Python does.
func decisionNode(root map[string]any) (map[string]any, bool) {
	node := root
	embedded := false
	if typ, _ := root["type"].(string); typ == "object" {
		if props, ok := root["properties"].(map[string]any); ok {
			if len(props) == 1 {
				if data, ok := props["data"].(map[string]any); ok {
					stripped := make(map[string]any, len(data))
					for k, v := range data {
						if k == "description" {
							continue
						}
						stripped[k] = v
					}
					node, embedded = stripped, true
				}
			}
		}
	}
	if ref, ok := node["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
		if defs, ok := root["$defs"].(map[string]any); ok {
			if def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any); ok {
				node = def
			}
		}
	}
	return node, embedded
}

func isBoolPair(values []any) bool {
	if len(values) != 2 {
		return false
	}
	a, aok := values[0].(bool)
	b, bok := values[1].(bool)
	return aok && bok && a != b
}

func allStrings(values []any) bool {
	for _, v := range values {
		if _, ok := v.(string); !ok {
			return false
		}
	}
	return true
}

// allInts accepts integral floats as well: JSON-decoded schemas carry integers as
// float64 (Python checks isinstance(int), which Go JSON decoding never yields).
func allInts(values []any) ([]int64, bool) {
	out := make([]int64, 0, len(values))
	for _, v := range values {
		n, ok := asInt(v)
		if !ok {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func numberAt(node map[string]any, key string) (float64, bool) {
	v, ok := node[key]
	if !ok {
		return 0, false
	}
	return numericValue(v)
}

func optionsFromValues(values []any) []DecisionOption {
	out := make([]DecisionOption, 0, len(values))
	for _, v := range values {
		out = append(out, DecisionOption{Value: v, Name: fmt.Sprint(v)})
	}
	return out
}

func optionsFromInts(values []int64) []DecisionOption {
	out := make([]DecisionOption, 0, len(values))
	for _, n := range values {
		out = append(out, DecisionOption{Value: n, Name: fmt.Sprint(n)})
	}
	return out
}

// IsRubric reports whether the options are the levels 0..n-1 in order, as the score
// APIs number them.
func (s *DecisionSpec) IsRubric() bool {
	for i, o := range s.Options {
		if n, ok := asInt(o.Value); !ok || n != int64(i) {
			return false
		}
	}
	return true
}

// PredicateValue maps a probability to the schema's value: the thresholded bool for
// boolean predicates, the raw probability otherwise.
func (s *DecisionSpec) PredicateValue(probability, threshold float64) any {
	if s.Boolean {
		return probability >= threshold
	}
	return probability
}

// SnapScore clamps a score to the nearest valid level, in rubric order.
// Note: rounding at exact .5 is away from zero (math.Round), while Python's round()
// uses banker's rounding — a deliberate, documented difference.
func (s *DecisionSpec) SnapScore(score float64) int {
	if len(s.Options) == 0 {
		return 0
	}
	n := int(math.Round(score))
	limit := len(s.Options) - 1
	if n < 0 {
		return 0
	}
	if n > limit {
		return limit
	}
	return n
}

// Render marshals value into the JSON the schema validates.
func (s *DecisionSpec) Render(value any) (string, error) {
	if s.Embedded {
		value = map[string]any{"data": value}
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
