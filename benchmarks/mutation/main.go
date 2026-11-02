// Command mutation is a minimal mutation-testing harness for one AGo package
// file: it applies small systematic edits ("mutants") to a single non-test .go
// file, runs the package tests against every mutant and reports which mutants
// the tests caught (killed) and which they let through (survived). A surviving
// mutant marks logic the suite does not pin down — a hint where tests are
// missing, not a defect in the mutated code.
//
// Mutation set (one edit per mutant):
//
//	== <-> !=    < <-> <=    > <-> >=    && <-> ||    unary ! removed
//	+ -> -       (binary "+" only; skipped when an operand is syntactically
//	              a string literal — see numericOperands)
//
// The harness is syntactic: it resolves no types (stdlib-only), so "+ -> -"
// can still hit string concatenation built from identifiers; such a mutant
// fails to compile and lands in the separate "broken" bucket, which counts as
// neither killed nor survived.
//
// The harness works on exactly one file of one package per run. Before any
// mutant is evaluated the baseline must hold: the package tests pass on the
// unmutated file. Otherwise kill/survive verdicts would be meaningless and
// nothing is mutated.
//
// The original file content is always restored: explicitly after every mutant,
// plus a defer that also covers harness panics, timed-out test runs and
// Ctrl-C. What is restored is the content the harness read at startup, not the
// git HEAD version — running it over uncommitted work is safe. The one
// situation no Go code can defend against is the harness process itself being
// killed outright (SIGKILL): a half-written mutant could survive that.
//
// Usage:
//
//	go run ./benchmarks/mutation -pkg ./stream -file history.go [-max 40] [-run <regex>] [-timeout 60s]
//
// -pkg must be a directory-style package path (./stream): it locates the file
// on disk and is passed to go test verbatim. Output is one line per mutant, a
// human totals line and a final machine-readable JSON line:
//
//	{"total":N,"killed":K,"broken":B,"survived":[{"line":N,"desc":"..."}]}
//
// Mutants run sequentially: they overwrite the same file on disk, so runs
// cannot be parallelized without copying the whole package tree.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// mutationPoint is one concrete edit: replace the source byte range [start,
// end) with repl. line is the 1-based line of the edited token and desc is the
// human/JSON description ("&& -> ||", "remove !").
type mutationPoint struct {
	start, end int
	repl       string
	line       int
	desc       string
}

// survivor is one mutant the package tests let through, in JSON summary form.
type survivor struct {
	Line int    `json:"line"`
	Desc string `json:"desc"`
}

// summary is the final JSON line; the field order fixes the JSON key order.
type summary struct {
	Total    int        `json:"total"`
	Killed   int        `json:"killed"`
	Broken   int        `json:"broken"`
	Survived []survivor `json:"survived"`
}

// testOutcome classifies one go test run.
type testOutcome int

