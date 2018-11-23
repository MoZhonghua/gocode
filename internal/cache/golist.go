package cache

/*
For cache importer, it is important to evit cached package if and only
if the package is outdated.

For source importer:
	hash all files' timestamp(or content) and re-import if any file
	changed since last import

For .a/.so importer:
	(1) .a/.so timestamp has changed, or
	(2) Stale(reported by go list) is ture

'go list' command provides many useful information:
	(1) files of pacakges with consideration of GOOS/GOARCH/tags
	(2) can report whether prebuild lib is stale
	(3) other information provided by 'go/build' package
*/

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// Copied from output of `go help list`
// Only keep fields we'are interested
type Package struct {
	Dir         string // directory containing package sources
	ImportPath  string // import path of package in dir
	Name        string // package name
	Target      string // install path
	Goroot      bool   // is this package in the Go root?
	Stale       bool   // would 'go install' do anything for this package?
	StaleReason string // explanation for Stale==true
	Root        string // Go root or Go path dir containing this package

	// Source files
	GoFiles         []string // .go source files (excluding CgoFiles, TestGoFiles, XTestGoFiles)
	CgoFiles        []string // .go source files that import "C"
	CompiledGoFiles []string // .go files presented to compiler (when using -compiled)
	IgnoredGoFiles  []string // .go source files ignored due to build constraints
	CFiles          []string // .c source files
	CXXFiles        []string // .cc, .cxx and .cpp source files
	MFiles          []string // .m source files
	HFiles          []string // .h, .hh, .hpp and .hxx source files
	FFiles          []string // .f, .F, .for and .f90 Fortran source files
	SFiles          []string // .s source files
	SwigFiles       []string // .swig files
	SwigCXXFiles    []string // .swigcxx files
	SysoFiles       []string // .syso object files to add to archive
	TestGoFiles     []string // _test.go files in package
	XTestGoFiles    []string // _test.go files outside package
}

func RunGoList(ctx *PackedContext, path string) (*Package, error) {
	cmd := exec.Command("go", "list", "-json", path)
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOROOT=%s", ctx.GOROOT))
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOPATH=%s", ctx.GOPATH))
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOARCH=%s", ctx.GOARCH))
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOOS=%s", ctx.GOOS))

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	pkg := new(Package)
	if err := json.Unmarshal(output, pkg); err != nil {
		return nil, err
	}

	return pkg, nil
}
