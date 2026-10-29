//go:build wasip1

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/diagnostics"
	"github.com/microsoft/TypeScript/tsc/internal/diagnosticwriter"
	"github.com/microsoft/TypeScript/tsc/internal/execute"
	"github.com/microsoft/TypeScript/tsc/internal/json"
	"github.com/microsoft/TypeScript/tsc/internal/locale"
	"github.com/microsoft/TypeScript/tsc/internal/scanner"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/microsoft/TypeScript/tsc/internal/vfs"
)

const (
	opRead     uint32 = 0
	opStat     uint32 = 1
	opReadDir  uint32 = 2
	opRealpath uint32 = 3
	opWrite    uint32 = 4
	opAppend   uint32 = 5
	opRemove   uint32 = 6
	opChtimes  uint32 = 7
)

//go:wasmimport tsrs_host fs
func hostFS(op uint32, ptr unsafe.Pointer, length uint32) int32

//go:wasmimport tsrs_host fs_take
func hostFSTake(ptr unsafe.Pointer)

//go:linkname runtimeMarkHeap runtime.MarkHeap
func runtimeMarkHeap()

//go:linkname runtimeResetHeap runtime.ResetHeap
func runtimeResetHeap()

const (
	wasmInitialInputCap  = 4 << 20
	wasmInitialOutputCap = 8 << 20
)

var (
	wasmInputBuf           []byte
	wasmOutputBuf          []byte
	wasmDummyByte          byte
	wasmBuffersInitialized bool
)

func init() {
	compiler.SetWasmHeapHooks(func() {
		_, _ = json.Marshal([]wasmDiagnosticResponse{{
			StartPosition:      &wasmDiagnosticPosition{},
			EndPosition:        &wasmDiagnosticPosition{},
			MessageChain:       []wasmDiagnosticResponse{{}},
			RelatedInformation: []wasmDiagnosticResponse{{}},
		}})
		runtimeMarkHeap()
	}, runtimeResetHeap)
}

func ensureWasmBuffers() {
	if !wasmBuffersInitialized {
		wasmInputBuf = make([]byte, 0, wasmInitialInputCap)
		wasmOutputBuf = make([]byte, 0, wasmInitialOutputCap)
		wasmBuffersInitialized = true
	}
}

//go:wasmexport tsrs_input
func tsrsInput(length uint32) unsafe.Pointer {
	ensureWasmBuffers()
	if int(length) <= cap(wasmInputBuf) {
		wasmInputBuf = wasmInputBuf[:length]
	} else {
		wasmInputBuf = make([]byte, max(int(length), 1))[:length]
	}
	if length == 0 {
		return unsafe.Pointer(&wasmDummyByte)
	}
	return unsafe.Pointer(unsafe.SliceData(wasmInputBuf))
}

//go:wasmexport tsrs_run
func tsrsRun() int32 {
	ensureWasmBuffers()
	req := string(wasmInputBuf)
	wasmInputBuf = wasmInputBuf[:0]
	wasmOutputBuf = wasmOutputBuf[:0]

	parts := strings.Split(req, "\x00")
	cwd := "/"
	if len(parts) > 0 && parts[0] != "" {
		cwd = parts[0]
	}
	var flags uint32
	if len(parts) > 1 {
		if v, err := strconv.ParseUint(parts[1], 10, 32); err == nil {
			flags = uint32(v)
		}
	}
	var args []string
	if len(parts) > 2 {
		args = parts[2:]
	}

	jsonDiagnostics := (flags & 1) != 0
	caseSensitive := (flags & 2) == 0
	tty := (flags & 4) != 0
	var writer io.Writer = os.Stdout
	if jsonDiagnostics {
		writer = io.Discard
	}
	sys := &wasmSys{
		cwd:                tspath.RootedDirectoryPathFromAbsolute(tspath.NormalizePath(cwd)),
		fs:                 bundled.WrapFS(&wasmHostFS{caseSensitive: caseSensitive}),
		defaultLibraryPath: bundled.LibPath(),
		writer:             writer,
		tty:                tty,
		start:              time.Now(),
	}
	result := execute.CommandLine(context.Background(), sys, args, nil)
	if jsonDiagnostics {
		responses := make([]wasmDiagnosticResponse, len(result.Diagnostics))
		for i, d := range result.Diagnostics {
			responses[i] = newWasmDiagnosticResponse(diagnosticwriter.WrapASTDiagnostic(d))
		}
		if encoded, err := json.Marshal(responses); err == nil {
			if len(encoded) <= cap(wasmOutputBuf) {
				wasmOutputBuf = wasmOutputBuf[:len(encoded)]
				copy(wasmOutputBuf, encoded)
			} else {
				wasmOutputBuf = encoded
			}
		}
	}
	compiler.ResetWasmHeapToWatermark()
	return int32(result.Status)
}

type wasmDiagnosticPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type wasmDiagnosticResponse struct {
	FileName           string                   `json:"fileName,omitempty"`
	Pos                int                      `json:"pos"`
	End                int                      `json:"end"`
	StartPosition      *wasmDiagnosticPosition  `json:"startPosition,omitempty"`
	EndPosition        *wasmDiagnosticPosition  `json:"endPosition,omitempty"`
	Code               int32                    `json:"code"`
	Category           diagnostics.Category     `json:"category"`
	Source             string                   `json:"source,omitempty"`
	Text               string                   `json:"text"`
	ReportsUnnecessary bool                     `json:"reportsUnnecessary,omitempty"`
	ReportsDeprecated  bool                     `json:"reportsDeprecated,omitempty"`
	MessageChain       []wasmDiagnosticResponse `json:"messageChain,omitempty"`
	RelatedInformation []wasmDiagnosticResponse `json:"relatedInformation,omitempty"`
}

func newWasmDiagnosticResponse(d *diagnosticwriter.ASTDiagnostic) wasmDiagnosticResponse {
	file := d.File()
	pos, end := d.Pos(), d.End()
	if file != nil {
		pos = max(0, min(pos, len(file.Text())))
		end = max(pos, min(end, len(file.Text())))
	}
	resp := wasmDiagnosticResponse{
		Pos:                pos,
		End:                end,
		Code:               d.Code(),
		Category:           d.Category(),
		Source:             d.Source(),
		Text:               d.Localize(locale.Default),
		ReportsUnnecessary: d.Diagnostic.ReportsUnnecessary(),
		ReportsDeprecated:  d.Diagnostic.ReportsDeprecated(),
	}
	if file != nil {
		resp.FileName = file.FileName().AsString()
		if sourceFile, ok := file.(*ast.SourceFile); ok {
			positionMap := sourceFile.GetPositionMap()
			resp.Pos = positionMap.UTF8ToUTF16(pos)
			resp.End = positionMap.UTF8ToUTF16(end)
		} else {
			resp.Pos = int(core.UTF16Len(file.Text()[:pos]))
			resp.End = int(core.UTF16Len(file.Text()[:end]))
		}
		startLine, startChar := scanner.GetECMALineAndUTF16CharacterOfPosition(file, pos)
		endLine, endChar := scanner.GetECMALineAndUTF16CharacterOfPosition(file, end)
		resp.StartPosition = &wasmDiagnosticPosition{Line: startLine, Character: int(startChar)}
		resp.EndPosition = &wasmDiagnosticPosition{Line: endLine, Character: int(endChar)}
	}
	if chain := d.MessageChain(); len(chain) > 0 {
		resp.MessageChain = make([]wasmDiagnosticResponse, len(chain))
		for i, c := range chain {
			resp.MessageChain[i] = newWasmDiagnosticResponse(c.(*diagnosticwriter.ASTDiagnostic))
		}
	}
	if related := d.RelatedInformation(); len(related) > 0 {
		resp.RelatedInformation = make([]wasmDiagnosticResponse, len(related))
		for i, r := range related {
			resp.RelatedInformation[i] = newWasmDiagnosticResponse(r.(*diagnosticwriter.ASTDiagnostic))
		}
	}
	return resp
}

//go:wasmexport tsrs_output
func tsrsOutput() unsafe.Pointer {
	if len(wasmOutputBuf) == 0 {
		return unsafe.Pointer(&wasmDummyByte)
	}
	return unsafe.Pointer(unsafe.SliceData(wasmOutputBuf))
}

