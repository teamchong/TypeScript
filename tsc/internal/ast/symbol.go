package ast

import (
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/debug"
)

// Symbol stores flags, checkFlags, id, and declaration header uniquely for every symbol instance.
// Ordinary symbols are embedded at offset 0 of SymbolWithData and store their parent in parentOrData;
// transient symbols created with SetSymbolData set checkFlagsSharedData and store their owner Symbol
// in parentOrData.
type Symbol struct {
	flags        SymbolFlags
	checkFlags   CheckFlags // Non-zero only in transient symbols created by Checker
	id           atomic.Uint32
	declHeader   uint32 // bits 0..14: declLen, bits 15..29: declCap, bits 30..31: lazyState
	parentOrData *Symbol
}

// Every symbol references a symbolData structure that may be shared with other symbols.
type symbolData struct {
	name             string
	declPtr          *Node
	valueDeclaration *Node
	extra            *symbolExtra
}
type symbolExtra struct {
	members      SymbolTable
	exports      SymbolTable
	exportSymbol *Symbol
}

// SymbolExtraArena batches optional symbol metadata without adding an allocator
// pointer to every symbol. References from symbolData keep its blocks alive.
type SymbolExtraArena struct {
	arena core.Arena[symbolExtra]
}

const (
	checkFlagsReferencedAny           CheckFlags = 1 << 28
	checkFlagsReferencedVariable      CheckFlags = 1 << 29
	checkFlagsReferencedTypeParameter CheckFlags = 1 << 30
	checkFlagsSharedData              CheckFlags = 1 << 31
	checkFlagsReferencedMask          CheckFlags = checkFlagsReferencedAny | checkFlagsReferencedVariable | checkFlagsReferencedTypeParameter
	checkFlagsInternalMask            CheckFlags = checkFlagsReferencedMask | checkFlagsSharedData

	declLenCapMask   uint32 = 0x7fff
	declCapShift     uint32 = 15
	lazyStateShift   uint32 = 30
	lazyStateMask    uint32 = 3 << lazyStateShift
	lazyStatePending uint32 = 1 << lazyStateShift
	lazyStateActive  uint32 = 2 << lazyStateShift
)

func (s *Symbol) owner() *Symbol {
	if s.checkFlags&checkFlagsSharedData != 0 {
		return s.parentOrData
	}
	return s
}

func (s *Symbol) data() *symbolData {
	if s.checkFlags&checkFlagsSharedData != 0 {
		return &((*SymbolWithData)(unsafe.Pointer(s.parentOrData)).d)
	}
	return &((*SymbolWithData)(unsafe.Pointer(s)).d)
}

func (s *Symbol) Flags() SymbolFlags { return s.flags }
func (s *Symbol) CheckFlags() CheckFlags {
	return CheckFlags(atomic.LoadUint32((*uint32)(&s.checkFlags))) &^ checkFlagsInternalMask
}

func (s *Symbol) AddReference(meaning SymbolFlags) {
	if s == nil {
		return
	}
	flags := uint32(checkFlagsReferencedAny)
	if meaning&SymbolFlagsVariable != 0 {
		flags |= uint32(checkFlagsReferencedVariable)
	}
	if meaning&SymbolFlagsTypeParameter != 0 {
		flags |= uint32(checkFlagsReferencedTypeParameter)
	}
	for {
		old := atomic.LoadUint32((*uint32)(&s.checkFlags))
		if old&flags == flags || atomic.CompareAndSwapUint32((*uint32)(&s.checkFlags), old, old|flags) {
			break
		}
	}
}

func (s *Symbol) ReferenceKinds() SymbolFlags {
	if s == nil {
		return 0
	}
	cf := CheckFlags(atomic.LoadUint32((*uint32)(&s.checkFlags)))
	if cf&checkFlagsReferencedMask == 0 {
		return 0
	}
	res := SymbolFlagsAll &^ (SymbolFlagsVariable | SymbolFlagsTypeParameter)
	if cf&checkFlagsReferencedVariable != 0 {
		res |= SymbolFlagsVariable
	}
	if cf&checkFlagsReferencedTypeParameter != 0 {
		res |= SymbolFlagsTypeParameter
	}
	return res
}
func (s *Symbol) Name() string { return s.data().name }
func (s *Symbol) Declarations() []*Node {
	owner := s.owner()
	hdr := owner.declHeader
	declLen := hdr & declLenCapMask
	if declLen == 0 {
		return nil
	}
	d := &((*SymbolWithData)(unsafe.Pointer(owner)).d)
	declCap := (hdr >> declCapShift) & declLenCapMask
	if declCap == 0 {
		return unsafe.Slice(&d.declPtr, 1)
	}
	return unsafe.Slice((**Node)(unsafe.Pointer(d.declPtr)), declCap)[:declLen]
}
func (s *Symbol) ValueDeclaration() *Node { return s.data().valueDeclaration }
func (s *Symbol) Parent() *Symbol         { return s.owner().parentOrData }

