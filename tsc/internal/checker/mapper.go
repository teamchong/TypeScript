package checker

import (
	"slices"
	"unsafe"

	"github.com/microsoft/TypeScript/tsc/internal/core"
)

// TypeMapper

type TypeMapper struct {
	kind         TypeMapperKind
	hasInference bool
}
type TypeMapperKind uint8

const (
	TypeMapperKindUnknown TypeMapperKind = iota
	TypeMapperKindSimple
	TypeMapperKindArray
	TypeMapperKindArrayToSingle
	TypeMapperKindDeferred
	TypeMapperKindFunction
	TypeMapperKindMerged
	TypeMapperKindComposite
	TypeMapperKindInference
)

// Each concrete mapper embeds the immutable kind header at offset zero.
func castTypeMapper[T any](m *TypeMapper, kind TypeMapperKind) *T {
	if m.kind != kind {
		panic("Incorrect type mapper kind")
	}
	return (*T)(unsafe.Pointer(m))
}

var (
	_ [0 - unsafe.Offsetof(SimpleTypeMapper{}.TypeMapperBase)]byte
	_ [0 - unsafe.Offsetof(ArrayTypeMapper{}.TypeMapperBase)]byte
	_ [0 - unsafe.Offsetof(ArrayToSingleTypeMapper{}.TypeMapperBase)]byte
	_ [0 - unsafe.Offsetof(DeferredTypeMapper{}.TypeMapperBase)]byte
	_ [0 - unsafe.Offsetof(FunctionTypeMapper{}.TypeMapperBase)]byte
	_ [0 - unsafe.Offsetof(MergedTypeMapper{}.TypeMapperBase)]byte
	_ [0 - unsafe.Offsetof(CompositeTypeMapper{}.TypeMapperBase)]byte
	_ [0 - unsafe.Offsetof(InferenceTypeMapper{}.TypeMapperBase)]byte
	_ [0 - unsafe.Offsetof(TypeMapperBase{}.TypeMapper)]byte
)

func (m *TypeMapper) Map(t *Type) *Type {
	switch m.kind {
	case TypeMapperKindSimple:
		s := (*SimpleTypeMapper)(unsafe.Pointer(m))
		if t == s.source {
			return s.target
		}
		return t
	case TypeMapperKindArray:
		a := (*ArrayTypeMapper)(unsafe.Pointer(m))
		if h := a.hint; h < a.sourcesLen && *(**Type)(unsafe.Add(unsafe.Pointer(a.sourcesPtr), uintptr(h)*unsafe.Sizeof((*Type)(nil)))) == t {
			a.hint = h + 1
			return *(**Type)(unsafe.Add(unsafe.Pointer(a.targetsPtr), uintptr(h)*unsafe.Sizeof((*Type)(nil))))
		}
		return a.Map(t)
	default:
		return m.mapSlow(t)
	}
}

func (m *TypeMapper) mapSlow(t *Type) *Type {
	switch m.kind {
	case TypeMapperKindSimple:
		return (*SimpleTypeMapper)(unsafe.Pointer(m)).Map(t)
	case TypeMapperKindArray:
		return (*ArrayTypeMapper)(unsafe.Pointer(m)).Map(t)
	case TypeMapperKindArrayToSingle:
		return (*ArrayToSingleTypeMapper)(unsafe.Pointer(m)).Map(t)
	case TypeMapperKindDeferred:
		return (*DeferredTypeMapper)(unsafe.Pointer(m)).Map(t)
	case TypeMapperKindFunction:
		return (*FunctionTypeMapper)(unsafe.Pointer(m)).Map(t)
	case TypeMapperKindMerged:
		return (*MergedTypeMapper)(unsafe.Pointer(m)).Map(t)
	case TypeMapperKindComposite:
		return (*CompositeTypeMapper)(unsafe.Pointer(m)).Map(t)
	case TypeMapperKindInference:
		return (*InferenceTypeMapper)(unsafe.Pointer(m)).Map(t)
	default:
		panic("Invalid type mapper kind")
	}
}

func (m *TypeMapper) Kind() TypeMapperKind { return m.kind }

func (m *TypeMapper) MapsThisOnly() bool {
	switch m.kind {
	case TypeMapperKindSimple:
		return castTypeMapper[SimpleTypeMapper](m, TypeMapperKindSimple).MapsThisOnly()
	case TypeMapperKindArray:
		return castTypeMapper[ArrayTypeMapper](m, TypeMapperKindArray).MapsThisOnly()
	case TypeMapperKindArrayToSingle:
		return castTypeMapper[ArrayToSingleTypeMapper](m, TypeMapperKindArrayToSingle).MapsThisOnly()
	case TypeMapperKindDeferred:
		return castTypeMapper[DeferredTypeMapper](m, TypeMapperKindDeferred).MapsThisOnly()
	default:
		return false
	}
}

