package main

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
)

// Dumps the exact mutant bytes the harness would write for the Nth NEQ/unary-NOT
// point on the given line: byte-range replace + format.Source, same as
// benchmarks/mutation mutantSource.
func main() {
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var wantLine, wantIdx int
	fmt.Sscanf(os.Args[2], "%d", &wantLine)
	fmt.Sscanf(os.Args[3], "%d", &wantIdx)
	repl := os.Args[4]
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, os.Args[1], src, parser.ParseComments)
	if err != nil {
		panic(err)
	}
	idx := 0
	start, end := -1, -1
	ast.Inspect(f, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			if e.Op == token.NEQ {
				pos := fset.Position(e.OpPos)
				if pos.Line == wantLine {
					idx++
					if wantIdx == idx {
						start = pos.Offset
						end = start + len(e.Op.String())
					}
				}
			}
		case *ast.UnaryExpr:
			if e.Op == token.NOT {
				pos := fset.Position(e.OpPos)
				if pos.Line == wantLine {
					idx++
					if wantIdx == idx {
						start = pos.Offset
						end = pos.Offset
					}
				}
			}
		}
		return true
	})
	if start < 0 {
		fmt.Println("point not found")
		os.Exit(1)
	}
	mut := append(append(append([]byte{}, src[:start]...), repl...), src[end:]...)
	mut, err = format.Source(mut)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[5], mut, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s (%d bytes); replaced src[%d:%d]\n", os.Args[5], len(mut), start, end)
}
