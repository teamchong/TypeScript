package compiler

import (
	"runtime"
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/binder"
	"github.com/microsoft/TypeScript/tsc/internal/checker"
	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/diagnostics"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/printer"
	"github.com/microsoft/TypeScript/tsc/internal/tsoptions"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/microsoft/TypeScript/tsc/internal/vfs"
	"github.com/microsoft/TypeScript/tsc/internal/vfs/cachedvfs"
)

type wasmCachedDts struct {
	size  int64
	mtime int64
	file  *ast.SourceFile
}

var (
	wasmStaticDtsCache map[ast.SourceFileParseOptions]wasmCachedDts
	wasmHeapMarked     bool
	wasmMarkHeapHook   func()
	wasmResetHeapHook  func()
)

func SetWasmHeapHooks(markFn, resetFn func()) {
	wasmMarkHeapHook = markFn
	wasmResetHeapHook = resetFn
}

func isStaticWasmDts(fileName tspath.RootedFilePath) bool {
	return !strings.Contains(fileName.AsString(), "state")
}

func resetAllWasmPools() {
	ast.ResetForcedLazyLists()
	for _, entry := range wasmStaticDtsCache {
		if entry.file != nil {
			entry.file.ResetWasmCaches()
		}
	}
	core.ResetWasmPools()
	parser.ResetWasmPools()
	binder.ResetWasmPools()
	printer.ResetWasmPools()
	checker.ResetWasmPools()
	resetParseTaskDataPool()
}

func FinishWasmProgramInit(program *Program) {
	if runtime.GOARCH != "wasm" || wasmMarkHeapHook == nil {
		return
	}
	if !wasmHeapMarked {
		program.BindSourceFiles()
		core.NodeCoreModules()
		resetAllWasmPools()
		wasmMarkHeapHook()
		ast.SetTrackForcedLazy(true)
		wasmHeapMarked = true
	}
}

func ResetWasmHeapToWatermark() {
	if !wasmHeapMarked || wasmResetHeapHook == nil {
		return
	}
	resetAllWasmPools()
	wasmResetHeapHook()
}

type CompilerHost interface {
	FS() vfs.FS
	DefaultLibraryPath() tspath.RootedDirectoryPath
	Trace(msg *diagnostics.Message, args ...any)
	GetSourceFile(opts ast.SourceFileParseOptions) *ast.SourceFile
	// GetContentMappedSourceFile produces the source file for a content-mapped (foreign) file by running
	// the given mapper's transform on the file's content. The caller resolves the mapper (and owns the
	// failure accounting), so implementations must use it as-is. It returns nil if the file cannot be read,
	// or an error if the transform fails or the mapper produces invalid position mappings. Implementations
	// may cache successful results.
	GetContentMappedSourceFiles(parseOptions ast.SourceFileParseOptions, mapper *contentmapper.Mapper) (contentmapper.SourceFiles, error)
	// ContentMapperProject returns the project-scoped content mapper used by this host, or nil when the
	// command line has no content mappers. The project owns transform identity and lifecycle state.
	ContentMapperProject() contentmapper.Project
	GetResolvedProjectReference(fileName tspath.RootedFilePath, path tspath.PathKey) *tsoptions.ParsedCommandLine
}

var _ CompilerHost = (*compilerHost)(nil)

type compilerHost struct {
	fs                   vfs.FS
	defaultLibraryPath   tspath.RootedDirectoryPath
	extendedConfigCache  tsoptions.ExtendedConfigCache
	trace                func(msg *diagnostics.Message, args ...any)
	contentMapperProject contentmapper.Project
}

func NewCachedFSCompilerHost(
	fs vfs.FS,
	defaultLibraryPath tspath.RootedDirectoryPath,
	extendedConfigCache tsoptions.ExtendedConfigCache,
	trace func(msg *diagnostics.Message, args ...any),
	contentMapperProject contentmapper.Project,
) CompilerHost {
	return NewCompilerHost(cachedvfs.From(fs), defaultLibraryPath, extendedConfigCache, trace, contentMapperProject)
}

