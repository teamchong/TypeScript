package ast

import (
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/microsoft/TypeScript/tsc/internal/collections"
	"github.com/microsoft/TypeScript/tsc/internal/core"
)

const (
	LazyListPending  uint32 = 0 // Parsed lazily; binder has not visited owner yet
	LazyListDeferred uint32 = 1 // Binder visited owner and deferred binding
	LazyListUnbound  uint32 = 2 // File finished binding without visiting owner
	LazyListDone     uint32 = 3 // Forced (parsed and bound if deferred)

	LazyParsingContextTypeAliasRHS uint8 = 255
)

var (
	lazySentinelElem *Node
	// SAFETY: Creates a 0-length slice backed by the address of lazySentinelElem as a sentinel tag.
	lazySentinelNodes = unsafe.Slice(&lazySentinelElem, 1)[:0]
)

type LazyBindContext struct {
	Container           *Node
	ThisContainer       *Node
	BlockScopeContainer *Node
	CurrentFlow         *FlowNode
	UnreachableFlow     *FlowNode
}

type LazyNodeList struct {
	NodeList        // Must be at offset 0 so (*LazyNodeList)(unsafe.Pointer(list)) is valid
	Placeholder     Node
	state           atomic.Uint32
	ParsingContext  uint8
	ContextFlags    NodeFlags
	ParsingContexts uint32
	Owner           *Node
	File            *SourceFile
	BindCtx         *LazyBindContext
	forcedNodes     []*Node
	nestedLazyLists []*LazyNodeList
	mu              sync.Mutex
}

type savedMemberSymbolState struct {
	sym        *Symbol
	declHeader uint32
	declPtr    unsafe.Pointer
	valueDecl  *Node
}

type forcedSymbolState struct {
	owner        *Symbol
	prevExtra    *symbolExtra
	savedMembers []savedMemberSymbolState
}

var (
	lazyReparseHook        func(lazy *LazyNodeList, file *SourceFile, allowNestedLazy bool) []*Node
	lazyLookupHook         func(lazy *LazyNodeList, file *SourceFile, name string) *Node
	lazyBindHook           func(lazy *LazyNodeList, file *SourceFile, nodes []*Node)
	lazySymbolLocks        [256]sync.Mutex
	trackForcedLazy        atomic.Bool
	forcedLazyMu           sync.Mutex
	forcedLazyLists        []*LazyNodeList
	forcedLazySymbols      []forcedSymbolState
	wasmHeapWatermark      uintptr
	watermarkNodeId        uint32
	watermarkSymbolId      uint32
	watermarkNodeBlockId   uint32
	watermarkSymbolBlockId uint32
	forcedNodeIds          []*Node
	forcedSymbolIds        []*Symbol
)

func SetTrackForcedLazy(enabled bool) {
	if enabled && wasmHeapWatermark == 0 {
		// SAFETY: Converts a newly allocated heap address to uintptr to record the current bump-allocator watermark.
		wasmHeapWatermark = uintptr(unsafe.Pointer(new(uint64)))
		watermarkNodeId = nextNodeId.Load()
		watermarkSymbolId = nextSymbolId.Load()
		watermarkNodeBlockId = nextNodeBlockId.Load()
		watermarkSymbolBlockId = nextSymbolBlockId.Load()
	}
	trackForcedLazy.Store(enabled)
}

func recordForcedNodeId(node *Node) {
	forcedLazyMu.Lock()
	forcedNodeIds = append(forcedNodeIds, node)
	forcedLazyMu.Unlock()
}

func recordForcedSymbolId(symbol *Symbol) {
	forcedLazyMu.Lock()
	forcedSymbolIds = append(forcedSymbolIds, symbol)
	forcedLazyMu.Unlock()
}

