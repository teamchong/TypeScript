package ast

import (
	"iter"
	"slices"
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
	declPtr          unsafe.Pointer
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
	checkFlagsMerged                  CheckFlags = 1 << 27 // Symbol is a key in Checker.mergedSymbols
	checkFlagsReferencedAny           CheckFlags = 1 << 28
	checkFlagsReferencedVariable      CheckFlags = 1 << 29
	checkFlagsReferencedTypeParameter CheckFlags = 1 << 30
	checkFlagsSharedData              CheckFlags = 1 << 31
	checkFlagsReferencedMask          CheckFlags = checkFlagsReferencedAny | checkFlagsReferencedVariable | checkFlagsReferencedTypeParameter
	checkFlagsInternalMask            CheckFlags = checkFlagsMerged | checkFlagsReferencedMask | checkFlagsSharedData
	declLenCapMask                    uint32     = 0x7fff
	declCapShift                      uint32     = 15
	lazyStateShift                    uint32     = 30
	lazyStateMask                     uint32     = 3 << lazyStateShift
	lazyStatePending                  uint32     = 1 << lazyStateShift
	lazyStateActive                   uint32     = 2 << lazyStateShift
)

func (s *Symbol) owner() *Symbol {
	if s.checkFlags&checkFlagsSharedData != 0 {
		return s.parentOrData
	}
	return s
}