func (s *Symbol) Members() SymbolTable {
	owner := s.owner()
	if owner.declHeader&lazyStateMask != 0 {
		return s.membersSlow()
	}
	d := &((*SymbolWithData)(unsafe.Pointer(owner)).d)
	if d.extra == nil {
		return nil
	}
	return d.extra.members
}

func (s *Symbol) Exports() SymbolTable {
	owner := s.owner()
	if owner.declHeader&lazyStateMask != 0 {
		return s.exportsSlow()
	}
	d := &((*SymbolWithData)(unsafe.Pointer(owner)).d)
	if d.extra == nil {
		return nil
	}
	return d.extra.exports
}
func (s *Symbol) ExportSymbol() *Symbol {
	d := s.data()
	if d.extra == nil {
		return nil
	}
	return d.extra.exportSymbol
}

func (s *Symbol) SetFlags(value SymbolFlags) { s.flags = value }
func (s *Symbol) SetCheckFlags(value CheckFlags) {
	for {
		old := atomic.LoadUint32((*uint32)(&s.checkFlags))
		next := (old & uint32(checkFlagsInternalMask)) | (uint32(value) &^ uint32(checkFlagsInternalMask))
		if atomic.CompareAndSwapUint32((*uint32)(&s.checkFlags), old, next) {
			break
		}
	}
}
func (s *Symbol) SetName(value string) { s.data().name = value }
func (s *Symbol) SetSingleDeclaration(node *Node) {
	owner := s.owner()
	d := &((*SymbolWithData)(unsafe.Pointer(owner)).d)
	d.declPtr = node
	owner.declHeader = (owner.declHeader & lazyStateMask) | 1
}

func (s *Symbol) SetDeclarations(value []*Node) {
	owner := s.owner()
	d := &((*SymbolWithData)(unsafe.Pointer(owner)).d)
	lazy := owner.declHeader & lazyStateMask
	if len(value) == 0 {
		d.declPtr = nil
		owner.declHeader = lazy
		return
	}
	d.declPtr = (*Node)(unsafe.Pointer(unsafe.SliceData(value)))
	declLen := uint32(len(value)) & declLenCapMask
	declCap := uint32(max(cap(value), len(value))) & declLenCapMask
	owner.declHeader = lazy | declLen | (declCap << declCapShift)
}
func (s *Symbol) SetValueDeclaration(value *Node) { s.data().valueDeclaration = value }
func (s *Symbol) SetParent(value *Symbol)         { s.owner().parentOrData = value }

func (s *Symbol) ensureExtra(arena *SymbolExtraArena) *symbolExtra {
	d := s.data()
	if d.extra == nil {
		d.extra = arena.arena.New()
	}
	return d.extra
}

func (s *Symbol) SetMembers(value SymbolTable, arena *SymbolExtraArena) {
	d := s.data()
	if value != nil || d.extra != nil {
		s.ensureExtra(arena).members = value
	}
}

func (s *Symbol) SetExports(value SymbolTable, arena *SymbolExtraArena) {
	d := s.data()
	if value != nil || d.extra != nil {
		s.ensureExtra(arena).exports = value
	}
}

func (s *Symbol) SetExportSymbol(value *Symbol, arena *SymbolExtraArena) {
	d := s.data()
	if value != nil || d.extra != nil {
		s.ensureExtra(arena).exportSymbol = value
	}
}

// SymbolWithData is a helper structure that contains both a Symbol and its associated symbolData.
type SymbolWithData struct {
	s Symbol
	d symbolData
}

// Initializes a SymbolWithData instance and returns the embedded Symbol.
func (sd *SymbolWithData) Initialize() *Symbol {
	return &sd.s
}

func NewSymbol() *Symbol {
	return (&SymbolWithData{}).Initialize()
}

// Initializes a Symbol's data field with the data from another Symbol. This allows multiple
func (s *Symbol) SetSymbolData(other *Symbol) {
	s.parentOrData = other.owner()
	s.checkFlags |= checkFlagsSharedData
}
// GetSourceFileOfSymbol returns the owning file of a published binder symbol, or
// nil for a non-file-owned symbol, even if it borrows declarations from a file.
// Ownership recovery walks only the first declaration's AST parents.
func GetSourceFileOfSymbol(symbol *Symbol) *SourceFile {
	debug.Assert(symbol != nil, "Expected a symbol")
	if symbol.Flags()&SymbolFlagsTransient != 0 {
		return nil
	}
	if len(symbol.Declarations()) == 0 {
		// A class's implicit prototype has no declaration of its own.
		debug.Assert(symbol.Flags()&SymbolFlagsPrototype != 0, "File-bound symbol has no declarations")
		debug.Assert(symbol.Parent() != nil && symbol.Parent().Flags()&SymbolFlagsClass != 0, "Prototype has no declaring class")
		symbol = symbol.Parent()
		debug.Assert(symbol.Flags()&SymbolFlagsTransient == 0, "Prototype parent is not file-bound")
		debug.Assert(len(symbol.Declarations()) != 0, "Prototype parent has no declarations")
	}
	file := GetSourceFileOfNode(symbol.Declarations()[0])
	debug.Assert(file != nil, "File-bound declaration has no source file")
	return file
}