func ResetForcedLazyLists() {
	forcedLazyMu.Lock()
	defer forcedLazyMu.Unlock()
	for i := len(forcedLazySymbols) - 1; i >= 0; i-- { //nolint:modernize
		entry := forcedLazySymbols[i]
		for _, sm := range entry.savedMembers {
			// SAFETY: savedMembers only records owner Symbols embedded at offset zero of SymbolWithData.
			md := &(*SymbolWithData)(unsafe.Pointer(sm.sym)).d
			sm.sym.declHeader = sm.declHeader
			md.declPtr = sm.declPtr
			md.valueDeclaration = sm.valueDecl
		}
		// SAFETY: entry.owner is an owner Symbol embedded at offset zero of SymbolWithData.
		d := &(*SymbolWithData)(unsafe.Pointer(entry.owner)).d
		d.extra = entry.prevExtra
		entry.owner.SetHasLazyMembers()
	}
	forcedLazySymbols = nil
	for i := len(forcedLazyLists) - 1; i >= 0; i-- { //nolint:modernize
		l := forcedLazyLists[i]
		l.forcedNodes = nil
		l.nestedLazyLists = nil
		if l.BindCtx != nil {
			l.state.Store(LazyListDeferred)
		} else {
			l.state.Store(LazyListUnbound)
		}
	}
	forcedLazyLists = nil
	for _, n := range forcedNodeIds {
		n.id.Store(0)
	}
	forcedNodeIds = nil
	for _, s := range forcedSymbolIds {
		s.id.Store(0)
	}
	forcedSymbolIds = nil
	if wasmHeapWatermark != 0 {
		nextNodeId.Store(watermarkNodeId)
		nextSymbolId.Store(watermarkSymbolId)
		nextNodeBlockId.Store(watermarkNodeBlockId)
		nextSymbolBlockId.Store(watermarkSymbolBlockId)
	}
	for i := range subtreeFactsShards {
		subtreeFactsShards[i].m = nil
	}
	setParentInChildrenPool = sync.Pool{
		New: func() any {
			return newParentInChildrenSetter()
		},
	}
}

func (node *SourceFile) ResetWasmCaches() {
	node.jsdocCache = nil
	node.identifiersOnce = sync.Once{}
	node.identifiers = collections.Set[string]{}
	node.declarationMap = nil
	node.nameTableOnce = sync.Once{}
	node.nameTable = nil
	node.positionMapOnce = sync.Once{}
	node.positionMap = nil
	node.ecmaLineMap = nil
	node.tokenCache = nil
	node.tokenFactory = nil
}

func SetLazyParseHook(fn func(lazy *LazyNodeList, file *SourceFile, allowNestedLazy bool) []*Node) {
	lazyReparseHook = fn
}

func SetLazyBindHook(fn func(lazy *LazyNodeList, file *SourceFile, nodes []*Node)) {
	lazyBindHook = fn
}

func SetLazyLookupHook(fn func(lazy *LazyNodeList, file *SourceFile, name string) *Node) {
	lazyLookupHook = fn
}

func NewLazyNodeList(loc core.TextRange, parsingContext uint8, contextFlags NodeFlags, parsingContexts uint32) *LazyNodeList {
	var list LazyNodeList //nolint:customlint
	list.Loc = loc
	list.Nodes = lazySentinelNodes
	list.ParsingContext = parsingContext
	list.ContextFlags = contextFlags
	list.ParsingContexts = parsingContexts
	return &list
}

func NewLazyTypeAliasRHS(loc core.TextRange, contextFlags NodeFlags, parsingContexts uint32) *LazyNodeList {
	var list LazyNodeList //nolint:customlint
	list.Loc = loc
	list.Nodes = lazySentinelNodes
	list.ParsingContext = LazyParsingContextTypeAliasRHS
	list.ContextFlags = contextFlags
	list.ParsingContexts = parsingContexts
	list.Placeholder.Loc = loc
	list.Placeholder.Kind = KindUnknown //nolint:customlint
	// SAFETY: Stores the enclosing LazyNodeList pointer in Placeholder.Parent for lazy type-alias RHS resolution.
	list.Placeholder.Parent = (*Node)(unsafe.Pointer(&list))
	return &list
}

