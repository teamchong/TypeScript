package checker

import (
	"math/bits"
	"unsafe"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/zeebo/xxh3"
)

// Persistent memo of control flow analysis results across getFlowTypeOfReferenceEx walks.
//
// Go walks the flow graph backwards from each reference (getFlowTypeOfReference) and caches only
// inside one walk (sharedFlows) and, per reference key, at loop labels (flowLoopCache). So N
// references to the same variable or property path in a long function each walk the prefix of the
// graph above them, and N distinct properties read from a narrowed object each re-evaluate the
// narrowing of that object.
//
// The memo caches a pure sub-walk of getTypeAtFlowNode(f, flow) by (flow, memoKey(f)):
//   - no transient taint: no in-process loop label (Go's incomplete types), circular or in-progress
//     resolution, reduce label (try/finally), depth or inline-level limit, and no value cached
//     while one of those was active;
//   - no reference taint: nothing read from the reference node beyond its key (its position, for
//     constraint substitution; the property symbol a property access resolved to);
//   - the result is complete, and the frame did not touch the instantiation counters or
//     create a type, symbol, or signature.
//
// With caches that only grow, a later walk of the same frame takes a subset of the paths this one
// took, so it does not touch them either; a frame that read flowTypeCache (which is reset) is
// used only while the same cache is current.

const (
	flowDepthLimit                = 2000
	flowMemoUnTainted      uint32 = 0xffff_ffff
	flowMemoTableBits             = 12
	flowMemoTableSize             = 1 << flowMemoTableBits
	flowMemoFlagEffects    uint8  = 1
	flowMemoFlagTypeCache  uint8  = 2
	flowMemoFlagCountReset uint8  = 4
)

type flowMemoHit struct {
	t          *Type
	epoch      uint32
	height     uint16
	countReset bool
}

type flowMemoSlot struct {
	key        CacheHashKey
	flow       *ast.FlowNode
	t          *Type
	epoch      uint32
	height     uint16
	countReset bool
}

type flowMemoRegs struct {
	transient uint32
	reference uint32
	height    uint16
	flags     uint8
}

var freshFlowMemoRegs = flowMemoRegs{
	transient: flowMemoUnTainted,
	reference: flowMemoUnTainted,
	height:    0,
	flags:     0,
}

type flowFrame struct {
	start                   uint32
	saved                   flowMemoRegs
	instantiationCount      uint32
	totalInstantiationCount uint32
	created                 uint32
	inInstantiation         bool
}

type frameTaint struct {
	transient uint32
	reference uint32
	flags     uint8
}

func (ft frameTaint) isPure() bool {
	return ft.transient == flowMemoUnTainted && ft.reference == flowMemoUnTainted
}

type frameMemo struct {
	key         CacheHashKey
	checkpoints uint32
}

type savedFlowTypeCache struct {
	cache map[*ast.Node]*Type
	epoch uint32
}

type flowMemo struct {
	enabled        bool
	slots          *[flowMemoTableSize]flowMemoSlot
	serial         uint32
	regs           flowMemoRegs
	typeCacheEpoch uint32
	checkpoints    []*ast.FlowNode
	keyScratch     []byte
}

func newFlowMemo() flowMemo {
	return flowMemo{
		enabled:        true,
		serial:         1,
		regs:           freshFlowMemoRegs,
		typeCacheEpoch: flowMemoUnTainted,
	}
}

func (m *flowMemo) sourceNow() uint32 {
	return m.serial
}

func (m *flowMemo) nextSerial() uint32 {
	s := m.serial
	m.serial++
	if m.serial == flowMemoUnTainted {
		m.enabled = false
		m.serial = 1
	}
	return s
}

func (m *flowMemo) begin(instantiationCount, totalInstantiationCount, created uint32, inInstantiation bool) flowFrame {
	frame := flowFrame{
		start:                   m.nextSerial(),
		saved:                   m.regs,
		instantiationCount:      instantiationCount,
		totalInstantiationCount: totalInstantiationCount,
		created:                 created,
		inInstantiation:         inInstantiation,
	}
	m.regs = freshFlowMemoRegs
	return frame
}

