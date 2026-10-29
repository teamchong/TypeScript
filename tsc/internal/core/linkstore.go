package core

// Links store

type LinkStore[K comparable, V any] struct {
	entries map[K]*V
	arena   Arena[V]
}

func (s *LinkStore[K, V]) Get(key K) *V {
	value := s.entries[key]
	if value != nil {
		return value
	}
	if s.entries == nil {
		s.entries = make(map[K]*V)
	}
	value = s.arena.New()
	s.entries[key] = value
	return value
}

func (s *LinkStore[K, V]) Has(key K) bool {
	_, ok := s.entries[key]
	return ok
}

func (s *LinkStore[K, V]) TryGet(key K) *V {
	return s.entries[key]
}

const (
	LinkPageShift = 6
	LinkPageSize  = 1 << LinkPageShift
	LinkPageMask  = LinkPageSize - 1
)

// PagedLinkStore implements a sparse-array-like structure for storing elements keyed by dense uint64 keys.
// Elements are allocated in an arena, and compressed indexes are stored in fixed-size pages of 256 entries.
// The pages contain no Go pointers, halving their size on 64-bit hosts and avoiding GC scans of each entry.

type PagedLinkStore[V any] struct {
	pages     []*[LinkPageSize]uint32
	pageArena Arena[[LinkPageSize]uint32]
	arena     IndexedArena[V]
}

func (s *PagedLinkStore[V]) Get(key uint64) *V {
	if pageIndex := int(key >> LinkPageShift); pageIndex < len(s.pages) {
		if page := s.pages[pageIndex]; page != nil {
			if index := page[key&LinkPageMask]; index != 0 {
				index--
				return &s.arena.blocks[index>>indexedArenaPageShift][index&((1<<indexedArenaPageShift)-1)]
			}
		}
	}
	return s.getSlow(key)
}

func (s *PagedLinkStore[V]) getSlow(key uint64) *V {
	pageIndex := key >> LinkPageShift
	if int(pageIndex) >= len(s.pages) {
		needed := int(pageIndex) + 1
		if needed <= cap(s.pages) {
			s.pages = s.pages[:needed]
		} else {
			newCap := max(needed, cap(s.pages)*2, 8)
			next := make([]*[LinkPageSize]uint32, needed, newCap)
			copy(next, s.pages)
			s.pages = next
		}
	}
	page := s.pages[pageIndex]
	if page == nil {
		page = s.pageArena.New()
		s.pages[pageIndex] = page
	}
	index := page[key&LinkPageMask]
	if index == 0 {
		id, value := s.arena.New()
		page[key&LinkPageMask] = id
		return value
	}
	return s.arena.Get(index)
}

func (s *PagedLinkStore[V]) Has(key uint64) bool {
	if pageIndex := int(key >> LinkPageShift); pageIndex < len(s.pages) {
		if page := s.pages[pageIndex]; page != nil {
			return page[key&LinkPageMask] != 0
		}
	}
	return false
}

func (s *PagedLinkStore[V]) TryGet(key uint64) *V {
	if pageIndex := int(key >> LinkPageShift); pageIndex < len(s.pages) {
		if page := s.pages[pageIndex]; page != nil {
			if index := page[key&LinkPageMask]; index != 0 {
				index--
				return &s.arena.blocks[index>>indexedArenaPageShift][index&((1<<indexedArenaPageShift)-1)]
			}
		}
	}
	return nil
}

// PagedValueLinkStore stores small value types directly inside fixed-size pages without
// a secondary arena indirection.
type PagedValueLinkStore[V any] struct {
	pages     []*[LinkPageSize]V
	pageArena Arena[[LinkPageSize]V]
}

func (s *PagedValueLinkStore[V]) Get(key uint64) *V {
	if v := s.TryGet(key); v != nil {
		return v
	}
	return s.getSlow(key)
}

func (s *PagedValueLinkStore[V]) getSlow(key uint64) *V {
	pageIndex := key >> LinkPageShift
	if int(pageIndex) >= len(s.pages) {
		needed := int(pageIndex) + 1
		if needed <= cap(s.pages) {
			s.pages = s.pages[:needed]
		} else {
			newCap := max(needed, cap(s.pages)*2, 8)
			next := make([]*[LinkPageSize]V, needed, newCap)
			copy(next, s.pages)
			s.pages = next
		}
	}
	page := s.pages[pageIndex]
	if page == nil {
		page = s.pageArena.New()
		s.pages[pageIndex] = page
	}
	return &page[key&LinkPageMask]
}

func (s *PagedValueLinkStore[V]) TryGet(key uint64) *V {
	if pageIndex := int(key >> LinkPageShift); pageIndex < len(s.pages) {
		if page := s.pages[pageIndex]; page != nil {
			return &page[key&LinkPageMask]
		}
	}
	return nil
}
