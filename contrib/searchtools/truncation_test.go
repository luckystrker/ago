package searchtools

import (
	"testing"
)

// TestTruncateRunes asserts text is cut by Unicode code points, the unit Python
// slices by, including the boundary cases the byte-based version got wrong.
func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		max     int
		want    string
		wantCut bool
	}{
		{"cyrillic cut", "привет мир", 5, "приве", true},
		{"exact fit", "привет", 6, "привет", false},
		{"one over", "привет", 5, "приве", true},
		{"max zero", "привет", 0, "", true},
		{"negative max keeps all", "привет", -1, "привет", false},
		{"empty text", "", 0, "", false},
		{"ascii cut", "abcdef", 3, "abc", true},
		{"emoji cut", "a😀b", 2, "a😀", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := truncate(tc.text, tc.max)
			if got != tc.want || cut != tc.wantCut {
				t.Fatalf("truncate(%q, %d) = %q, %v; want %q, %v", tc.text, tc.max, got, cut, tc.want, tc.wantCut)
			}
		})
	}
}

// TestTruncateLinksRunes asserts link budgets count characters and stop as soon
// as the running total passes the cap.
func TestTruncateLinksRunes(t *testing.T) {
	cyrillic := []string{"абв", "где", "ёжз"}
	cases := []struct {
		name    string
		links   []string
		max     int
		want    []string
		wantCut bool
	}{
		{"exact fit", cyrillic, 9, []string{"абв", "где", "ёжз"}, false},
		{"partial", cyrillic, 6, []string{"абв", "где"}, true},
		{"one over", cyrillic, 5, []string{"абв"}, true},
		{"zero cap", cyrillic, 0, []string{}, true},
		{"negative keeps all", cyrillic, -1, cyrillic, false},
		{"empty", nil, 3, []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := truncateLinks(tc.links, tc.max)
			if len(got) != len(tc.want) || cut != tc.wantCut {
				t.Fatalf("truncateLinks(%v, %d) = %v, %v; want %v, %v", tc.links, tc.max, got, cut, tc.want, tc.wantCut)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("truncateLinks(%v, %d)[%d] = %q, want %q", tc.links, tc.max, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestGlasserContentRunes asserts the content budget of one Glasser field is
// spent in characters, including the exact-fit and exhausted-budget edges.
func TestGlasserContentRunes(t *testing.T) {
	got, remaining, cut := glasserContent("привет мир", 5)
	if got != "приве" || remaining != 0 || !cut {
		t.Fatalf("glasserContent = %v, %d, %v", got, remaining, cut)
	}
	got, remaining, cut = glasserContent("привет", 6)
	if got != "привет" || remaining != 0 || cut {
		t.Fatalf("exact fit = %v, %d, %v; want the value untouched", got, remaining, cut)
	}
	got, remaining, cut = glasserContent("привет", 10)
	if got != "привет" || remaining != 4 || cut {
		t.Fatalf("budget = %v, %d, %v; want four characters left", got, remaining, cut)
	}
	// A non-string value that fits stays an object; one that does not becomes a
	// character-bounded string prefix.
	got, remaining, cut = glasserContent(map[string]any{"key": "привет"}, 100)
	if remaining != 100-runeLen(`{"key": "привет"}`) || cut {
		t.Fatalf("object budget = %v, %d, %v", got, remaining, cut)
	}
	if _, ok := got.(map[string]any); !ok {
		t.Fatalf("a fitting object must stay an object, got %T", got)
	}
	got, remaining, cut = glasserContent(map[string]any{"key": "привет"}, 3)
	if got != `{"k` || remaining != 0 || !cut {
		t.Fatalf("cut object = %v, %d, %v", got, remaining, cut)
	}
}
