//go:build gc.leaking

package runtime

// This GC implementation is the simplest useful memory allocator possible: it
// only allocates memory and never frees it. For some constrained systems, it
// may be the only memory allocator possible.

import (
	"internal/task"
	"sync/atomic"
	"unsafe"
)

const needsStaticHeap = true

var gcScanState atomic.Uint32

func scanCurrentStack() {}

// Ever-incrementing pointer: no memory is freed.
var (
	heapptr   uintptr
	small4Ptr uintptr
	small4End uintptr
)

// Total amount allocated for runtime.MemStats
var (
	gcTotalAlloc uint64
	gcLiveAlloc  uint64
)

// Total number of calls to alloc()
var (
	gcMallocs uint64
	gcFrees   uint64
)

// Heap lock for parallel goroutines. No-op when single threaded.
var gcLock task.PMutex

var (
	freeLists     [257]unsafe.Pointer
	largeFreePtr  [256]unsafe.Pointer
	largeFreeSize [256]uintptr
)

//go:noinline
func alloc(size uintptr, layout unsafe.Pointer) unsafe.Pointer {
	if size == 0 {
		return unsafe.Pointer(zeroSizeAllocPtr)
	}
	if size <= 4 {
		if small4Ptr == small4End {
			chunk := uintptr(alloc(256, nil))
			small4Ptr = chunk
			small4End = chunk + 256
		} else {
			gcMallocs++
		}
		addr := small4Ptr
		small4Ptr += 4
		return unsafe.Pointer(addr)
	}
	size = align(size)
	if size <= 2048 {
		idx := size >> 3
		if p := freeLists[idx]; p != nil {
			freeLists[idx] = *(*unsafe.Pointer)(p)
			memzero(p, size)
			gcTotalAlloc += uint64(size)
			gcLiveAlloc += uint64(size)
			gcMallocs++
			return p
		}
	} else {
		for i := range 256 {
			if largeFreeSize[i] == size {
				p := largeFreePtr[i]
				largeFreePtr[i] = nil
				largeFreeSize[i] = 0
				memzero(p, size)
				gcTotalAlloc += uint64(size)
				gcLiveAlloc += uint64(size)
				gcMallocs++
				return p
			}
		}
	}
	gcLock.Lock()
	addr := heapptr
	gcTotalAlloc += uint64(size)
	gcLiveAlloc += uint64(size)
	gcMallocs++
	heapptr += size
	if heapptr < addr {
		runtimePanic(errOutOfMemory)
	}
	for heapptr > heapEnd {
		if growHeap() {
			continue
		}
		runtimeFatal("out of memory")
	}
	gcLock.Unlock()

	pointer := unsafe.Pointer(addr)
	if GOOS != "wasip1" && GOARCH != "wasm" {
		zero_new_alloc(pointer, size)
	}
	return pointer
}

func freeSized(ptr unsafe.Pointer, size uintptr) {
	if ptr == nil || size < 8 {
		return
	}
	size = align(size)
	if size <= 2048 {
		idx := size >> 3
		*(*unsafe.Pointer)(ptr) = freeLists[idx]
		freeLists[idx] = ptr
		gcFrees++
		gcLiveAlloc -= uint64(size)
	} else {
		for i := range 256 {
			if largeFreeSize[i] == 0 {
				largeFreePtr[i] = ptr
				largeFreeSize[i] = size
				gcFrees++
				gcLiveAlloc -= uint64(size)
				return
			}
		}
	}
}

func free(ptr unsafe.Pointer) {
}

//go:noinline
func freeTaskStack(ptr uintptr) {
}

func markRoots(start, end uintptr) {
	runtimeFatal("unreachable: markRoots")
}

// ReadMemStats populates m with memory statistics.
//
// The returned memory statistics are up to date as of the
// call to ReadMemStats. This would not do GC implicitly for you.
func ReadMemStats(m *MemStats) {
	gcLock.Lock()

	m.HeapIdle = 0
	m.HeapInuse = gcLiveAlloc
	m.HeapReleased = 0 // always 0, we don't currently release memory back to the OS.

	m.HeapSys = m.HeapInuse + m.HeapIdle
	m.GCSys = 0
	m.TotalAlloc = gcTotalAlloc
	m.Mallocs = gcMallocs
	m.Frees = gcFrees
	m.Sys = uint64(heapEnd - heapStart)
	m.HeapAlloc = gcLiveAlloc
	m.HeapObjects = gcMallocs - gcFrees
	m.Alloc = m.HeapAlloc
	m.NumGC = 0 // this GC never collects, so no cycle ever completes

	gcLock.Unlock()
}

func mallocs() uint64 {
	gcLock.Lock()
	mallocs := gcMallocs
	gcLock.Unlock()
	return mallocs
}

func GC() {
	// No-op.
}

func SetFinalizer(obj interface{}, finalizer interface{}) {
	// No-op.
}

func initHeap() {
	heapptr = heapStart
}

// setHeapEnd sets a new (larger) heapEnd pointer.
func setHeapEnd(newHeapEnd uintptr) {
	// This "heap" is so simple that simply assigning a new value is good
	// enough.
	heapEnd = newHeapEnd
}
