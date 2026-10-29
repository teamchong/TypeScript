package sync

import (
	"internal/task"
	_ "unsafe"
)

//go:linkname poolEpoch runtime.PoolEpoch
func poolEpoch() uint32

// Pool is a very simple implementation of sync.Pool.
type Pool struct {
	lock  task.PMutex
	New   func() interface{}
	items []interface{}
	epoch uint32
}

// Get returns an item in the pool, or the value of calling Pool.New() if there
// are no items.
func (p *Pool) Get() interface{} {
	p.lock.Lock()
	ep := poolEpoch()
	if p.epoch != ep {
		p.items = nil
		p.epoch = ep
	}
	if len(p.items) > 0 {
		x := p.items[len(p.items)-1]
		p.items = p.items[:len(p.items)-1]
		p.lock.Unlock()
		return x
	}
	p.lock.Unlock()
	if p.New != nil {
		return p.New()
	}
	return nil
}

// Put adds a value back into the pool.
func (p *Pool) Put(x interface{}) {
	if x == nil {
		return
	}
	p.lock.Lock()
	ep := poolEpoch()
	if p.epoch != ep {
		p.items = nil
		p.epoch = ep
	}
	p.items = append(p.items, x)
	p.lock.Unlock()
}