func (n *Node) IsLazyTypeNode() bool {
	return n != nil && n.Kind == KindUnknown && n.Parent != nil
}

func (n *Node) AsLazyTypeNode() *LazyNodeList {
	// SAFETY: IsLazyTypeNode callers guarantee n.Parent holds the *LazyNodeList stored by NewLazyTypeAliasRHS.
	return (*LazyNodeList)(unsafe.Pointer(n.Parent))
}

//go:noinline
func (n *Node) forceLazyTypeNode() *Node {
	if n.Parent == nil {
		return nil
	}
	// SAFETY: forceLazyTypeNode is only called on KindUnknown placeholders created by NewLazyTypeAliasRHS.
	nodes := (*LazyNodeList)(unsafe.Pointer(n.Parent)).Force()
	if len(nodes) > 0 {
		return nodes[0]
	}
	return nil
}

func SetLazyTypeOwner(typeNode *Node, owner *Node) {
	if typeNode.IsLazyTypeNode() {
		typeNode.AsLazyTypeNode().Owner = owner
	}
}

func (list *NodeList) IsLazy() bool {
	// SAFETY: SliceData only inspects the backing-array pointer of list.Nodes.
	return list != nil && unsafe.SliceData(list.Nodes) == &lazySentinelElem
}

func (list *NodeList) AsLazy() *LazyNodeList {
	// SAFETY: IsLazy callers guarantee list was allocated as a LazyNodeList, which embeds NodeList at offset zero.
	return (*LazyNodeList)(unsafe.Pointer(list))
}

func (list *NodeList) Slice() []*Node {
	if list == nil {
		return nil
	}
	// SAFETY: SliceData only inspects the backing-array pointer of list.Nodes.
	if unsafe.SliceData(list.Nodes) == &lazySentinelElem {
		return list.forceLazy()
	}
	return list.Nodes
}

//go:noinline
func (list *NodeList) forceLazy() []*Node {
	// SAFETY: forceLazy is only called when list.Nodes has the lazySentinelElem backing pointer of a LazyNodeList.
	return (*LazyNodeList)(unsafe.Pointer(list)).Force()
}

func SetLazyOwner(list *NodeList, owner *Node) {
	// SAFETY: SliceData only inspects the backing-array pointer of list.Nodes.
	if list != nil && unsafe.SliceData(list.Nodes) == &lazySentinelElem {
		// SAFETY: The lazySentinelElem backing pointer guarantees list is embedded at offset zero of a LazyNodeList.
		(*LazyNodeList)(unsafe.Pointer(list)).Owner = owner
	}
}

func (lazy *LazyNodeList) State() uint32 {
	return lazy.state.Load()
}

func (lazy *LazyNodeList) SetNestedLazyLists(lists []*LazyNodeList) {
	lazy.nestedLazyLists = lists
}

func (lazy *LazyNodeList) NestedLazyLists() []*LazyNodeList {
	return lazy.nestedLazyLists
}

func (lazy *LazyNodeList) Defer(file *SourceFile, ctx *LazyBindContext) {
	lazy.File = file
	lazy.BindCtx = ctx
	lazy.state.Store(LazyListDeferred)
}

func (lazy *LazyNodeList) MarkUnbound(file *SourceFile) {
	if lazy.state.Load() == LazyListPending {
		lazy.File = file
		lazy.state.Store(LazyListUnbound)
	}
}

func (lazy *LazyNodeList) ParseNow(file *SourceFile) []*Node {
	lazy.File = file
	nodes := lazyReparseHook(lazy, file, true /*allowNestedLazy*/)
	lazy.forcedNodes = nodes
	lazy.state.Store(LazyListDone)
	return nodes
}