// Factory functions

func getMappedType(t *Type, mapper *TypeMapper) *Type {
	return mapper.Map(getNonDistributedTypeParameter(t))
}

func newTypeMapper(sources []*Type, targets []*Type) *TypeMapper {
	if len(sources) == 1 {
		return newSimpleTypeMapper(sources[0], targets[0])
	}
	return newArrayTypeMapper(sources, targets)
}

func (c *Checker) combineTypeMappers(m1 *TypeMapper, m2 *TypeMapper) *TypeMapper {
	if m1 != nil {
		return newCompositeTypeMapper(c, m1, m2)
	}
	return m2
}

func (c *Checker) mapTypeWithCompositeMapper(t *Type, m1 *TypeMapper, m2 *TypeMapper) *Type {
	if m1 == nil {
		return getMappedType(t, m2)
	}
	t1 := getMappedType(t, m1)
	if t1 != t {
		return c.instantiateType(t1, m2)
	}
	return getMappedType(t, m2)
}

func mergeTypeMappers(m1 *TypeMapper, m2 *TypeMapper) *TypeMapper {
	if m1 != nil {
		return newMergedTypeMapper(m1, m2)
	}
	return m2
}

func prependTypeMapping(source *Type, target *Type, mapper *TypeMapper) *TypeMapper {
	tp := getNonDistributedTypeParameter(source)
	if mapper == nil {
		return newSimpleTypeMapper(tp, target)
	}
	if mapper.kind == TypeMapperKindSimple {
		s := castTypeMapper[SimpleTypeMapper](mapper, TypeMapperKindSimple)
		if s.source == tp {
			if s.target == target && target != tp {
				return mapper
			}
			if target.flags&TypeFlagsTypeParameter == 0 {
				return newSimpleTypeMapper(tp, target)
			}
		}
	}
	return newMergedTypeMapper(newSimpleTypeMapper(tp, target), mapper)
}

func appendTypeMapping(mapper *TypeMapper, source *Type, target *Type) *TypeMapper {
	if mapper == nil {
		return newSimpleTypeMapper(getNonDistributedTypeParameter(source), target)
	}
	return newMergedTypeMapper(mapper, newSimpleTypeMapper(getNonDistributedTypeParameter(source), target))
}

// Maps forward-references to later types parameters to the empty object type.
// This is used during inference when instantiating type parameter defaults.
func (c *Checker) newBackreferenceMapper(context *InferenceContext, index int) *TypeMapper {
	forwardInferences := context.inferences[index:]
	typeParameters := core.Map(forwardInferences, func(i *InferenceInfo) *Type {
		return i.typeParameter
	})
	return newArrayToSingleTypeMapper(typeParameters, c.unknownType)
}

// TypeMapperBase

type TypeMapperBase struct {
	TypeMapper
}

func (m *TypeMapperBase) Map(t *Type) *Type  { return t }
func (m *TypeMapperBase) MapsThisOnly() bool { return false }

// SimpleTypeMapper

type SimpleTypeMapper struct {
	TypeMapperBase
	source *Type
	target *Type
}

func newSimpleTypeMapper(source *Type, target *Type) *TypeMapper {
	if source != nil && source.checker != nil {
		m := source.checker.simpleMapperArena.New()
		m.kind = TypeMapperKindSimple
		m.source = source
		m.target = target
		return &m.TypeMapper
	}
	m := &SimpleTypeMapper{}
	m.kind = TypeMapperKindSimple
	m.source = source
	m.target = target
	return &m.TypeMapper
}

func (m *SimpleTypeMapper) Map(t *Type) *Type {
	if t == m.source {
		return m.target
	}
	return t
}

func (m *SimpleTypeMapper) MapsThisOnly() bool {
	return isThisTypeParameter(m.source)
}

// ArrayTypeMapper

type ArrayTypeMapper struct {
	TypeMapperBase
	sourcesLen uint16
	targetsLen uint16
	hint       uint16
	sourcesPtr **Type
	targetsPtr **Type
}

func newArrayTypeMapper(sources []*Type, targets []*Type) *TypeMapper {
	var sourcesPtr, targetsPtr **Type
	if len(sources) > 0 {
		sourcesPtr = unsafe.SliceData(sources)
	}
	if len(targets) > 0 {
		targetsPtr = unsafe.SliceData(targets)
	}
	if len(sources) > 0 && sources[0] != nil && sources[0].checker != nil {
		m := sources[0].checker.arrayMapperArena.New()
		m.kind = TypeMapperKindArray
		m.sourcesLen = uint16(len(sources))
		m.targetsLen = uint16(len(targets))
		m.sourcesPtr = sourcesPtr
		m.targetsPtr = targetsPtr
		return &m.TypeMapper
	}
	m := &ArrayTypeMapper{}
	m.kind = TypeMapperKindArray
	m.sourcesLen = uint16(len(sources))
	m.targetsLen = uint16(len(targets))
	m.sourcesPtr = sourcesPtr
	m.targetsPtr = targetsPtr
	return &m.TypeMapper
}