//go:wasmexport tsrs_output_len
func tsrsOutputLen() uint32 {
	return uint32(len(wasmOutputBuf))
}

func hostCall(op uint32, arg string) ([]byte, error) {
	var ptr unsafe.Pointer
	if len(arg) > 0 {
		ptr = unsafe.Pointer(unsafe.StringData(arg))
	} else {
		ptr = unsafe.Pointer(&wasmDummyByte)
	}
	n := hostFS(op, ptr, uint32(len(arg)))
	if n == 0 {
		return nil, nil
	}
	if n > 0 {
		buf := make([]byte, n)
		hostFSTake(unsafe.Pointer(unsafe.SliceData(buf)))
		return buf, nil
	}
	if n == -1 {
		return nil, vfs.ErrNotExist
	}
	errLen := -2 - n
	if errLen <= 0 {
		return nil, errors.New("io error")
	}
	errBuf := make([]byte, errLen)
	hostFSTake(unsafe.Pointer(unsafe.SliceData(errBuf)))
	return nil, errors.New(string(errBuf))
}

type wasmFileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (fi *wasmFileInfo) Name() string       { return fi.name }
func (fi *wasmFileInfo) Size() int64        { return fi.size }
func (fi *wasmFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *wasmFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *wasmFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *wasmFileInfo) Sys() any           { return nil }

type wasmHostFS struct {
	caseSensitive bool
}

func (w *wasmHostFS) CaseSensitivity() tspath.CaseSensitivity {
	if w.caseSensitive {
		return tspath.CaseSensitive
	}
	return tspath.CaseInsensitive
}

func (w *wasmHostFS) Stat(path tspath.RootedPath) vfs.FileInfo {
	p := path.AsString()
	out, err := hostCall(opStat, p)
	if err != nil || len(out) == 0 {
		return nil
	}
	text := unsafe.String(unsafe.SliceData(out), len(out))
	s1 := strings.IndexByte(text, ' ')
	if s1 < 0 {
		return nil
	}
	kind := text[:s1]
	rest := text[s1+1:]
	s2 := strings.IndexByte(rest, ' ')
	var sizeStr, mtimeStr string
	if s2 >= 0 {
		sizeStr = rest[:s2]
		mtimeStr = rest[s2+1:]
	} else {
		sizeStr = rest
	}
	var mode fs.FileMode
	switch kind {
	case "d":
		mode = fs.ModeDir | 0o755
	case "f":
		mode = 0o644
	default:
		mode = fs.ModeIrregular
	}
	size, _ := strconv.ParseInt(sizeStr, 10, 64)
	mtimeNs, _ := strconv.ParseInt(mtimeStr, 10, 64)
	trimmed := strings.TrimRight(p, "/")
	base := "/"
	if trimmed != "" {
		if idx := strings.LastIndexByte(trimmed, '/'); idx >= 0 {
			base = trimmed[idx+1:]
		} else {
			base = trimmed
		}
	}
	return &wasmFileInfo{
		name:    base,
		size:    size,
		mode:    mode,
		modTime: time.Unix(0, mtimeNs),
	}
}

func (w *wasmHostFS) FileExists(path tspath.RootedFilePath) bool {
	fi := w.Stat(path.AsPath())
	return fi != nil && !fi.IsDir()
}

func (w *wasmHostFS) DirectoryExists(path tspath.RootedDirectoryPath) bool {
	fi := w.Stat(path.AsPath())
	return fi != nil && fi.IsDir()
}

func (w *wasmHostFS) ReadFile(path tspath.RootedFilePath) (string, bool) {
	b, err := hostCall(opRead, path.AsString())
	if err != nil {
		return "", false
	}
	if len(b) == 0 {
		return "", true
	}
	if len(b) >= 2 {
		if b[0] == 0xFE && b[1] == 0xFF {
			return decodeUTF16(b[2:], binary.BigEndian), true
		}
		if b[0] == 0xFF && b[1] == 0xFE {
			return decodeUTF16(b[2:], binary.LittleEndian), true
		}
		if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
			b = b[3:]
			if len(b) == 0 {
				return "", true
			}
		}
	}
	return unsafe.String(unsafe.SliceData(b), len(b)), true
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	u16s := make([]uint16, len(b)/2)
	for i := range u16s {
		u16s[i] = order.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u16s))
}

