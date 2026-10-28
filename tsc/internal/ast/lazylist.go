package ast

import (
	"maps"
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
	lazySentinelElem  *Node
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
	lazyReparseHook   func(lazy *LazyNodeList, file *SourceFile, allowNestedLazy bool) []*Node
	lazyBindHook      func(lazy *LazyNodeList, file *SourceFile, nodes []*Node)
	lazySymbolLocks   [256]sync.Mutex
	trackForcedLazy   atomic.Bool
	forcedLazyMu      sync.Mutex
	forcedLazyLists   []*LazyNodeList
	forcedLazySymbols []forcedSymbolState
)

func SetTrackForcedLazy(enabled bool) {
	trackForcedLazy.Store(enabled)
}

func ResetForcedLazyLists() {
	forcedLazyMu.Lock()
	defer forcedLazyMu.Unlock()
	for i := len(forcedLazySymbols) - 1; i >= 0; i-- {
		entry := forcedLazySymbols[i]
		for _, sm := range entry.savedMembers {
			md := &(*SymbolWithData)(unsafe.Pointer(sm.sym)).d
			sm.sym.declHeader = sm.declHeader
			md.declPtr = sm.declPtr
			md.valueDeclaration = sm.valueDecl
		}
		d := &(*SymbolWithData)(unsafe.Pointer(entry.owner)).d
		d.extra = entry.prevExtra
		entry.owner.SetHasLazyMembers()
	}
	forcedLazySymbols = nil
	for i := len(forcedLazyLists) - 1; i >= 0; i-- {
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

func NewLazyNodeList(loc core.TextRange, parsingContext uint8, contextFlags NodeFlags, parsingContexts uint32) *LazyNodeList {
	var list LazyNodeList
	list.Loc = loc
	list.Nodes = lazySentinelNodes
	list.ParsingContext = parsingContext
	list.ContextFlags = contextFlags
	list.ParsingContexts = parsingContexts
	return &list
}

func NewLazyTypeAliasRHS(loc core.TextRange, contextFlags NodeFlags, parsingContexts uint32) *LazyNodeList {
	var list LazyNodeList
	list.Loc = loc
	list.Nodes = lazySentinelNodes
	list.ParsingContext = LazyParsingContextTypeAliasRHS
	list.ContextFlags = contextFlags
	list.ParsingContexts = parsingContexts
	list.Placeholder.Loc = loc
	list.Placeholder.Kind = KindUnknown
	list.Placeholder.Parent = (*Node)(unsafe.Pointer(&list))
	return &list
}

func (n *Node) IsLazyTypeNode() bool {
	return n != nil && n.Kind == KindUnknown && n.Parent != nil
}

func (n *Node) AsLazyTypeNode() *LazyNodeList {
	return (*LazyNodeList)(unsafe.Pointer(n.Parent))
}

//go:noinline
func (n *Node) forceLazyTypeNode() *Node {
	if n.Parent == nil {
		return nil
	}
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
	return list != nil && unsafe.SliceData(list.Nodes) == &lazySentinelElem
}

func (list *NodeList) AsLazy() *LazyNodeList {
	return (*LazyNodeList)(unsafe.Pointer(list))
}

func (list *NodeList) Slice() []*Node {
	if list == nil {
		return nil
	}
	if unsafe.SliceData(list.Nodes) == &lazySentinelElem {
		return list.forceLazy()
	}
	return list.Nodes
}

//go:noinline
func (list *NodeList) forceLazy() []*Node {
	return (*LazyNodeList)(unsafe.Pointer(list)).Force()
}

func SetLazyOwner(list *NodeList, owner *Node) {
	if list != nil && unsafe.SliceData(list.Nodes) == &lazySentinelElem {
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
	for _, m := range t {
		if m != nil {
			mo := m.owner()
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
	mu := &lazySymbolLocks[(uintptr(unsafe.Pointer(owner))>>6)&255]
	mu.Lock()
	defer mu.Unlock()
	if atomic.LoadUint32(&owner.declHeader)&lazyStateMask != lazyStatePending {
		return
	}
	tracking := trackForcedLazy.Load()
	if tracking {
		d := &(*SymbolWithData)(unsafe.Pointer(owner)).d
		prevExtra := d.extra
		var savedMembers []savedMemberSymbolState
		if prevExtra != nil {
			cloned := *prevExtra
			if cloned.members != nil {
				savedMembers = saveTableMemberStates(cloned.members, savedMembers)
				cloned.members = maps.Clone(cloned.members)
			}
			if cloned.exports != nil {
				savedMembers = saveTableMemberStates(cloned.exports, savedMembers)
				cloned.exports = maps.Clone(cloned.exports)
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
