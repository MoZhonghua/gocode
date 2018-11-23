package cache

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"go/build"
	goimporter "go/importer"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/gcexportdata"
)

// We need to mangle go/build.Default to make gcimporter work as
// intended, so use a lock to protect against concurrent accesses.
var buildDefaultLock sync.Mutex

// Mu must be held while using the cache importer.
var Mu sync.Mutex

var importCache = importerCache{
	fset:    token.NewFileSet(),
	imports: make(map[string]importCacheEntry),
}

func NewImporter(ctx *PackedContext, filename string, fallbackToSource bool, logger func(string, ...interface{})) types.ImporterFrom {
	importCache.clean()

	imp := &importer{
		ctx:              ctx,
		importerCache:    &importCache,
		fallbackToSource: fallbackToSource,
		logf:             logger,
	}

	slashed := filepath.ToSlash(filename)
	i := strings.LastIndex(slashed, "/vendor/src/")
	if i < 0 {
		i = strings.LastIndex(slashed, "/src/")
	}
	if i > 0 {
		paths := filepath.SplitList(imp.ctx.GOPATH)

		gbroot := filepath.FromSlash(slashed[:i])
		gbvendor := filepath.Join(gbroot, "vendor")
		if SamePath(gbroot, imp.ctx.GOROOT) {
			goto Found
		}
		for _, path := range paths {
			if SamePath(path, gbroot) || SamePath(path, gbvendor) {
				goto Found
			}
		}

		imp.gbroot = gbroot
		imp.gbvendor = gbvendor
	Found:
	}

	return imp
}

type importer struct {
	*importerCache
	gbroot, gbvendor string
	ctx              *PackedContext
	fallbackToSource bool
	logf             func(string, ...interface{})
}

type importerCache struct {
	fset    *token.FileSet
	imports map[string]importCacheEntry
}

type importCacheEntry struct {
	pkg    *types.Package
	mtime  time.Time
	digest string
}

func (i *importer) Import(importPath string) (*types.Package, error) {
	return i.ImportFrom(importPath, "", 0)
}

func (i *importer) ImportFrom(importPath, srcDir string, mode types.ImportMode) (*types.Package, error) {
	buildDefaultLock.Lock()
	defer buildDefaultLock.Unlock()

	origDef := build.Default
	defer func() { build.Default = origDef }()

	def := &build.Default
	// The gb root of a project can be used as a $GOPATH because it contains pkg/.
	def.GOPATH = i.ctx.GOPATH
	if i.gbroot != "" {
		def.GOPATH = i.gbroot
	}
	def.GOARCH = i.ctx.GOARCH
	def.GOOS = i.ctx.GOOS
	def.GOROOT = i.ctx.GOROOT
	def.CgoEnabled = i.ctx.CgoEnabled
	def.UseAllFiles = i.ctx.UseAllFiles
	def.Compiler = i.ctx.Compiler
	def.BuildTags = i.ctx.BuildTags
	def.ReleaseTags = i.ctx.ReleaseTags
	def.InstallSuffix = i.ctx.InstallSuffix
	def.SplitPathList = i.splitPathList
	def.JoinPath = i.joinPath

	i.logf("importing: %v, srcdir: %v", importPath, srcDir)
	filename, path := gcexportdata.Find(importPath, srcDir)

	entry, ok := i.imports[path]
	if filename == "" {
		i.logf("no gcexportdata file for %s", path)
		// If there is no export data, check the cache.

		var err error
		// Digest package source files' timestamp, evit it if changed
		goListPkg, err := RunGoList(i.ctx, path)
		if err != nil {
			i.logf("failed to run go list on package: %s: %v", path, err)
			return nil, err
		}

		digest, err := digestPackageFilesTimestamp(goListPkg)
		if err != nil {
			i.logf("failed to digest package source files: %s: %v", path, err)
			return nil, err
		}

		if ok {
			if entry.digest == digest {
				i.logf("use cache for package: %s", path)
				return entry.pkg, nil
			} else {
				i.logf("package source files changed, evit cache for package: %s", path)
				delete(i.imports, path)
			}
		}

		// If there is no cache entry or cache entry is outdated and the
		// user has configured the correct setting, import and cache
		// using the source importer.
		var pkg *types.Package
		if i.fallbackToSource {
			i.logf("cache: falling back to the source importer for %s", path)
			pkg, err = goimporter.For("source", nil).Import(path)
		} else {
			i.logf("cache: falling back to the source default for %s", path)
			pkg, err = goimporter.Default().Import(path)
		}
		if pkg == nil {
			i.logf("failed to fall back to another importer for %s: %v", pkg, err)
			return nil, err
		}
		entry = importCacheEntry{pkg, time.Now(), digest}
		i.imports[path] = entry
		return entry.pkg, nil
	}

	// If there is export data for the package.
	// TODO: maybe fallback to source importer if 'go list' report this
	// package is stale
	fi, err := os.Stat(filename)
	if err != nil {
		i.logf("could not stat %s", filename)
		return nil, err
	}
	if entry.mtime != fi.ModTime() {
		f, err := os.Open(filename)
		if err != nil {
			return nil, err
		}
		in, err := gcexportdata.NewReader(f)
		if err != nil {
			return nil, err
		}
		pkg, err := gcexportdata.Read(in, i.fset, make(map[string]*types.Package), path)
		if err != nil {
			return nil, err
		}
		entry = importCacheEntry{pkg, fi.ModTime(), ""}
		i.imports[path] = entry
	}

	return entry.pkg, nil
}