func (m *flowMemo) endWith(frame flowFrame, height uint16) frameTaint {
	r := m.regs
	transient := flowMemoUnTainted
	if r.transient < frame.start {
		transient = r.transient
	}
	reference := flowMemoUnTainted
	if r.reference < frame.start {
		reference = r.reference
	}
	taint := frameTaint{
		transient: transient,
		reference: reference,
		flags:     r.flags,
	}
	p := frame.saved
	nextHeight := height + 1
	if height == 0xffff {
		nextHeight = 0xffff
	}
	if p.height > nextHeight {
		nextHeight = p.height
	}
	m.regs = flowMemoRegs{
		transient: min(r.transient, p.transient),
		reference: min(r.reference, p.reference),
		height:    nextHeight,
		flags:     r.flags | p.flags,
	}
	return taint
}

func (m *flowMemo) end(frame flowFrame) (frameTaint, uint16) {
	height := m.regs.height
	return m.endWith(frame, height), height
}

func (m *flowMemo) raiseHeight(height uint16) {
	if height > m.regs.height {
		m.regs.height = height
	}
}

func (m *flowMemo) addFlags(flags uint8) {
	m.regs.flags |= flags
}

func (m *flowMemo) saveHeight() uint16 {
	saved := m.regs.height
	m.regs.height = 0
	return saved
}

func (m *flowMemo) restoreHeight(saved uint16) {
	m.regs.height = saved
}

func (m *flowMemo) taint(source uint32) {
	if source < m.regs.transient {
		m.regs.transient = source
	}
}

func (m *flowMemo) taintReference(source uint32) {
	if source < m.regs.reference {
		m.regs.reference = source
	}
}

func flowMemoSlotIndex(flow *ast.FlowNode, key CacheHashKey) uint32 {
	f := uint64(uintptr(unsafe.Pointer(flow))) * 0x9E37_79B9_7F4A_7C15
	h := (key.Lo ^ bits.RotateLeft64(key.Hi, 29) ^ bits.RotateLeft64(f, 17)) * 0xBF58_476D_1CE4_E5B9
	return uint32(h>>(64-flowMemoTableBits)) & (flowMemoTableSize - 1)
}

func (m *flowMemo) lookup(flow *ast.FlowNode, key CacheHashKey) (flowMemoHit, bool) {
	if m.slots == nil {
		return flowMemoHit{}, false
	}
	slot := &m.slots[flowMemoSlotIndex(flow, key)]
	if slot.flow == flow && slot.key == key && slot.t != nil {
		return flowMemoHit{
			t:          slot.t,
			height:     slot.height,
			epoch:      slot.epoch,
			countReset: slot.countReset,
		}, true
	}
	return flowMemoHit{}, false
}

func (m *flowMemo) store(flow *ast.FlowNode, key CacheHashKey, t *Type, height uint16, epoch uint32, countReset bool) {
	if m.slots == nil {
		m.slots = new([flowMemoTableSize]flowMemoSlot)
	}
	m.slots[flowMemoSlotIndex(flow, key)] = flowMemoSlot{
		flow:       flow,
		key:        key,
		t:          t,
		height:     height,
		epoch:      epoch,
		countReset: countReset,
	}
}

func isFlowMemoCheckpoint(flow *ast.FlowNode) bool {
	return (uint64(uintptr(unsafe.Pointer(flow)))*0x9E37_79B9_7F4A_7C15)>>62 == 0
}

func (c *Checker) flowMemoCreated() uint32 {
	return c.TypeCount + c.SymbolCount + c.SignatureCount
}

func (c *Checker) flowFrameBegin() flowFrame {
	return c.flowMemo.begin(
		c.instantiationCount,
		c.TotalInstantiationCount,
		c.flowMemoCreated(),
		len(c.activeMappers) != 0,
	)
}

func (c *Checker) flowFrameCounters(frame flowFrame) {
	if c.TotalInstantiationCount != frame.totalInstantiationCount ||
		frame.inInstantiation ||
		c.flowMemoCreated() != frame.created {
		c.flowMemo.addFlags(flowMemoFlagEffects)
	}
	if frame.instantiationCount == 0 && c.instantiationCount != frame.instantiationCount {
		c.flowMemo.addFlags(flowMemoFlagCountReset)
	}
}

