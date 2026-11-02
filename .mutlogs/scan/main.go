package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
)

type pt struct {
	line, col int
	desc      string
}

func main() {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, os.Args[1], nil, parser.ParseComments)
	if err != nil {
		panic(err)
	}
	var pts []pt
	ast.Inspect(f, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			var desc string
			switch e.Op {
			case token.EQL:
				desc = "== -> !="
			case token.NEQ:
				desc = "!= -> =="
			case token.LSS:
				desc = "< -> <="
			case token.LEQ:
				desc = "<= -> <"
			case token.GTR:
				desc = "> -> >="
			case token.GEQ:
				desc = ">= -> >"
			case token.LAND:
				desc = "&& -> ||"
			case token.LOR:
				desc = "|| -> &&"
			case token.ADD:
				if numericOperands(e) {
					desc = "+ -> -"
				}
			}
			if desc != "" {
				pos := fset.Position(e.OpPos)
				pts = append(pts, pt{pos.Line, pos.Column, desc})
			}
		case *ast.UnaryExpr:
			if e.Op == token.NOT {
				pos := fset.Position(e.OpPos)
				pts = append(pts, pt{pos.Line, pos.Column, "remove !"})
			}
		}
		return true
	})
	sort.Slice(pts, func(i, j int) bool { return pts[i].line < pts[j].line || (pts[i].line == pts[j].line && pts[i].col < pts[j].col) })
	for i, p := range pts {
		fmt.Printf("%d\t%s:%d:%d\t%s\n", i+1, os.Args[1], p.line, p.col, p.desc)
	}
}

func numericOperands(e ast.Expr) bool {
	be, ok := e.(*ast.BinaryExpr)
	return ok && be.Op == token.ADD && !stringyExpr(be.X) && !stringyExpr(be.Y)
}

func stringyExpr(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.BasicLit:
		return t.Kind == token.STRING
	case *ast.ParenExpr:
		return stringyExpr(t.X)
	case *ast.BinaryExpr:
		return t.Op == token.ADD && (stringyExpr(t.X) || stringyExpr(t.Y))
	}
	return false
}
