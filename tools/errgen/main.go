// Command errgen generates the Go constants and TS types of the stable error
// code registry from its YAML sources.
//
// The output is deterministic (sorted by code) so a drift check can compare a
// fresh run against what is committed.
package main

import (
	"errors"
	"fmt"
	"go/format"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type def struct {
	Code   string `yaml:"code"`
	Status int    `yaml:"status"`
	Title  string `yaml:"title"`
	ZH     string `yaml:"zh"`
}

// output is where one segment compiles to. It is declared by the segment
// itself rather than by a table in this file, because a build that does not
// ship a segment does not ship the package that segment compiles into either
// — and a table here would name a package that such a build has no directory
// for.
type output struct {
	GoDir     string `yaml:"go_dir"`
	GoPackage string `yaml:"go_package"`
	// RegistersInto is the import path of the base segment's package. A
	// segment that sets it reuses that package's Def type and hands its own
	// definitions to it at init time; a segment that leaves it empty is the
	// base segment and declares Def itself.
	RegistersInto string `yaml:"registers_into"`
}

type registry struct {
	Output output `yaml:"output"`
	Codes  []def  `yaml:"codes"`
}

// A code has an owning namespace and a snake_case name. The generator is
// intentionally namespace-agnostic: an integrating module declares its own
// segment without teaching this public tool about private product names.
var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "errgen:", err)
		os.Exit(1)
	}
}

// specs lists the segments of the error code registry, in the order they are
// merged. Splitting the registry by file keeps each build's codes together,
// and each file names the package it compiles into, so a build that has no
// reason to emit a segment's codes does not compile them in either.
var specs = []string{"api/errors-core.yaml"}

// tsFile is the one TypeScript output. The browser client is assembled per
// deployment rather than per layer, so the codes stay in a single module and
// a build that ships fewer segments simply gets a shorter file.
const tsFile = "web/packages/api-client/src/errors.gen.ts"

func configuredSpecs() []string {
	if raw := os.Getenv("FAIRLB_ERRGEN_SPECS"); raw != "" {
		return strings.Split(raw, ",")
	}
	return specs
}

func run() error {
	var merged []def
	var read []string
	var parts []registry
	seen := map[string]string{}
	dirs := map[string]string{}
	for _, spec := range configuredSpecs() {
		raw, err := os.ReadFile(spec)
		if errors.Is(err, os.ErrNotExist) {
			// Not every build ships every segment, so a missing file is that
			// build's normal shape rather than an error. This tolerance cannot
			// decay into a silent no-op: the zero-code guard below still fires
			// if no segment at all could be read.
			continue
		}
		if err != nil {
			return err
		}
		var part registry
		if err := yaml.Unmarshal(raw, &part); err != nil {
			return fmt.Errorf("parsing %s: %w", spec, err)
		}
		if part.Output.GoDir == "" || part.Output.GoPackage == "" {
			return fmt.Errorf("%s declares no output package: a segment that does not say where it compiles to would be parsed and then silently dropped", spec)
		}
		// Two segments writing the same directory would leave whichever ran
		// last, and the loss looks exactly like a segment that was never read.
		if prev, clash := dirs[part.Output.GoDir]; clash {
			return fmt.Errorf("%s and %s both generate into %s: the second run would overwrite the first",
				prev, spec, part.Output.GoDir)
		}
		dirs[part.Output.GoDir] = spec
		// A code defined twice across segments only becomes visible while
		// merging, and its consequence is one code with two meanings. The
		// duplicate check in validate runs on the merged list and cannot say
		// which files collided, so the collision is caught here instead.
		for _, d := range part.Codes {
			if prev, dup := seen[d.Code]; dup {
				return fmt.Errorf("%s and %s both define %s: an error code is part of the public contract and must not be duplicated across files",
					prev, spec, d.Code)
			}
			seen[d.Code] = spec
		}
		merged = append(merged, part.Codes...)
		parts = append(parts, part)
		read = append(read, spec)
	}
	if len(merged) == 0 {
		return errors.New("no codes parsed from any registry file: that is a broken run, not an empty registry")
	}
	if err := validate(merged); err != nil {
		return err
	}

	for i, part := range parts {
		codes := slices.Clone(part.Codes)
		sortCodes(codes)
		dst := filepath.Join(filepath.FromSlash(part.Output.GoDir), "errcode.gen.go")
		if err := writeFile(dst, genGo(codes, read[i], part.Output)); err != nil {
			return err
		}
	}

	if os.Getenv("FAIRLB_ERRGEN_SKIP_TS") == "1" {
		return nil
	}
	sortCodes(merged)
	return writeFile(filepath.FromSlash(tsFile), []byte(genTS(merged, read)))
}

func sortCodes(codes []def) {
	slices.SortFunc(codes, func(a, b def) int { return strings.Compare(a.Code, b.Code) })
}