func (c *Checker) flowFrameEnd(frame flowFrame) (frameTaint, uint16) {
	c.flowFrameCounters(frame)
	return c.flowMemo.end(frame)
}

func (c *Checker) flowFrameEndWith(frame flowFrame, height uint16) frameTaint {
	c.flowFrameCounters(frame)
	return c.flowMemo.endWith(frame, height)
}

func (c *Checker) flowMemoConsume(transient, reference uint32) {
	if transient != flowMemoUnTainted {
		c.flowMemoConsumeTransient(transient)
	}
	if reference != flowMemoUnTainted {
		c.flowMemo.taintReference(reference)
	}
}

func (c *Checker) flowMemoConsumeTransient(source uint32) {
	live := false
	if source != 0 {
		for i := range c.flowLoopStack {
			if c.flowLoopStack[i].serial == source {
				live = true
				break
			}
		}
		if !live {
			for i := range c.typeResolutions {
				if c.typeResolutions[i].serial == source {
					live = true
					break
				}
			}
		}
	}
	if live {
		c.flowMemo.taint(source)
	} else {
		c.flowMemo.taint(0)
	}
}

// flowMemoKey computes the walk's memo key once per walk: a packed or hashed representation of
// the reference's structure (root symbol or 'this', the property names of a property access chain),
// the declared and initial types, and the flow container. Only identifiers, 'this', and chains of
// plain property accesses on them get one. Computing it has no side effects: it reads a resolved
// symbol only if one is already cached and assigns no node or symbol IDs.
func (c *Checker) flowMemoKey(f *FlowState) (CacheHashKey, bool) {
	switch f.memoKeyState {
	case 1:
		return CacheHashKey{}, false
	case 2:
		return f.memoKey, true
	}
	key, ok := c.computeFlowMemoKey(f)
	if ok {
		f.memoKey = key
		f.memoKeyState = 2
		return key, true
	}
	f.memoKeyState = 1
	return CacheHashKey{}, false
}

func flowContainerAddr(n *ast.Node) uint64 {
	if n == nil {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&n.Flags)))
}

func (c *Checker) computeFlowMemoKey(f *FlowState) (CacheHashKey, bool) {
	ref := f.reference
	declared := uint32(f.declaredType.id)
	initial := uint32(f.initialType.id)
	switch ref.Kind {
	case ast.KindIdentifier:
		sym := c.flowMemoRootSymbol(ref)
		if sym == nil {
			return CacheHashKey{}, false
		}
		if declared != 0 && initial < (1<<31) {
			p := uint64(uintptr(unsafe.Pointer(sym)))
			if f.flowContainer == nil {
				return CacheHashKey{
					Hi: uint64(declared) | (uint64(initial) << 32),
					Lo: p,
				}, true
			}
			fc := flowContainerAddr(f.flowContainer) * 0x9E37_79B9_7F4A_7C15
			return CacheHashKey{
				Hi: uint64(declared) | (uint64(initial) << 32),
				Lo: p ^ bits.RotateLeft64(fc, 31),
			}, true
		}
		var b [25]byte
		b[0] = 'i'
		p := uint64(uintptr(unsafe.Pointer(sym)))
		fc := flowContainerAddr(f.flowContainer)
		b[1], b[2], b[3], b[4], b[5], b[6], b[7], b[8] = byte(p), byte(p>>8), byte(p>>16), byte(p>>24), byte(p>>32), byte(p>>40), byte(p>>48), byte(p>>56)
		b[9], b[10], b[11], b[12] = byte(declared), byte(declared>>8), byte(declared>>16), byte(declared>>24)
		b[13], b[14], b[15], b[16] = byte(initial), byte(initial>>8), byte(initial>>16), byte(initial>>24)
		b[17], b[18], b[19], b[20], b[21], b[22], b[23], b[24] = byte(fc), byte(fc>>8), byte(fc>>16), byte(fc>>24), byte(fc>>32), byte(fc>>40), byte(fc>>48), byte(fc>>56)
		h := xxh3.Hash128(b[:])
		h.Hi |= 1 << 63
		return CacheHashKey(h), true
	case ast.KindThisKeyword:
		if declared != 0 && initial < (1<<31) {
			if f.flowContainer == nil {
				return CacheHashKey{
					Hi: uint64(declared) | (uint64(initial) << 32),
					Lo: 0,
				}, true
			}
			fc := flowContainerAddr(f.flowContainer) * 0x9E37_79B9_7F4A_7C15
			return CacheHashKey{
				Hi: uint64(declared) | (uint64(initial) << 32),
				Lo: bits.RotateLeft64(fc, 31),
			}, true
		}
		var b [17]byte
		b[0] = 't'
		fc := flowContainerAddr(f.flowContainer)
		b[1], b[2], b[3], b[4] = byte(declared), byte(declared>>8), byte(declared>>16), byte(declared>>24)
		b[5], b[6], b[7], b[8] = byte(initial), byte(initial>>8), byte(initial>>16), byte(initial>>24)
		b[9], b[10], b[11], b[12], b[13], b[14], b[15], b[16] = byte(fc), byte(fc>>8), byte(fc>>16), byte(fc>>24), byte(fc>>32), byte(fc>>40), byte(fc>>48), byte(fc>>56)
		h := xxh3.Hash128(b[:])
		h.Hi |= 1 << 63
		return CacheHashKey(h), true
	}
	return c.computeHashedFlowMemoKey(f)
}