func (m *ArrayTypeMapper) sources() []*Type {
	if m.sourcesLen == 0 {
		return nil
	}
	return unsafe.Slice(m.sourcesPtr, m.sourcesLen)
}

func (m *ArrayTypeMapper) targets() []*Type {
	if m.targetsLen == 0 {
		return nil
	}
	return unsafe.Slice(m.targetsPtr, m.targetsLen)
}

func (m *ArrayTypeMapper) Map(t *Type) *Type {
	n := int(m.sourcesLen)
	if n == 0 {
		return t
	}
	sources := unsafe.Slice(m.sourcesPtr, n)
	h := int(m.hint)
	if h >= n {
		h = 0
	}
	for i := h; i < n; i++ {
		if sources[i] == t {
			m.hint = uint16(i + 1)
			return unsafe.Slice(m.targetsPtr, m.targetsLen)[i]
		}
	}
	for i := range h {
		if sources[i] == t {
			m.hint = uint16(i + 1)
			return unsafe.Slice(m.targetsPtr, m.targetsLen)[i]
		}
	}
	return t
}

func (m *ArrayTypeMapper) MapsThisOnly() bool {
	return m.sourcesLen == 1 && isThisTypeParameter(*m.sourcesPtr)
}

// ArrayToSingleTypeMapper

type ArrayToSingleTypeMapper struct {
	TypeMapperBase
	sources []*Type
	target  *Type
}

func newArrayToSingleTypeMapper(sources []*Type, target *Type) *TypeMapper {
	m := &ArrayToSingleTypeMapper{}
	m.kind = TypeMapperKindArrayToSingle
	m.sources = sources
	m.target = target
	return &m.TypeMapper
}

func (m *ArrayToSingleTypeMapper) Map(t *Type) *Type {
	if slices.Contains(m.sources, t) {
		return m.target
	}
	return t
}

func (m *ArrayToSingleTypeMapper) MapsThisOnly() bool {
	return len(m.sources) == 1 && isThisTypeParameter(m.sources[0])
}

// DeferredTypeMapper

type DeferredTypeMapper struct {
	TypeMapperBase
	sources []*Type
	targets []func() *Type
}

func newDeferredTypeMapper(sources []*Type, targets []func() *Type) *TypeMapper {
	m := &DeferredTypeMapper{}
	m.kind = TypeMapperKindDeferred
	m.sources = sources
	m.targets = targets
	return &m.TypeMapper
}

func (m *DeferredTypeMapper) Map(t *Type) *Type {
	for i, s := range m.sources {
		if t == s {
			return m.targets[i]()
		}
	}
	return t
}

func (m *DeferredTypeMapper) MapsThisOnly() bool {
	return len(m.sources) == 1 && isThisTypeParameter(m.sources[0])
}

// FunctionTypeMapper

type FunctionTypeMapper struct {
	TypeMapperBase
	fn func(*Type) *Type
}

func newFunctionTypeMapper(fn func(*Type) *Type) *TypeMapper {
	m := &FunctionTypeMapper{}
	m.kind = TypeMapperKindFunction
	m.fn = fn
	return &m.TypeMapper
}

func (m *FunctionTypeMapper) Map(t *Type) *Type {
	return m.fn(t)
}

// MergedTypeMapper

type MergedTypeMapper struct {
	TypeMapperBase
	m1 *TypeMapper
	m2 *TypeMapper
}

func newMergedTypeMapper(m1 *TypeMapper, m2 *TypeMapper) *TypeMapper {
	var c *Checker
	if m1 != nil && m1.kind == TypeMapperKindSimple {
		if s := castTypeMapper[SimpleTypeMapper](m1, TypeMapperKindSimple).source; s != nil {
			c = s.checker
		}
	} else if m2 != nil && m2.kind == TypeMapperKindSimple {
		if s := castTypeMapper[SimpleTypeMapper](m2, TypeMapperKindSimple).source; s != nil {
			c = s.checker
		}
	} else if m1 != nil && m1.kind == TypeMapperKindComposite {
		c = castTypeMapper[CompositeTypeMapper](m1, TypeMapperKindComposite).c
	}
	hasInference := m1 != nil && m1.hasInference || m2 != nil && m2.hasInference
	if c != nil {
		m := c.mergedMapperArena.New()
		m.kind = TypeMapperKindMerged
		m.hasInference = hasInference
		m.m1 = m1
		m.m2 = m2
		return &m.TypeMapper
	}
	m := &MergedTypeMapper{}
	m.kind = TypeMapperKindMerged
	m.hasInference = hasInference
	m.m1 = m1
	m.m2 = m2
	return &m.TypeMapper
}