func validate(codes []def) error {
	seen := map[string]bool{}
	for _, d := range codes {
		if !codeRe.MatchString(d.Code) {
			return fmt.Errorf("code %q does not match namespace.snake_case", d.Code)
		}
		if seen[d.Code] {
			return fmt.Errorf("code %q is defined twice", d.Code)
		}
		seen[d.Code] = true
		if d.Status < 400 || d.Status > 599 {
			return fmt.Errorf("code %q has status %d, outside 400-599", d.Code, d.Status)
		}
		if d.Title == "" || d.ZH == "" {
			return fmt.Errorf("code %q is missing title or zh", d.Code)
		}
	}
	return nil
}

// constName turns a dotted code such as common.not_found into the exported Go
// identifier CommonNotFound.
func constName(code string) string {
	var b strings.Builder
	for part := range strings.SplitSeq(strings.ReplaceAll(code, ".", "_"), "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// header names the segments this run actually read. Naming them — rather than
// a fixed string — is what keeps the line true in every build: a build that
// ships only some segments gets a header listing only the files it has, and a
// segment that is renamed or split cannot leave a generated file pointing at
// something that no longer exists.
func header(read ...string) string {
	return "// Code generated by tools/errgen from " + strings.Join(read, ", ") + ". DO NOT EDIT.\n"
}

func genGo(codes []def, spec string, out output) []byte {
	var b strings.Builder
	b.WriteString(header(spec))

	// The base segment declares Def; the others reuse it, so that one code
	// looked up through the base registry answers the same whichever segment
	// defined it.
	defType := "Def"
	if out.RegistersInto != "" {
		base := path.Base(out.RegistersInto)
		defType = base + ".Def"
		fmt.Fprintf(&b, `
// Package %s holds the error codes of a segment of the registry that only
// some builds ship. Its definitions are handed to the base registry at init
// time, so a lookup by code string finds them without the lookup having to
// know which segment a code came from.
package %s

import %q
`, out.GoPackage, out.GoPackage, out.RegistersInto)
	} else {
		fmt.Fprintf(&b, `
// Package %s is the registry of stable error codes.
package %s

// Def describes one error code: the problem+json code plus its default
// status and title.
type Def struct {
	Code    string
	Status  int
	Title   string
	TitleZH string
}
`, out.GoPackage, out.GoPackage)
	}

	b.WriteString("\nconst (\n")
	for _, d := range codes {
		fmt.Fprintf(&b, "\t%s = %q\n", constName(d.Code), d.Code)
	}
	fmt.Fprintf(&b, ")\n\n// registry indexes every definition by code. Unexported on purpose: an error\n"+
		"// code is part of the public contract, and a package-level map anyone can\n"+
		"// assign into makes \"only the generator writes this\" unenforceable. Read it\n"+
		"// through Lookup/All (ADR-0161).\nvar registry = map[string]%s{\n", defType)
	for _, d := range codes {
		fmt.Fprintf(&b, "\t%s: {Code: %s, Status: %d, Title: %q, TitleZH: %q},\n",
			constName(d.Code), constName(d.Code), d.Status, d.Title, d.ZH)
	}
	b.WriteString("}\n")
	fmt.Fprintf(&b, "\n// All returns a copy of this segment's definitions. A copy, so that walking\n"+
		"// the contract cannot become writing to it.\nfunc All() map[string]%s {\n"+
		"\tout := make(map[string]%s, len(registry))\n"+
		"\tfor code, d := range registry {\n\t\tout[code] = d\n\t}\n\treturn out\n}\n", defType, defType)

	if out.RegistersInto != "" {
		// Registration happens here rather than at an assembly point because a
		// build that compiles these codes in is exactly a build that can emit
		// them: tying the two together removes the state where the codes exist
		// but every response carrying one renders as an internal error.
		fmt.Fprintf(&b, "\nfunc init() { %s.Register(registry) }\n", path.Base(out.RegistersInto))
	}

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		panic(fmt.Sprintf("errgen: generated Go source does not format: %v", err))
	}
	return src
}

func genTS(codes []def, read []string) string {
	var b strings.Builder
	b.WriteString(header(read...))
	b.WriteString(`
export const ErrorCodes = {
`)
	for _, d := range codes {
		fmt.Fprintf(&b, "  %s: %q,\n", constName(d.Code), d.Code)
	}
	b.WriteString(`} as const;

export type ErrorCode = (typeof ErrorCodes)[keyof typeof ErrorCodes];

export interface ErrorCodeDef {
  status: number;
  title: string;
  titleZh: string;
}

export const errorCodeDefs: Record<ErrorCode, ErrorCodeDef> = {
`)
	for _, d := range codes {
		fmt.Fprintf(&b, "  %q: { status: %d, title: %q, titleZh: %q },\n", d.Code, d.Status, d.Title, d.ZH)
	}
	b.WriteString("};\n")
	return b.String()
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