func (s *Symbol) IsExternalModule() bool {
	return s.Flags()&SymbolFlagsModule != 0 && IsAmbientModuleSymbolName(s.Name())
}

func (s *Symbol) IsStatic() bool {
	if s.ValueDeclaration() == nil {
		return false
	}
	modifierFlags := s.ValueDeclaration().ModifierFlags()
	return modifierFlags&ModifierFlagsStatic != 0
}

// See comment on `declareModuleMember` in `binder.go`.
func (s *Symbol) CombinedLocalAndExportSymbolFlags() SymbolFlags {
	if s.ExportSymbol() != nil {
		return s.Flags() | s.ExportSymbol().Flags()
	}
	return s.Flags()
}

// SymbolTable

type SymbolTable map[string]*Symbol

const InternalSymbolNamePrefix = "\xFE" // Invalid UTF8 sequence, will never occur as IdentifierName

const (
	InternalSymbolNameCall                    = InternalSymbolNamePrefix + "call"                    // Call signatures
	InternalSymbolNameConstructor             = InternalSymbolNamePrefix + "constructor"             // Constructor implementations
	InternalSymbolNameNew                     = InternalSymbolNamePrefix + "new"                     // Constructor signatures
	InternalSymbolNameIndex                   = InternalSymbolNamePrefix + "index"                   // Index signatures
	InternalSymbolNameExportStar              = InternalSymbolNamePrefix + "export"                  // Module export * declarations
	InternalSymbolNameGlobal                  = InternalSymbolNamePrefix + "global"                  // Global self-reference
	InternalSymbolNameMissing                 = InternalSymbolNamePrefix + "missing"                 // Indicates missing symbol
	InternalSymbolNameType                    = InternalSymbolNamePrefix + "type"                    // Anonymous type literal symbol
	InternalSymbolNameObject                  = InternalSymbolNamePrefix + "object"                  // Anonymous object literal declaration
	InternalSymbolNameJSXAttributes           = InternalSymbolNamePrefix + "jsxAttributes"           // Anonymous JSX attributes object literal declaration
	InternalSymbolNameClass                   = InternalSymbolNamePrefix + "class"                   // Unnamed class expression
	InternalSymbolNameFunction                = InternalSymbolNamePrefix + "function"                // Unnamed function expression
	InternalSymbolNameComputed                = InternalSymbolNamePrefix + "computed"                // Computed property name declaration with dynamic name
	InternalSymbolNameAssignmentDeclaration   = InternalSymbolNamePrefix + "assignment"              // Assignment declarations
	InternalSymbolNameInstantiationExpression = InternalSymbolNamePrefix + "instantiationExpression" // Instantiation expressions
	InternalSymbolNameImportAttributes        = InternalSymbolNamePrefix + "importAttributes"
	InternalSymbolNameExportEquals            = "export=" // Export assignment symbol
	InternalSymbolNameDefault                 = "default" // Default export symbol (technically not wholly internal, but included here for usability)
	InternalSymbolNameThis                    = "this"
	InternalSymbolNameModuleExports           = "module.exports"
)

func SymbolName(symbol *Symbol) string {
	if symbol.ValueDeclaration() != nil && IsPrivateIdentifierClassElementDeclaration(symbol.ValueDeclaration()) {
		return symbol.ValueDeclaration().Name().Text()
	}
	return symbol.Name()
}

// EscapeAllInternalSymbolNames replaces internal symbol name markers ("\xFE") with "__".
func EscapeAllInternalSymbolNames(name string) string {
	return strings.ReplaceAll(name, InternalSymbolNamePrefix, "__")
}

func EscapeInternalSymbolName(name string) string {
	if rest, ok := strings.CutPrefix(name, InternalSymbolNamePrefix); ok {
		return "__" + rest
	}
	return name
}

// EscapeSymbolName converts a binder symbol name into its escaped "__String"
// form. Internal names (prefixed with the "\xFE" sentinel) become "__"-prefixed,
// and user names that already begin with "__" gain an extra leading underscore
// so they can be distinguished from internal names.
func EscapeSymbolName(name string) string {
	if rest, ok := strings.CutPrefix(name, InternalSymbolNamePrefix); ok {
		return "__" + rest
	}
	if len(name) >= 2 && name[0] == '_' && name[1] == '_' {
		return "_" + name
	}
	return name
}