func (m *MergedTypeMapper) Map(t *Type) *Type {
	return m.m2.Map(m.m1.Map(t))
}

// CompositeTypeMapper

type CompositeTypeMapper struct {
	TypeMapperBase
	c  *Checker
	m1 *TypeMapper
	m2 *TypeMapper
}

func newCompositeTypeMapper(c *Checker, m1 *TypeMapper, m2 *TypeMapper) *TypeMapper {
	hasInference := m1 != nil && m1.hasInference || m2 != nil && m2.hasInference
	if c != nil {
		m := c.compositeMapperArena.New()
		m.kind = TypeMapperKindComposite
		m.hasInference = hasInference
		m.c = c
		m.m1 = m1
		m.m2 = m2
		return &m.TypeMapper
	}
	m := &CompositeTypeMapper{}
	m.kind = TypeMapperKindComposite
	m.hasInference = hasInference
	m.c = c
	m.m1 = m1
	m.m2 = m2
	return &m.TypeMapper
}

func (m *CompositeTypeMapper) Map(t *Type) *Type {
	t1 := m.m1.Map(t)
	if t1 != t {
		if t1.flags&(TypeFlagsInstantiable|TypeFlagsObject|TypeFlagsUnionOrIntersection) == 0 {
			return t1
		}
		return m.c.instantiateType(t1, m.m2)
	}
	return m.m2.Map(t)
}

// InferenceTypeMapper

type InferenceTypeMapper struct {
	TypeMapperBase
	fixing bool
	hint   int
	c      *Checker
	n      *InferenceContext
}

func (c *Checker) initInferenceTypeMapper(m *InferenceTypeMapper, n *InferenceContext, fixing bool) *TypeMapper {
	m.kind = TypeMapperKindInference
	m.hasInference = true
	m.fixing = fixing
	m.hint = 0
	m.c = c
	m.n = n
	return &m.TypeMapper
}

func markMapperEscaped(m *TypeMapper) {
	if m != nil && m.hasInference {
		markMapperEscapedSlow(m)
	}
}

func markMapperEscapedSlow(m *TypeMapper) {
	m.hasInference = false
	switch m.kind {
	case TypeMapperKindInference:
		castTypeMapper[InferenceTypeMapper](m, TypeMapperKindInference).n.escaped = true
	case TypeMapperKindMerged:
		merged := castTypeMapper[MergedTypeMapper](m, TypeMapperKindMerged)
		if merged.m1 != nil && merged.m1.hasInference {
			markMapperEscapedSlow(merged.m1)
		}
		if merged.m2 != nil && merged.m2.hasInference {
			markMapperEscapedSlow(merged.m2)
		}
	case TypeMapperKindComposite:
		comp := castTypeMapper[CompositeTypeMapper](m, TypeMapperKindComposite)
		if comp.m1 != nil && comp.m1.hasInference {
			markMapperEscapedSlow(comp.m1)
		}
		if comp.m2 != nil && comp.m2.hasInference {
			markMapperEscapedSlow(comp.m2)
		}
	}
}

func (m *InferenceTypeMapper) Map(t *Type) *Type {
	inferences := m.n.inferences
	n := len(inferences)
	if n == 0 {
		return t
	}
	if n == 1 {
		inference := inferences[0]
		if t == inference.typeParameter {
			if m.fixing && !inference.isFixed {
				m.c.inferFromIntraExpressionSites(m.n)
				clearCachedInferences(inferences)
				inference.isFixed = true
			}
			return m.c.getInferredType(m.n, 0)
		}
		return t
	}
	h := m.hint
	if h >= n {
		h = 0
	}
	for i := h; i < n; i++ {
		inference := inferences[i]
		if t == inference.typeParameter {
			m.hint = i + 1
			if m.fixing && !inference.isFixed {
				m.c.inferFromIntraExpressionSites(m.n)
				clearCachedInferences(inferences)
				inference.isFixed = true
			}
			return m.c.getInferredType(m.n, i)
		}
	}
	for i := range h {
		inference := inferences[i]
		if t == inference.typeParameter {
			m.hint = i + 1
			if m.fixing && !inference.isFixed {
				m.c.inferFromIntraExpressionSites(m.n)
				clearCachedInferences(inferences)
				inference.isFixed = true
			}
			return m.c.getInferredType(m.n, i)
		}
	}
	return t
}