func (s *Symbol) data() *symbolData {
	if s.checkFlags&checkFlagsSharedData != 0 {
		return &(*SymbolWithData)(unsafe.Pointer(s.parentOrData)).d
	}
	return &(*SymbolWithData)(unsafe.Pointer(s)).d
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

func (s *Symbol) IsMerged() bool {
	return CheckFlags(atomic.LoadUint32((*uint32)(&s.checkFlags)))&checkFlagsMerged != 0
}

func (s *Symbol) MarkMerged() {
	for {
		old := atomic.LoadUint32((*uint32)(&s.checkFlags))
		if CheckFlags(old)&checkFlagsMerged != 0 {
			return
		}
		if atomic.CompareAndSwapUint32((*uint32)(&s.checkFlags), old, old|uint32(checkFlagsMerged)) {
			return
		}
	}
}
func (s *Symbol) Name() string { return s.data().name }
func (s *Symbol) Declarations() []*Node {
	owner := s.owner()
	hdr := owner.declHeader
	declLen := hdr & declLenCapMask
	if declLen == 0 {
		return nil
	}
	d := &(*SymbolWithData)(unsafe.Pointer(owner)).d
	declCap := (hdr >> declCapShift) & declLenCapMask
	if declCap == 0 {
		return unsafe.Slice((**Node)(unsafe.Pointer(&d.declPtr)), 1)
	}
	return unsafe.Slice((**Node)(d.declPtr), declCap)[:declLen]
}

func (s *Symbol) FirstDeclaration() *Node {
	owner := s.owner()
	hdr := owner.declHeader
	if hdr&declLenCapMask == 0 {
		return nil
	}
	d := &(*SymbolWithData)(unsafe.Pointer(owner)).d
	if (hdr>>declCapShift)&declLenCapMask == 0 {
		return *(**Node)(unsafe.Pointer(&d.declPtr))
	}
	return *(**Node)(d.declPtr)
}
func (s *Symbol) ValueDeclaration() *Node { return s.data().valueDeclaration }
func (s *Symbol) Parent() *Symbol         { return s.owner().parentOrData }

func (s *Symbol) Members() SymbolTable {
	if s.declHeader < lazyStatePending {
		if extra := (*SymbolWithData)(unsafe.Pointer(s)).d.extra; extra != nil {
			return extra.members
		}
	}
	return s.membersSlow()
}

func (s *Symbol) Exports() SymbolTable {
	if s.declHeader < lazyStatePending {
		if extra := (*SymbolWithData)(unsafe.Pointer(s)).d.extra; extra != nil {
			return extra.exports
		}
	}
	return s.exportsSlow()
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
	d := &(*SymbolWithData)(unsafe.Pointer(owner)).d
	if node != nil {
		d.declPtr = unsafe.Add(unsafe.Pointer(&node.Flags), -int(unsafe.Offsetof(Node{}.Flags)))
	} else {
		d.declPtr = nil
	}
	owner.declHeader = (owner.declHeader & lazyStateMask) | 1
}

func (s *Symbol) SetDeclarations(value []*Node) {
	owner := s.owner()
	d := &(*SymbolWithData)(unsafe.Pointer(owner)).d
	lazy := owner.declHeader & lazyStateMask
	if len(value) == 0 {
		d.declPtr = nil
		owner.declHeader = lazy
		return
	}
	d.declPtr = unsafe.Pointer(unsafe.SliceData(value))
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
	s.declHeader = lazyStateMask
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
//
// A SymbolTable maps names to symbols and iterates in insertion order, like Strada's Map, so that iterating a table
// is deterministic and visits symbols in the order the binder and checker added them. A nil SymbolTable is an empty
// table that can be read and iterated but not written, like a nil map.
//
// The entries are kept in insertion order in one slice. Most tables are small, so a table is searched linearly until
// it has more than symbolTableLinearMax entries, and only then gets a hash index of entry positions. An entry does not
// store its key as the key is almost always the symbol's own name. In the rare cases when a symbol has a different name,
// the table stores a proxy symbol with the new name and a reference to the original symbol in the parent field.
//
// A table must not be changed with Set or Delete while it is being iterated.
type SymbolTable = *SymbolMap

const symbolTableLinearMax = 16

type SymbolMap struct {
	entries []*Symbol
	index   []int32 // open addressing table of entry positions plus one (0 is empty), only past symbolTableLinearMax
}

func hashSymbolName(s string) uint64 {
	n := len(s)
	if n == 0 {
		return 0
	}
	p := unsafe.Pointer(unsafe.StringData(s))
	var h uint64
	if n >= 8 {
		h = *(*uint64)(p)
		for off := 8; off+8 < n; off += 8 {
			h = (h ^ *(*uint64)(unsafe.Add(p, off))) * 0x9e3779b97f4a7c15
		}
		h ^= *(*uint64)(unsafe.Add(p, n-8)) * 0xbf58476d1ce4e5b9
	} else if n >= 4 {
		h = uint64(*(*uint32)(p)) | (uint64(*(*uint32)(unsafe.Add(p, n-4))) << 32)
	} else {
		h = uint64(*(*byte)(p)) | (uint64(*(*byte)(unsafe.Add(p, n>>1))) << 8) | (uint64(*(*byte)(unsafe.Add(p, n-1))) << 16)
	}
	h = (h ^ uint64(n)) * 0x94d049bb133111eb
	return h ^ (h >> 32)
}

func NewSymbolTable() SymbolTable {
	return NewSymbolTableWithCapacity(0)
}

func NewSymbolTableWithCapacity(capacity int) SymbolTable {
	m := &SymbolMap{}
	if capacity > 0 {
		m.entries = make([]*Symbol, 0, capacity)
		if capacity > symbolTableLinearMax {
			m.index = make([]int32, indexSizeFor(capacity))
		}
	}
	return m
}

// SymbolTableOf returns a new table holding symbol under name.
func SymbolTableOf(name string, symbol *Symbol) SymbolTable {
	m := NewSymbolTableWithCapacity(1)
	m.Set(name, symbol)
	return m
}

func (m *SymbolMap) keyAt(i int) string {
	return m.entries[i].Name()
}

func (m *SymbolMap) symbolAt(i int) *Symbol {
	symbol := m.entries[i]
	if symbol.flags == SymbolFlagsNameProxy {
		symbol = symbol.parentOrData
	}
	return symbol
}

func (m *SymbolMap) find(key string) int {
	if m == nil {
		return -1
	}
	if m.index != nil {
		mask := uint64(len(m.index) - 1)
		for slot := hashSymbolName(key) & mask; ; slot = (slot + 1) & mask {
			p := m.index[slot]
			if p == 0 {
				return -1
			}
			if i := int(p - 1); m.keyAt(i) == key {
				return i
			}
		}
	}
	for i := range m.entries {
		if m.keyAt(i) == key {
			return i
		}
	}
	return -1
}

func (m *SymbolMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

func (m *SymbolMap) Get(key string) *Symbol {
	if i := m.find(key); i >= 0 {
		return m.symbolAt(i)
	}
	return nil
}

func (m *SymbolMap) Lookup(key string) (*Symbol, bool) {
	if i := m.find(key); i >= 0 {
		return m.symbolAt(i), true
	}
	return nil, false
}

// Set replaces the symbol of an existing key in place, keeping its position, or adds the key at the end.
func (m *SymbolMap) Set(key string, symbol *Symbol) {
	if key != symbol.Name() {
		proxy := NewSymbol()
		proxy.flags = SymbolFlagsNameProxy
		proxy.data().name = key
		proxy.parentOrData = symbol
		symbol = proxy
	}
	if m.index != nil {
		mask := uint64(len(m.index) - 1)
		slot := hashSymbolName(key) & mask
		for {
			p := m.index[slot]
			if p == 0 {
				break
			}
			if i := int(p - 1); m.keyAt(i) == key {
				m.entries[i] = symbol
				return
			}
			slot = (slot + 1) & mask
		}
		i := int32(len(m.entries))
		m.entries = append(m.entries, symbol)
		if len(m.entries)*4 <= len(m.index)*3 {
			m.index[slot] = i + 1
		} else {
			m.buildIndex()
		}
		return
	}
	for i := range m.entries {
		if m.keyAt(i) == key {
			m.entries[i] = symbol
			return
		}
	}
	if cap(m.entries) == 0 {
		// Most tables hold one or two entries, so start with room for two rather than growing from one.
		m.entries = make([]*Symbol, 0, 2)
	}
	m.entries = append(m.entries, symbol)
	if len(m.entries) > symbolTableLinearMax {
		m.buildIndex()
	}
}

func indexSizeFor(n int) int {
	size := 32
	for size*3 < n*4 {
		size *= 2
	}
	return size
}

func (m *SymbolMap) insertIndex(key string, i int32) {
	mask := uint64(len(m.index) - 1)
	slot := hashSymbolName(key) & mask
	for m.index[slot] != 0 {
		slot = (slot + 1) & mask
	}
	m.index[slot] = i + 1
}

func (m *SymbolMap) buildIndex() {
	m.index = make([]int32, indexSizeFor(len(m.entries)))
	for i := range m.entries {
		m.insertIndex(m.keyAt(i), int32(i))
	}
}

func (m *SymbolMap) Delete(key string) {
	d := m.find(key)
	if d < 0 {
		return
	}
	m.entries = slices.Delete(m.entries, d, d+1)
	m.index = nil
	if len(m.entries) > symbolTableLinearMax {
		m.buildIndex()
	}
}

// All iterates over the keys and symbols of the table in insertion order.
func (m *SymbolMap) All() iter.Seq2[string, *Symbol] {
	return func(yield func(string, *Symbol) bool) {
		if m == nil {
			return
		}
		entries := m.entries
		for i := range entries {
			if !yield(m.keyAt(i), m.symbolAt(i)) {
				return
			}
		}
	}
}

func (m *SymbolMap) Keys() iter.Seq[string] {
	return func(yield func(string) bool) {
		for key := range m.All() {
			if !yield(key) {
				return
			}
		}
	}
}

func (m *SymbolMap) Values() iter.Seq[*Symbol] {
	return func(yield func(*Symbol) bool) {
		if m == nil {
			return
		}
		entries := m.entries
		for i := range entries {
			if !yield(m.symbolAt(i)) {
				return
			}
		}
	}
}

func (m *SymbolMap) Clone() SymbolTable {
	if m == nil {
		return nil
	}
	clone := &SymbolMap{entries: slices.Clone(m.entries), index: slices.Clone(m.index)}
	return clone
}

func (m *SymbolMap) Clear() {
	if m == nil {
		return
	}
	clear(m.entries)
	m.entries = m.entries[:0]
	m.index = nil
}

func (m *SymbolMap) CopyFrom(src SymbolTable) {
	if src == nil || len(src.entries) == 0 {
		return
	}
	if len(m.entries) == 0 {
		m.entries = append(m.entries, src.entries...)
		if src.index != nil {
			m.index = slices.Clone(src.index)
		}
		return
	}
	for i := range src.entries {
		m.Set(src.keyAt(i), src.symbolAt(i))
	}
}

func (m *SymbolMap) HasNameProxies() bool {
	if m == nil {
		return false
	}
	for _, entry := range m.entries {
		if entry.flags == SymbolFlagsNameProxy {
			return true
		}
	}
	return false
}
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