func (lazy *LazyNodeList) Force() []*Node {
	switch lazy.state.Load() {
	case LazyListDone:
		return lazy.forcedNodes
	case LazyListPending:
		return nil
	}
	if lazy.Owner != nil {
		if sym := lazy.Owner.Symbol(); sym != nil && atomic.LoadUint32(&sym.owner().declHeader)&lazyStateMask != 0 {
			sym.forceLazyMembers()
			if lazy.state.Load() == LazyListDone {
				return lazy.forcedNodes
			}
		}
	}
	lazy.mu.Lock()
	defer lazy.mu.Unlock()
	if lazy.state.Load() == LazyListDone {
		return lazy.forcedNodes
	}
	if lazy.state.Load() == LazyListPending {
		return nil
	}
	wasDeferred := lazy.state.Load() == LazyListDeferred
	nodes := lazyReparseHook(lazy, lazy.File, wasDeferred)
	lazy.forcedNodes = nodes
	if wasDeferred && lazy.BindCtx != nil {
		lazyBindHook(lazy, lazy.File, nodes)
	}
	for _, nested := range lazy.nestedLazyLists {
		nested.MarkUnbound(lazy.File)
	}
	lazy.nestedLazyLists = nil
	if trackForcedLazy.Load() {
		forcedLazyMu.Lock()
		forcedLazyLists = append(forcedLazyLists, lazy)
		forcedLazyMu.Unlock()
	} else {
		lazy.BindCtx = nil
	}
	lazy.state.Store(LazyListDone)
	return nodes
}

func (s *Symbol) SetHasLazyMembers() {
	owner := s.owner()
	for {
		old := atomic.LoadUint32(&owner.declHeader)
		next := (old &^ lazyStateMask) | lazyStatePending
		if atomic.CompareAndSwapUint32(&owner.declHeader, old, next) {
			break
		}
	}
}

func (s *Symbol) HasLazyMembers() bool {
	return s != nil && atomic.LoadUint32(&s.owner().declHeader)&lazyStateMask == lazyStatePending
}

//go:noinline
func (s *Symbol) membersSlow() SymbolTable {
	if atomic.LoadUint32(&s.owner().declHeader)&lazyStateMask != 0 {
		s.forceLazyMembers()
	}
	d := s.data()
	if d.extra == nil {
		return nil
	}
	return d.extra.members
}

//go:noinline
func (s *Symbol) exportsSlow() SymbolTable {
	if atomic.LoadUint32(&s.owner().declHeader)&lazyStateMask != 0 {
		s.forceLazyMembers()
	}
	d := s.data()
	if d.extra == nil {
		return nil
	}
	return d.extra.exports
}

//go:noinline
func memberCountHint(decl *Node) int {
	list := decl.MemberList()
	if list == nil {
		return 0
	}
	if list.IsLazy() {
		return len(list.AsLazy().forcedNodes)
	}
	return len(list.Nodes)
}

func (s *Symbol) getMembersSlow(arena *SymbolExtraArena) SymbolTable {
	if atomic.LoadUint32(&s.owner().declHeader)&lazyStateMask == lazyStatePending {
		s.forceLazyMembers()
	}
	m := &s.ensureExtra(arena).members
	if *m == nil {
		var capHint int
		if decls := s.Declarations(); len(decls) > 0 && decls[0] != nil {
			switch decls[0].Kind {
			case KindInterfaceDeclaration, KindClassDeclaration, KindClassExpression, KindTypeLiteral:
				capHint = memberCountHint(decls[0])
			case KindObjectLiteralExpression, KindJsxAttributes:
				capHint = len(decls[0].Properties())
			}
		}
		*m = NewSymbolTableWithCapacity(capHint)
	}
	return *m
}

//go:noinline
func (s *Symbol) getExportsSlow(arena *SymbolExtraArena) SymbolTable {
	if atomic.LoadUint32(&s.owner().declHeader)&lazyStateMask == lazyStatePending {
		s.forceLazyMembers()
	}
	m := &s.ensureExtra(arena).exports
	if *m == nil {
		var capHint int
		if decls := s.Declarations(); len(decls) > 0 && decls[0] != nil {
			switch decls[0].Kind {
			case KindEnumDeclaration:
				capHint = memberCountHint(decls[0])
			case KindSourceFile:
				capHint = len(decls[0].Statements())
			case KindModuleDeclaration:
				if body := decls[0].Body(); body != nil && body.Kind == KindModuleBlock {
					capHint = len(body.Statements())
				}
			}
		}
		*m = NewSymbolTableWithCapacity(capHint)
	}
	return *m
}