// Delete random files to keep the cache at most 100 entries.
// Only call while holding the importer's mutex.
func (i *importerCache) clean() {
	for k := range i.imports {
		if len(i.imports) <= 100 {
			break
		}
		delete(i.imports, k)
	}
}

func (i *importer) splitPathList(list string) []string {
	res := filepath.SplitList(list)
	if i.gbroot != "" {
		res = append(res, i.gbroot, i.gbvendor)
	}
	return res
}

func (i *importer) joinPath(elem ...string) string {
	res := filepath.Join(elem...)

	if i.gbroot != "" {
		// Want to rewrite "$GBROOT/(vendor/)?pkg/$GOOS_$GOARCH(_)?"
		// into "$GBROOT/pkg/$GOOS-$GOARCH(-)?".
		// Note: gb doesn't use vendor/pkg.
		if gbrel, err := filepath.Rel(i.gbroot, res); err == nil {
			gbrel = filepath.ToSlash(gbrel)
			gbrel, _ = match(gbrel, "vendor/")
			if gbrel, ok := match(gbrel, fmt.Sprintf("pkg/%s_%s", i.ctx.GOOS, i.ctx.GOARCH)); ok {
				gbrel, hasSuffix := match(gbrel, "_")

				// Reassemble into result.
				if hasSuffix {
					gbrel = "-" + gbrel
				}
				gbrel = fmt.Sprintf("pkg/%s-%s/", i.ctx.GOOS, i.ctx.GOARCH) + gbrel
				gbrel = filepath.FromSlash(gbrel)
				res = filepath.Join(i.gbroot, gbrel)
			}
		}
	}
	return res
}

func match(s, prefix string) (string, bool) {
	rest := strings.TrimPrefix(s, prefix)
	return rest, len(rest) < len(s)
}

func digestPackageFilesTimestamp(pkg *Package) (string, error) {
	h := md5.New()
	filesList := [][]string{
		pkg.GoFiles,
		pkg.CgoFiles,
		pkg.CompiledGoFiles,
		pkg.IgnoredGoFiles,
		pkg.CFiles,
		pkg.CXXFiles,
		pkg.MFiles,
		pkg.HFiles,
		pkg.FFiles,
		pkg.SFiles,
		pkg.SwigFiles,
		pkg.SwigCXXFiles,
		pkg.SysoFiles,
		pkg.TestGoFiles,
		pkg.XTestGoFiles,
	}

	srcDir := path.Join(pkg.Root, "src", pkg.ImportPath)
	buf := make([]byte, 8)
	for _, files := range filesList {
		sort.Strings(files)
		for _, f := range files {
			h.Write([]byte(f))
			ts, err := fileTimestamp(path.Join(srcDir, f))
			if err != nil {
				return "", err
			}

			binary.LittleEndian.PutUint64(buf, ts)
			h.Write(buf)
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileTimestamp(f string) (uint64, error) {
	fi, err := os.Stat(f)
	if err != nil {
		return 0, err
	}

	return uint64(fi.ModTime().UnixNano()), nil
}