func NewCompilerHost(
	fs vfs.FS,
	defaultLibraryPath tspath.RootedDirectoryPath,
	extendedConfigCache tsoptions.ExtendedConfigCache,
	trace func(msg *diagnostics.Message, args ...any),
	contentMapperProject contentmapper.Project,
) CompilerHost {
	if trace == nil {
		trace = func(msg *diagnostics.Message, args ...any) {}
	}
	return &compilerHost{
		fs:                   fs,
		defaultLibraryPath:   defaultLibraryPath,
		extendedConfigCache:  extendedConfigCache,
		trace:                trace,
		contentMapperProject: contentMapperProject,
	}
}

func (h *compilerHost) FS() vfs.FS {
	return h.fs
}

func (h *compilerHost) DefaultLibraryPath() tspath.RootedDirectoryPath {
	return h.defaultLibraryPath
}

func (h *compilerHost) Trace(msg *diagnostics.Message, args ...any) {
	h.trace(msg, args...)
}

func (h *compilerHost) GetSourceFile(opts ast.SourceFileParseOptions) *ast.SourceFile {
	if runtime.GOARCH == "wasm" && wasmMarkHeapHook != nil && opts.FileName.IsDeclarationFile() && isStaticWasmDts(opts.FileName) {
		if wasmStaticDtsCache != nil {
			if entry, ok := wasmStaticDtsCache[opts]; ok {
				if fi := h.FS().Stat(opts.FileName.AsPath()); fi != nil && fi.Size() == entry.size && fi.ModTime().UnixNano() == entry.mtime {
					return entry.file
				}
			}
		}
		if !wasmHeapMarked {
			text, ok := h.FS().ReadFile(opts.FileName)
			if !ok {
				return nil
			}
			sf := parser.ParseSourceFile(opts, text, core.EnsureScriptKindFromFileName(opts.FileName))
			binder.BindSourceFile(sf)
			if wasmStaticDtsCache == nil {
				wasmStaticDtsCache = make(map[ast.SourceFileParseOptions]wasmCachedDts, 64)
			}
			var size, mtime int64
			if fi := h.FS().Stat(opts.FileName.AsPath()); fi != nil {
				size = fi.Size()
				mtime = fi.ModTime().UnixNano()
			} else {
				size = int64(len(text))
			}
			wasmStaticDtsCache[opts] = wasmCachedDts{
				size:  size,
				mtime: mtime,
				file:  sf,
			}
			return sf
		}
	}
	text, ok := h.FS().ReadFile(opts.FileName)
	if !ok {
		return nil
	}
	return parser.ParseSourceFile(opts, text, core.EnsureScriptKindFromFileName(opts.FileName))
}

func (h *compilerHost) GetContentMappedSourceFiles(parseOptions ast.SourceFileParseOptions, mapper *contentmapper.Mapper) (contentmapper.SourceFiles, error) {
	if h.contentMapperProject == nil {
		return contentmapper.SourceFiles{}, contentmapper.ErrProjectUnavailable
	}
	content, ok := h.FS().ReadFile(parseOptions.FileName)
	if !ok {
		return contentmapper.SourceFiles{}, nil
	}
	files, err := contentmapper.TransformAndParse(parseOptions, content, mapper, h.contentMapperProject)
	if err == nil {
		err = contentmapper.CheckSupplementalFileNameCollisions(files, h.FS().FileExists)
	}
	return files, err
}

func (h *compilerHost) ContentMapperProject() contentmapper.Project {
	return h.contentMapperProject
}

func (h *compilerHost) GetResolvedProjectReference(fileName tspath.RootedFilePath, path tspath.PathKey) *tsoptions.ParsedCommandLine {
	commandLine, _ := tsoptions.GetParsedCommandLineOfConfigFilePath(fileName, path, nil, nil /*optionsRaw*/, tsoptions.NewDefaultParseConfigHost(h.fs), h.extendedConfigCache)
	return commandLine
}