func saveTableMemberStates(t SymbolTable, dst []savedMemberSymbolState) []savedMemberSymbolState {
	for m := range t.Values() {
		if m != nil {
			mo := m.owner()
			// SAFETY: owner() returns a Symbol embedded at offset zero of SymbolWithData.
			md := &(*SymbolWithData)(unsafe.Pointer(mo)).d
			dst = append(dst, savedMemberSymbolState{
				sym:        mo,
				declHeader: mo.declHeader,
				declPtr:    md.declPtr,
				valueDecl:  md.valueDeclaration,
			})
		}
	}
	return dst
}

func (s *Symbol) forceLazyMembers() {
	if s == nil {
		return
	}
	owner := s.owner()
	if atomic.LoadUint32(&owner.declHeader)&lazyStateMask == 0 {
		return
	}
	// SAFETY: Converts the owner Symbol address to uintptr solely to select a lock shard.
	mu := &lazySymbolLocks[(uintptr(unsafe.Pointer(owner))>>6)&255]
	mu.Lock()
	defer mu.Unlock()
	if atomic.LoadUint32(&owner.declHeader)&lazyStateMask != lazyStatePending {
		return
	}
	tracking := trackForcedLazy.Load()
	if tracking {
		// SAFETY: owner() returns a Symbol embedded at offset zero of SymbolWithData.
		d := &(*SymbolWithData)(unsafe.Pointer(owner)).d
		prevExtra := d.extra
		var savedMembers []savedMemberSymbolState
		if prevExtra != nil {
			cloned := *prevExtra
			if cloned.members != nil {
				savedMembers = saveTableMemberStates(cloned.members, savedMembers)
				cloned.members = cloned.members.Clone()
			}
			if cloned.exports != nil {
				savedMembers = saveTableMemberStates(cloned.exports, savedMembers)
				cloned.exports = cloned.exports.Clone()
			}
			d.extra = &cloned
		}
		forcedLazyMu.Lock()
		forcedLazySymbols = append(forcedLazySymbols, forcedSymbolState{
			owner:        owner,
			prevExtra:    prevExtra,
			savedMembers: savedMembers,
		})
		forcedLazyMu.Unlock()
	}
	for {
		old := atomic.LoadUint32(&owner.declHeader)
		if atomic.CompareAndSwapUint32(&owner.declHeader, old, (old&^lazyStateMask)|lazyStateActive) {
			break
		}
	}
	for _, decl := range s.Declarations() {
		if decl == nil {
			continue
		}
		switch decl.Kind {
		case KindInterfaceDeclaration, KindClassDeclaration, KindClassExpression, KindTypeLiteral:
			list := decl.MemberList()
			if list.IsLazy() {
				l := list.AsLazy()
				if l.state.Load() == LazyListDeferred {
					nodes := lazyReparseHook(l, l.File, true /*allowNestedLazy*/)
					l.forcedNodes = nodes
					lazyBindHook(l, l.File, nodes)
					for _, nested := range l.nestedLazyLists {
						nested.MarkUnbound(l.File)
					}
					l.nestedLazyLists = nil
					if tracking {
						forcedLazyMu.Lock()
						forcedLazyLists = append(forcedLazyLists, l)
						forcedLazyMu.Unlock()
					} else {
						l.BindCtx = nil
					}
					l.state.Store(LazyListDone)
				}
			}
		}
	}
	for {
		old := atomic.LoadUint32(&owner.declHeader)
		if atomic.CompareAndSwapUint32(&owner.declHeader, old, old&^lazyStateMask) {
			break
		}
	}
}