func (w *wasmHostFS) GetAccessibleEntries(path tspath.RootedDirectoryPath) (result vfs.Entries) {
	result.Symlinks = map[string]struct{}{}
	out, err := hostCall(opReadDir, path.AsString())
	if err != nil || len(out) == 0 {
		return result
	}
	raw := unsafe.String(unsafe.SliceData(out), len(out))
	items := strings.Split(raw, "\x00")
	// Sort by entry name (after the 1-byte kind prefix) to match fs.ReadDir.
	valid := items[:0]
	for _, item := range items {
		if len(item) > 1 {
			valid = append(valid, item)
		}
	}
	slices.SortFunc(valid, func(a, b string) int {
		return strings.Compare(a[1:], b[1:])
	})
	for _, item := range valid {
		kind := item[0]
		name := item[1:]
		switch kind {
		case 'd':
			result.Directories = append(result.Directories, name)
		case 'f':
			result.Files = append(result.Files, name)
		case 'l':
			if stat := w.Stat(path.ResolveFile(name).AsPath()); stat != nil {
				if stat.IsDir() {
					result.Directories = append(result.Directories, name)
					result.Symlinks[name] = struct{}{}
				} else if stat.Mode().IsRegular() {
					result.Files = append(result.Files, name)
					result.Symlinks[name] = struct{}{}
				}
			}
		}
	}
	return result
}

func (w *wasmHostFS) Realpath(path tspath.RootedPath) tspath.RootedPath {
	out, err := hostCall(opRealpath, path.AsString())
	if err != nil || len(out) == 0 {
		return path
	}
	return tspath.RootedPathFromAbsolute(tspath.NormalizeSlashes(unsafe.String(unsafe.SliceData(out), len(out))))
}

func (w *wasmHostFS) WriteFile(path tspath.RootedFilePath, data string) error {
	_, err := hostCall(opWrite, path.AsString()+"\x00"+data)
	return err
}

func (w *wasmHostFS) AppendFile(path tspath.RootedFilePath, data string) error {
	_, err := hostCall(opAppend, path.AsString()+"\x00"+data)
	return err
}

func (w *wasmHostFS) Remove(path tspath.RootedPath) error {
	_, err := hostCall(opRemove, path.AsString())
	return err
}

func (w *wasmHostFS) Chtimes(path tspath.RootedPath, aTime time.Time, mTime time.Time) error {
	_, err := hostCall(opChtimes, fmt.Sprintf("%s\x00%d\x00%d", path.AsString(), aTime.UnixNano(), mTime.UnixNano()))
	return err
}

type wasmSys struct {
	fs                 vfs.FS
	defaultLibraryPath tspath.RootedDirectoryPath
	cwd                tspath.RootedDirectoryPath
	writer             io.Writer
	tty                bool
	start              time.Time
}

func (s *wasmSys) SinceStart() time.Duration                       { return time.Since(s.start) }
func (s *wasmSys) Now() time.Time                                  { return time.Now() }
func (s *wasmSys) FS() vfs.FS                                      { return s.fs }
func (s *wasmSys) DefaultLibraryPath() tspath.RootedDirectoryPath  { return s.defaultLibraryPath }
func (s *wasmSys) GetCurrentDirectory() tspath.RootedDirectoryPath { return s.cwd }
func (s *wasmSys) Writer() io.Writer                               { return s.writer }
func (s *wasmSys) ErrorWriter() io.Writer                          { return os.Stderr }
func (s *wasmSys) WriteOutputIsTTY() bool                          { return s.tty }
func (s *wasmSys) GetWidthOfTerminal() int                         { return 0 }
func (s *wasmSys) GetEnvironmentVariable(name string) (string, bool) {
	return os.LookupEnv(name)
}

func (s *wasmSys) Spawn(command []string, dir string, stderr io.Writer) (io.ReadWriteCloser, error) {
	return nil, errors.New("spawn not supported on wasm")
}

func main() {}
