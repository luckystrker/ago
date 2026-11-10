package searchtools

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	agocontext "github.com/luckystrker/ago/context"
)

// TestToolSchemas asserts the model-facing parameter schema of every tool: the
// property names (Python parity: each tool takes exactly the arguments the
// extension's function signature exposes) and the required set.
func TestToolSchemas(t *testing.T) {
	want := map[string]struct {
		properties []string
		required   []string
	}{
		"exa_search":               {[]string{"query"}, []string{"query"}},
		"exa_find_similar":         {[]string{"url"}, []string{"url"}},
		"exa_get_contents":         {[]string{"urls"}, []string{"urls"}},
		"exa_answer":               {[]string{"query"}, []string{"query"}},
		"firecrawl_search":         {[]string{"query"}, []string{"query"}},
		"firecrawl_scrape":         {[]string{"url"}, []string{"url"}},
		"glasser_web_search":       {[]string{"query"}, []string{"query"}},
		"glasser_company_profile":  {[]string{"domain"}, []string{"domain"}},
		"glasser_find_people":      {[]string{"company_domain", "job_titles", "keywords", "locations"}, []string{}},
		"glasser_keyword_overview": {[]string{"keywords"}, []string{"keywords"}},
		"glasser_social_search":    {[]string{"platform", "query"}, []string{"platform", "query"}},
		"glasser_stock_quote":      {[]string{"symbol"}, []string{"symbol"}},
		"keenable_search":          {[]string{"query"}, []string{"query"}},
		"keenable_fetch":           {[]string{"url"}, []string{"url"}},
		"serply_web_search":        {[]string{"query"}, []string{"query"}},
		"serply_news_search":       {[]string{"query"}, []string{"query"}},
		"serply_scholar_search":    {[]string{"query"}, []string{"query"}},
		"tinyfish_search":          {[]string{"query"}, []string{"query"}},
		"tinyfish_fetch":           {[]string{"urls"}, []string{"urls"}},
		"xquik_tweet_search":       {[]string{"query"}, []string{"query"}},
	}
	toolkits := []interface {
		Schemas(*agocontext.Context) ([]any, error)
	}{}
	for _, build := range []func() (interface {
		Schemas(*agocontext.Context) ([]any, error)
	}, error){
		func() (interface {
			Schemas(*agocontext.Context) ([]any, error)
		}, error) {
			return NewExaToolkit("k")
		},
		func() (interface {
			Schemas(*agocontext.Context) ([]any, error)
		}, error) {
			return NewFirecrawlToolkit("k")
		},
		func() (interface {
			Schemas(*agocontext.Context) ([]any, error)
		}, error) {
			return NewGlasserToolkit("k")
		},
		func() (interface {
			Schemas(*agocontext.Context) ([]any, error)
		}, error) {
			return NewKeenableSearchToolkit("")
		},
		func() (interface {
			Schemas(*agocontext.Context) ([]any, error)
		}, error) {
			return NewSerplySearchToolkit("k")
		},
		func() (interface {
			Schemas(*agocontext.Context) ([]any, error)
		}, error) {
			return NewTinyFishSearchToolkit("k")
		},
		func() (interface {
			Schemas(*agocontext.Context) ([]any, error)
		}, error) {
			return NewXquikSearchToolkit("k")
		},
	} {
		tk, err := build()
		if err != nil {
			t.Fatalf("build toolkit: %v", err)
		}
		toolkits = append(toolkits, tk)
	}
	_, cx := newTestContext()
	seen := 0
	for _, tk := range toolkits {
		schemas, err := tk.Schemas(cx)
		if err != nil {
			t.Fatalf("Schemas: %v", err)
		}
		for _, raw := range schemas {
			b, err := json.Marshal(raw)
			if err != nil {
				t.Fatalf("marshal schema: %v", err)
			}
			var schema struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]any `json:"properties"`
					Required   []string       `json:"required"`
				} `json:"parameters"`
			}
			if err := json.Unmarshal(b, &schema); err != nil {
				t.Fatalf("decode schema: %v", err)
			}
			expected, ok := want[schema.Name]
			if !ok {
				t.Fatalf("unexpected tool schema %q", schema.Name)
			}
			seen++
			properties := make([]string, 0, len(schema.Parameters.Properties))
			for name := range schema.Parameters.Properties {
				properties = append(properties, name)
			}
			sort.Strings(properties)
			required := append([]string{}, schema.Parameters.Required...)
			sort.Strings(required)
			if strings.Join(properties, ",") != strings.Join(expected.properties, ",") {
				t.Fatalf("%s properties = %v, want %v", schema.Name, properties, expected.properties)
			}
			if strings.Join(required, ",") != strings.Join(expected.required, ",") {
				t.Fatalf("%s required = %v, want %v", schema.Name, required, expected.required)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("schemas = %d, want %d", seen, len(want))
	}
}