func (c *Checker) computeHashedFlowMemoKey(f *FlowState) (CacheHashKey, bool) {
	buf := c.flowMemo.keyScratch[:0]
	var ok bool
	buf, ok = c.writeFlowMemoReference(buf, f.reference)
	if !ok {
		c.flowMemo.keyScratch = buf[:0]
		return CacheHashKey{}, false
	}
	d := uint32(f.declaredType.id)
	in := uint32(f.initialType.id)
	fc := flowContainerAddr(f.flowContainer)
	buf = append(buf,
		byte(d), byte(d>>8), byte(d>>16), byte(d>>24),
		byte(in), byte(in>>8), byte(in>>16), byte(in>>24),
		byte(fc), byte(fc>>8), byte(fc>>16), byte(fc>>24),
		byte(fc>>32), byte(fc>>40), byte(fc>>48), byte(fc>>56),
	)
	h := xxh3.Hash128(buf)
	c.flowMemo.keyScratch = buf[:0]
	h.Hi |= 1 << 63
	return CacheHashKey(h), true
}

func (c *Checker) flowMemoRootSymbol(node *ast.Node) *ast.Symbol {
	links := c.symbolNodeLinks.TryGet(node)
	if links == nil || links.resolvedSymbol == nil {
		return nil
	}
	sym := links.resolvedSymbol
	if sym == c.unknownSymbol || (node.AsIdentifier().Text == "this" && ast.IsThisInTypeQuery(node)) {
		return nil
	}
	return sym
}

