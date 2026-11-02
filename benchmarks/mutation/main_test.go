package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"testing"
)

func TestScanMutations(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string // mutation descs in source order
	}{
		{"none", "package p\n\nfunc f() {}\n", nil},
		{"eql", "package p\n\nfunc f(a, b int) bool { return a == b }\n", []string{"== -> !="}},
		{"neq", "package p\n\nfunc f(a, b int) bool { return a != b }\n", []string{"!= -> =="}},
		{"lss", "package p\n\nfunc f(a, b int) bool { return a < b }\n", []string{"< -> <="}},
		{"leq", "package p\n\nfunc f(a, b int) bool { return a <= b }\n", []string{"<= -> <"}},
		{"gtr", "package p\n\nfunc f(a, b int) bool { return a > b }\n", []string{"> -> >="}},
		{"geq", "package p\n\nfunc f(a, b int) bool { return a >= b }\n", []string{">= -> >"}},
		{"land", "package p\n\nfunc f(a, b bool) bool { return a && b }\n", []string{"&& -> ||"}},
		{"lor", "package p\n\nfunc f(a, b bool) bool { return a || b }\n", []string{"|| -> &&"}},
		{"not", "package p\n\nfunc f(a bool) bool { return !a }\n", []string{"remove !"}},
		{"plus-int", "package p\n\nfunc f(a int) int { return a + 1 }\n", []string{"+ -> -"}},
		{"plus-string-operand", "package p\n\nfunc f(a string) string { return a + \"s\" }\n", nil},
		// "x" + a + "y": the inner "+" has a string operand and the outer one
		// nests it, so neither may be mutated.
		{"plus-string-chain", "package p\n\nfunc f(a string) string { return \"x\" + a + \"y\" }\n", nil},
		{"shift-untouched", "package p\n\nfunc f(a int) int { return a << 2 }\n", nil},
		{"assign-plus-untouched", "package p\n\nfunc f(a *int) { *a += 1 }\n", nil},
		// ast.Inspect visits the parent "&&" before its operand comparisons;
		// the result must come back sorted by source position anyway.
		{"sorted-by-position",
			"package p\n\nfunc f(a, b, c, d int) bool { return a < b && c > d }\n",
			[]string{"< -> <=", "&& -> ||", "> -> >="}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pts, err := scanMutations(tt.name+".go", []byte(tt.src))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(pts) != len(tt.want) {
				t.Fatalf("got %d point(s) %v, want %d", len(pts), descs(pts), len(tt.want))
			}
			for i, w := range tt.want {
				if pts[i].desc != w {
					t.Errorf("point %d: desc %q, want %q", i, pts[i].desc, w)
				}
			}
		})
	}
}

func descs(pts []mutationPoint) []string {
	out := make([]string, len(pts))
	for i, p := range pts {
		out[i] = p.desc
	}
	return out
}

func TestMutantSource(t *testing.T) {
	src := []byte("package p\n\n// a comment to prove round-tripping keeps comments\nfunc f(a, b int) bool {\n\treturn a == b\n}\n")
	pts, err := scanMutations("t.go", src)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d point(s), want 1", len(pts))
	}
	mut, err := mutantSource(src, pts[0])
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if want := "a != b"; !bytes.Contains(mut, []byte(want)) {
		t.Errorf("mutant does not contain %q:\n%s", want, mut)
	}
	if banned := "a == b"; bytes.Contains(mut, []byte(banned)) {
		t.Errorf("mutant still contains %q:\n%s", banned, mut)
	}
	if want := "// a comment"; !bytes.Contains(mut, []byte(want)) {
		t.Errorf("mutant lost comments:\n%s", mut)
	}
	// The mutant must remain a parseable Go file.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "t.go", mut, 0); err != nil {
		t.Errorf("mutant does not parse: %v", err)
	}
}
