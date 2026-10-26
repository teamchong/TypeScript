package core

import (
	"runtime"
	"slices"
	"unsafe"
)

// Arena allocator

type Arena[T any] struct {
	data []T
}

// Allocate a single element at a stable address. Small chunks bound the unused
// capacity and unrelated values retained by a surviving pointer into a chunk.
func (a *Arena[T]) New() *T {
	if len(a.data) == cap(a.data) {
		nextSize := nextArenaSize[T](cap(a.data))
		if runtime.GOARCH == "wasm" {
			a.data = make([]T, 0, nextSize)
		} else {
			// Use the same trick as slices.Concat; Grow rounds up to the next size class.
			a.data = slices.Grow[[]T](nil, nextSize)
		}
	}
	index := len(a.data)
	a.data = a.data[:index+1]
	return &a.data[index]
}

func (a *Arena[T]) Pop(p *T) {
	if n := len(a.data); n > 0 && &a.data[n-1] == p {
		var zero T
		a.data[n-1] = zero
		a.data = a.data[:n-1]
	}
}

// Allocate a slice of the given size in the arena. If the requested size is beyond the capacity of the arena
// and an arena of the next size up still wouldn't fit the slice, make a separate memory allocation for the slice.
// Otherwise, grow the arena if necessary and allocate a slice out of it. The length and capacity of the resulting
// slice are equal to the given size.
func (a *Arena[T]) NewSlice(size int) []T {
	if size == 0 {
		return nil
	}
	if len(a.data)+size > cap(a.data) {
		nextSize := nextArenaSize[T](cap(a.data))
		if size > nextSize || (cap(a.data)-len(a.data) > size && size*2 > nextSize) {
			return make([]T, size)
		}
		if runtime.GOARCH == "wasm" {
			a.data = make([]T, 0, nextSize)
		} else {
			// Use the same trick as slices.Concat; Grow rounds up to the next size class.
			a.data = slices.Grow[[]T](nil, nextSize)
		}
	}
	newLen := len(a.data) + size
	slice := a.data[len(a.data):newLen:newLen]
	a.data = a.data[:newLen]
	return slice
}

func (a *Arena[T]) NewSlice1(t T) []T {
	slice := a.NewSlice(1)
	slice[0] = t
	return slice
}

func (a *Arena[T]) Clone(t []T) []T {
	if len(t) == 0 {
		return nil
	}
	slice := a.NewSlice(len(t))
	copy(slice, t)
	return slice
}

func nextArenaSize[T any](size int) int {
	if runtime.GOARCH == "wasm" {
		maxSize := max(4096/max(int(unsafe.Sizeof(*new(T))), 1), 1)
		size = max(size, 8)
		size = min(size*2, 1024, maxSize)
		return size
	}
	// Target 32 KiB (four Go pages) before allocator size-class rounding, with at
	// least one value. Small arenas start at 8 elements and double up to maxSize.
	maxSize := max(32768/max(int(unsafe.Sizeof(*new(T))), 1), 1)
	size = max(size, 4)
	size = min(size*2, 1024, maxSize)
	return size
}

// IndexedArena keeps values at stable addresses and references them with uint32
// indexes instead of Go pointers. Index zero is nil. Unlike Arena, it retains all
// blocks so that an index alone keeps its value reachable through the arena.
type IndexedArena[T any] struct {
	blocks []*[1 << indexedArenaPageShift]T
	size   uint32
}

const indexedArenaPageShift = 10

func (a *IndexedArena[T]) New() (uint32, *T) {
	if a.size == ^uint32(0) {
		panic("IndexedArena exhausted")
	}
	index := a.size
	if int(index>>indexedArenaPageShift) >= len(a.blocks) {
		a.blocks = append(a.blocks, new([1 << indexedArenaPageShift]T))
	}
	a.size = index + 1
	return index + 1, &a.blocks[index>>indexedArenaPageShift][index&((1<<indexedArenaPageShift)-1)]
}

func (a *IndexedArena[T]) Get(index uint32) *T {
	if index == 0 {
		return nil
	}
	i := index - 1
	return &a.blocks[i>>indexedArenaPageShift][i&((1<<indexedArenaPageShift)-1)]
}