func (c *Checker) writeFlowMemoReference(buf []byte, node *ast.Node) ([]byte, bool) {
	switch node.Kind {
	case ast.KindIdentifier:
		sym := c.flowMemoRootSymbol(node)
		if sym == nil {
			return buf, false
		}
		p := uint64(uintptr(unsafe.Pointer(sym)))
		buf = append(buf, 'i',
			byte(p), byte(p>>8), byte(p>>16), byte(p>>24),
			byte(p>>32), byte(p>>40), byte(p>>48), byte(p>>56),
		)
		return buf, true
	case ast.KindThisKeyword:
		buf = append(buf, 't')
		return buf, true
	case ast.KindParenthesizedExpression, ast.KindNonNullExpression:
		return c.writeFlowMemoReference(buf, node.Expression())
	case ast.KindPropertyAccessExpression:
		var ok bool
		buf, ok = c.writeFlowMemoReference(buf, node.Expression())
		if !ok {
			return buf, false
		}
		if node.Flags&ast.NodeFlagsOptionalChain != 0 {
			buf = append(buf, 'p', '?')
		} else {
			buf = append(buf, 'p')
		}
		name := node.Name().Text()
		n := uint32(len(name))
		buf = append(buf, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
		buf = append(buf, name...)
		return buf, true
	default:
		return buf, false
	}
}

func (c *Checker) flowMemoConsultOk(f *FlowState, hit flowMemoHit) bool {
	cacheOk := hit.epoch == 0 || hit.epoch == c.flowMemo.typeCacheEpoch
	countersOk := len(c.activeMappers) == 0 && (!hit.countReset || c.instantiationCount == 0)
	return !f.memoFaithful && len(c.flowLoopStack) == 0 && cacheOk && countersOk && f.impureShared == 0
}

func (c *Checker) flowMemoAtNode(f *FlowState, entryFlow, flow *ast.FlowNode, flags ast.FlowFlags, key CacheHashKey, entryDepth int) (flowMemoHit, bool, bool) {
	endsIteration := flags&(ast.FlowFlagsCondition|ast.FlowFlagsSwitchClause|ast.FlowFlagsArrayMutation|ast.FlowFlagsStart) != 0 ||
		(flags&(ast.FlowFlagsBranchLabel|ast.FlowFlagsLoopLabel) != 0 && flow.Antecedents() != nil && flow.Antecedents().Next != nil)
	if !endsIteration && flow != entryFlow {
		if !isFlowMemoCheckpoint(flow) {
			return flowMemoHit{}, false, false
		}
		c.flowMemo.checkpoints = append(c.flowMemo.checkpoints, flow)
	}
	hit, ok := c.flowMemoLookup(f, flow, key, entryDepth)
	if !ok {
		return flowMemoHit{}, false, false
	}
	return hit, endsIteration, true
}

func (c *Checker) flowMemoLookup(f *FlowState, flow *ast.FlowNode, key CacheHashKey, entryDepth int) (flowMemoHit, bool) {
	hit, ok := c.flowMemo.lookup(flow, key)
	if !ok {
		return flowMemoHit{}, false
	}
	if !c.flowMemoConsultOk(f, hit) {
		return flowMemoHit{}, false
	}
	if entryDepth+int(hit.height) >= flowDepthLimit {
		return flowMemoHit{}, false
	}
	return hit, true
}

func (c *Checker) flowMemoFill(f *FlowState, memo frameMemo, entryFlow, finalFlow *ast.FlowNode, t FlowType, taint frameTaint, height uint16, steps uint32, hasHit bool) {
	if !taint.isPure() || taint.flags&flowMemoFlagEffects != 0 {
		return
	}
	if t.incomplete || c.flowAnalysisDisabled {
		return
	}
	if height == 0 && steps < 4 && !hasHit {
		return
	}
	epoch := uint32(0)
	if taint.flags&flowMemoFlagTypeCache != 0 {
		epoch = c.flowMemo.typeCacheEpoch
	}
	countReset := taint.flags&flowMemoFlagCountReset != 0
	if entryFlow != finalFlow || !hasHit {
		c.flowMemo.store(entryFlow, memo.key, t.t, height, epoch, countReset)
	}
	if finalFlow != entryFlow && !hasHit {
		c.flowMemo.store(finalFlow, memo.key, t.t, height, epoch, countReset)
	}
	for i := int(memo.checkpoints); i < len(c.flowMemo.checkpoints); i++ {
		checkpoint := c.flowMemo.checkpoints[i]
		if checkpoint != finalFlow {
			c.flowMemo.store(checkpoint, memo.key, t.t, height, epoch, countReset)
		}
	}
}

func (c *Checker) takeFlowTypeCache() savedFlowTypeCache {
	saved := savedFlowTypeCache{
		cache: c.flowTypeCache,
		epoch: c.flowMemo.typeCacheEpoch,
	}
	c.flowTypeCache = nil
	c.flowMemo.typeCacheEpoch = c.flowMemo.nextSerial()
	return saved
}

func (c *Checker) restoreFlowTypeCache(saved savedFlowTypeCache) {
	c.flowTypeCache = saved.cache
	c.flowMemo.typeCacheEpoch = saved.epoch
}