func (lazy *LazyNodeList) IsBinaryKeyedTable() bool {
	if lazy == nil || lazy.Loc.Len() <= 8192 || lazy.File == nil {
		return false
	}
	text := lazy.File.Text()[lazy.Loc.Pos():lazy.Loc.End()]
	if len(text) < 8 || text[0] != '\n' {
		return false
	}
	i := 1
	for i < len(text) && i < 16 && text[i] == ' ' {
		i++
	}
	if i == 1 || i >= len(text) || text[i] != '\'' {
		return false
	}
	kStart := i + 1
	kEnd := kStart
	for kEnd < len(text) && kEnd < kStart+64 && (text[kEnd] == '0' || text[kEnd] == '1') {
		kEnd++
	}
	return kEnd > kStart && kEnd+2 <= len(text) && text[kEnd] == '\'' && text[kEnd+1] == ':'
}

func (s *Symbol) IsBinaryKeyedTypeLiteral() bool {
	if s == nil || !s.HasLazyMembers() {
		return false
	}
	decls := s.Declarations()
	if len(decls) != 1 || decls[0] == nil || decls[0].Kind != KindTypeLiteral {
		return false
	}
	list := decls[0].MemberList()
	return list != nil && list.IsLazy() && list.AsLazy().IsBinaryKeyedTable()
}

func (s *Symbol) LookupLazyMember(name string) *Symbol {
	if s == nil {
		return nil
	}
	owner := s.owner()
	// SAFETY: Converts the owner Symbol address to uintptr solely to select a lock shard.
	mu := &lazySymbolLocks[(uintptr(unsafe.Pointer(owner))>>6)&255]
	mu.Lock()
	defer mu.Unlock()
	// SAFETY: owner() returns a Symbol embedded at offset zero of SymbolWithData.
	d := &(*SymbolWithData)(unsafe.Pointer(owner)).d
	if atomic.LoadUint32(&owner.declHeader)&lazyStateMask != lazyStatePending {
		if d.extra == nil {
			return nil
		}
		return d.extra.members.Get(name)
	}
	if d.extra != nil && d.extra.members != nil {
		if sym, ok := d.extra.members.Lookup(name); ok {
			return sym
		}
	}
	decls := s.Declarations()
	if len(decls) != 1 || decls[0] == nil || decls[0].Kind != KindTypeLiteral {
		return nil
	}
	list := decls[0].MemberList()
	if list == nil || !list.IsLazy() {
		return nil
	}
	l := list.AsLazy()
	if l.state.Load() != LazyListDeferred || lazyLookupHook == nil {
		return nil
	}
	node := lazyLookupHook(l, l.File, name)
	if node == nil {
		return nil
	}
	if trackForcedLazy.Load() && (d.extra == nil || d.extra.members == nil) {
		prevExtra := d.extra
		var savedMembers []savedMemberSymbolState
		if prevExtra != nil {
			cloned := *prevExtra
			if cloned.members != nil {
				savedMembers = saveTableMemberStates(cloned.members, savedMembers)
				cloned.members = cloned.members.Clone()
			}
			if cloned.exports != nil {
				savedMembers = saveTableMemberStates(cloned.exports, savedMembers)
				cloned.exports = cloned.exports.Clone()
			}
			d.extra = &cloned
		}
		forcedLazyMu.Lock()
		forcedLazySymbols = append(forcedLazySymbols, forcedSymbolState{
			owner:        owner,
			prevExtra:    prevExtra,
			savedMembers: savedMembers,
		})
		forcedLazyMu.Unlock()
	}
	for {
		old := atomic.LoadUint32(&owner.declHeader)
		if atomic.CompareAndSwapUint32(&owner.declHeader, old, (old&^lazyStateMask)|lazyStateActive) {
			break
		}
	}
	lazyBindHook(l, l.File, []*Node{node})
	for {
		old := atomic.LoadUint32(&owner.declHeader)
		if atomic.CompareAndSwapUint32(&owner.declHeader, old, (old&^lazyStateMask)|lazyStatePending) {
			break
		}
	}
	if d.extra == nil {
		return nil
	}
	return d.extra.members.Get(name)
}