const (
	outcomePass        testOutcome = iota // tests green
	outcomeFail                           // tests ran and failed
	outcomeBuildFailed                    // package never produced a test binary
	outcomeTimedOut                       // run exceeded the timeout and was killed
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mutation:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// -h / -help: usage on stdout and exit 0. flag's own handler (used for
	// malformed flags below) prints to stderr and exits 2.
	for _, arg := range args {
		if arg == "-h" || arg == "-help" || arg == "--help" {
			usage(os.Stdout)
			return nil
		}
	}

	var opts struct {
		pkg, file, run string
		max            int
		timeout        time.Duration
	}
	fs := flag.NewFlagSet("mutation", flag.ExitOnError)
	fs.StringVar(&opts.pkg, "pkg", "", "package directory holding the file, e.g. ./stream (required)")
	fs.StringVar(&opts.file, "file", "", "non-test .go file name inside -pkg (required)")
	fs.IntVar(&opts.max, "max", 40, "evaluate at most this many mutants, in source order (0 = no limit)")
	fs.StringVar(&opts.run, "run", "", "go test -run regex selecting which tests judge the mutants")
	fs.DurationVar(&opts.timeout, "timeout", 60*time.Second, "kill a mutant's go test after this duration")
	fs.Usage = func() { usage(os.Stderr) }
	fs.Parse(args)
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if opts.max < 0 {
		return errors.New("-max must be >= 0")
	}

	// Ctrl-C: cancel the in-flight go test so the run unwinds into the
	// restore defer instead of dying mid-mutant.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	return mutate(ctx, workDir, opts.pkg, opts.file, opts.run, opts.max, opts.timeout)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `mutation — minimal mutation-testing harness for one AGo package file.

Applies small operator-level mutations (== <-> !=, < <-> <=, > <-> >=,
&& <-> ||, unary ! removal, + -> - in numeric binary expressions) to a single
non-test .go file, runs the package tests against every mutant and reports
killed / broken / survived. The package tests must pass on the unmutated file
(baseline) before any mutant runs. The original file content is always
restored — panics, test timeouts and Ctrl-C included.

Usage:
  go run ./benchmarks/mutation -pkg <dir> -file <name.go> [flags]

  -pkg      package directory holding the file, e.g. ./stream (required)
  -file     non-test .go file inside -pkg, e.g. history.go (required)
  -max      evaluate at most this many mutants, in source order
            (default 40, 0 = no limit)
  -run      go test -run regex to select the judging tests
  -timeout  kill a mutant's go test after this duration (default 1m0s)
  -h        this help

Output: one line per mutant, a totals line, and JSON as the last line:
  {"total":N,"killed":K,"broken":B,"survived":[{"line":N,"desc":"..."}]}
`)
}

// mutate is the harness core: baseline run, one test run per mutant, report.
// The target file is restored to its original content no matter how this
// function returns.
func mutate(ctx context.Context, workDir, pkg, file, runRegex string, max int, timeout time.Duration) error {
	if pkg == "" {
		return errors.New("-pkg is required (e.g. -pkg ./stream)")
	}
	if file == "" {
		return errors.New("-file is required (e.g. -file history.go)")
	}
	if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
		return fmt.Errorf("-file %q must be a non-test .go file", file)
	}
	target := filepath.Join(pkg, file)
	original, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("cannot read target (is -pkg a directory path like ./stream?): %w", err)
	}
	mode, err := fileMode(target)
	if err != nil {
		return err
	}

	// The loop restores after every mutant too; this defer is the safety net
	// for panics and early returns between a mutant write and its restore.
	// os.Exit here skips the remaining defers, but the file on disk holds the
	// mutant — failing loudly beats failing silently.
	restore := func() {
		if err := os.WriteFile(target, original, mode); err != nil {
			fmt.Fprintf(os.Stderr, "mutation: FATAL: restoring %s failed: %v\n", target, err)
			os.Exit(1)
		}
	}
	defer restore()

	fmt.Printf("target: %s\n", target)
	if out, err := runTests(ctx, workDir, pkg, runRegex, timeout); err != nil {
		return err
	} else if out != outcomePass {
		return fmt.Errorf("baseline go test did not pass (%s); mutation results would be meaningless", outcomeName(out))
	}
	fmt.Printf("baseline: pass (go test -count=1 %s)\n", pkg)

	points, err := scanMutations(file, original)
	if err != nil {
		return fmt.Errorf("parse %s: %w", target, err)
	}
	evaluated := points
	if max > 0 && len(points) > max {
		evaluated = points[:max]
	}
	fmt.Printf("mutants: found %d mutation point(s), evaluating %d\n", len(points), len(evaluated))

	res := summary{Survived: []survivor{}}
	for i, p := range evaluated {
		out, err := runMutant(ctx, workDir, target, mode, original, pkg, runRegex, timeout, p)
		if err != nil {
			return err // defer restore() puts the original back
		}
		if ctx.Err() != nil {
			return errors.New("interrupted; original file restored")
		}
		verdict, note := "", ""
		switch out {
		case outcomePass:
			res.Survived = append(res.Survived, survivor{Line: p.line, Desc: p.desc})
			verdict = "SURVIVED"
		case outcomeBuildFailed:
			res.Broken++
			verdict, note = "BROKEN", " (compile error)"
		default:
			// outcomeFail and outcomeTimedOut: the tests did not pass, so the
			// mutant did not slip through — a hung suite counts as killed.
			res.Killed++
			verdict = "KILLED"
			if out == outcomeTimedOut {
				note = " (timeout)"
			}
		}
		res.Total++
		fmt.Printf("mutant %d/%d  %-8s %s:%d  %s%s\n", i+1, len(evaluated), verdict, file, p.line, p.desc, note)
	}
	return printSummary(res)
}

// runMutant writes one mutant to target, runs the package tests against it and
// restores the original content before returning.
func runMutant(ctx context.Context, workDir, target string, mode os.FileMode, original []byte, pkg, runRegex string, timeout time.Duration, p mutationPoint) (testOutcome, error) {
	mut, err := mutantSource(original, p)
	if err != nil {
		// Not expected for the supported rules; report honestly as broken and
		// leave nothing on disk.
		return outcomeBuildFailed, nil
	}
	if err := os.WriteFile(target, mut, mode); err != nil {
		return 0, fmt.Errorf("write mutant: %w", err)
	}
	out, err := runTests(ctx, workDir, pkg, runRegex, timeout)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(target, original, mode); err != nil {
		return 0, fmt.Errorf("restore %s: %w", target, err)
	}
	return out, nil
}

// runTests runs `go test -count=1 [-run regex] <pkg>` in workDir and
// classifies the outcome. go test marks packages that never produced a test
// binary with "[build failed]" — that is how a compile-broken mutant is told
// apart from failing tests.
func runTests(ctx context.Context, workDir, pkg, runRegex string, timeout time.Duration) (testOutcome, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"test", "-count=1"}
	if runRegex != "" {
		args = append(args, "-run", runRegex)
	}
	args = append(args, pkg)
	cmd := exec.CommandContext(runCtx, "go", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if runCtx.Err() != nil {
		return outcomeTimedOut, nil
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return 0, fmt.Errorf("running go: %w", err)
	}
	if err != nil {
		if strings.Contains(string(out), "[build failed]") {
			return outcomeBuildFailed, nil
		}
		return outcomeFail, nil
	}
	return outcomePass, nil
}

func outcomeName(o testOutcome) string {
	switch o {
	case outcomePass:
		return "pass"
	case outcomeFail:
		return "tests failed"
	case outcomeBuildFailed:
		return "build failed"
	default:
		return "timed out"
	}
}

// scanMutations parses src and returns every mutation point in source order:
// one per mutable operator occurrence; the byte ranges never overlap.
func scanMutations(name string, src []byte) ([]mutationPoint, error) {
	fset := token.NewFileSet()
	// ParseComments keeps comments in the AST so format.Source can reproduce
	// the unmutated parts of the file unchanged.
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var pts []mutationPoint
	ast.Inspect(f, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.BinaryExpr:
			repl, desc := binaryMutation(e)
			if desc == "" {
				return true
			}
			start := fset.Position(e.OpPos).Offset
			// token.String is the source spelling of the operator, so its
			// byte length bounds the token exactly.
			pts = append(pts, mutationPoint{
				start: start,
				end:   start + len(e.Op.String()),
				repl:  repl,
				line:  fset.Position(e.OpPos).Line,
				desc:  desc,
			})
		case *ast.UnaryExpr:
			if e.Op != token.NOT {
				return true
			}
			// Delete "!" plus any whitespace up to the operand: the span ends
			// at the operand's first byte.
			pts = append(pts, mutationPoint{
				start: fset.Position(e.OpPos).Offset,
				end:   fset.Position(e.X.Pos()).Offset,
				line:  fset.Position(e.OpPos).Line,
				desc:  "remove !",
			})
		}
		return true
	})
	sort.Slice(pts, func(i, j int) bool { return pts[i].start < pts[j].start })
	return pts, nil
}

// binaryMutation describes the one mutation for a binary operator; an empty
// desc means the operator is not mutated. Every supported operator maps both
// ways so repeated runs on survivor reports stay actionable.
func binaryMutation(e *ast.BinaryExpr) (repl, desc string) {
	switch e.Op {
	case token.EQL:
		return "!=", "== -> !="
	case token.NEQ:
		return "==", "!= -> =="
	case token.LSS:
		return "<=", "< -> <="
	case token.LEQ:
		return "<", "<= -> <"
	case token.GTR:
		return ">=", "> -> >="
	case token.GEQ:
		return ">", ">= -> >"
	case token.LAND:
		return "||", "&& -> ||"
	case token.LOR:
		return "&&", "|| -> &&"
	case token.ADD:
		if numericOperands(e) {
			return "-", "+ -> -"
		}
	}
	return "", ""
}

// numericOperands reports whether both sides of a "+" look numeric under a
// syntactic heuristic: a string literal operand — directly, parenthesized or
// nested in a "+" chain — rules the mutation out. Types are not resolved: an
// operand whose string-ness is invisible syntactically (identifier, call) is
// still mutated, and that mutant typically fails to compile and is reported
// as broken rather than killed or survived.
func numericOperands(e *ast.BinaryExpr) bool {
	return !stringyExpr(e.X) && !stringyExpr(e.Y)
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

// mutantSource applies one mutation point to the original source and gofmts
// the result. Repo sources are gofmt-clean, so everything outside the mutated
// span formats back byte-identical.
func mutantSource(src []byte, p mutationPoint) ([]byte, error) {
	mut := make([]byte, 0, len(src)+len(p.repl))
	mut = append(mut, src[:p.start]...)
	mut = append(mut, p.repl...)
	mut = append(mut, src[p.end:]...)
	return format.Source(mut)
}

// fileMode remembers the target's permissions so restore recreates the file
// exactly as it was.
func fileMode(path string) (os.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Mode(), nil
}

// printSummary writes the human totals line and the machine-readable JSON
// line; the JSON is guaranteed to be the last line the harness prints. HTML
// escaping is off so descriptions keep their literal "x -> y" arrows.
func printSummary(res summary) error {
	res.Total = res.Killed + res.Broken + len(res.Survived)
	fmt.Printf("summary: total %d, killed %d, broken %d, survived %d\n",
		res.Total, res.Killed, res.Broken, len(res.Survived))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	fmt.Print(buf.String()) // Encode already appends the trailing newline
	return nil
}
