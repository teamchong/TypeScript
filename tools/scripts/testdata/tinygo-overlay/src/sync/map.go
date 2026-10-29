package sync

import (
	"internal/task"
)

// This file implements just enough of sync.Map to get packages to compile. It
// is no more efficient than a map with a lock.

type Map struct {
	lock     task.PMutex
	m        map[interface{}]interface{}
	postMark map[interface{}]interface{}
	epoch    uint32
}

func (m *Map) activeMap() map[interface{}]interface{} {
	ep := poolEpoch()
	if ep <= 1 {
		if m.m == nil {
			m.m = make(map[interface{}]interface{})
		}
		return m.m
	}
	if m.epoch != ep || m.postMark == nil {
		m.postMark = make(map[interface{}]interface{})
		m.epoch = ep
	}
	return m.postMark
}

func (m *Map) Delete(key interface{}) {
	m.lock.Lock()
	defer m.lock.Unlock()
	ep := poolEpoch()
	if ep <= 1 {
		delete(m.m, key)
	} else if m.epoch == ep && m.postMark != nil {
		delete(m.postMark, key)
	}
}

func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.m != nil {
		if value, ok = m.m[key]; ok {
			return value, true
		}
	}
	ep := poolEpoch()
	if ep > 1 && m.epoch == ep && m.postMark != nil {
		value, ok = m.postMark[key]
	}
	return
}

func (m *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.m != nil {
		if existing, ok := m.m[key]; ok {
			return existing, true
		}
	}
	target := m.activeMap()
	if existing, ok := target[key]; ok {
		return existing, true
	}
	target[key] = value
	return value, false
}

func (m *Map) LoadAndDelete(key interface{}) (value interface{}, loaded bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	ep := poolEpoch()
	if ep <= 1 {
		value, ok := m.m[key]
		if !ok {
			return nil, false
		}
		delete(m.m, key)
		return value, true
	}
	if m.epoch == ep && m.postMark != nil {
		if value, ok := m.postMark[key]; ok {
			delete(m.postMark, key)
			return value, true
		}
	}
	return nil, false
}

func (m *Map) Store(key, value interface{}) {
	m.lock.Lock()
	defer m.lock.Unlock()
	target := m.activeMap()
	target[key] = value
}

// Range calls f for each key and value in the map. If f returns false, the iteration stops.
func (m *Map) Range(f func(key, value interface{}) bool) {
	m.lock.Lock()
	keys := make([]interface{}, 0, len(m.m))
	for k := range m.m {
		keys = append(keys, k)
	}
	ep := poolEpoch()
	if ep > 1 && m.epoch == ep {
		for k := range m.postMark {
			if _, ok := m.m[k]; !ok {
				keys = append(keys, k)
			}
		}
	}
	m.lock.Unlock()

	for _, k := range keys {
		if v, ok := m.Load(k); ok {
			if !f(k, v) {
				break
			}
		}
	}
}

func (m *Map) Clear() {
	m.lock.Lock()
	defer m.lock.Unlock()
	ep := poolEpoch()
	if ep <= 1 {
		clear(m.m)
	} else {
		m.postMark = nil
		m.epoch = ep
	}
}

// Swap replaces the value for the given key, and returns the old value if any.
func (m *Map) Swap(key, value any) (previous any, loaded bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.m != nil {
		if prev, ok := m.m[key]; ok {
			previous, loaded = prev, true
		}
	}
	target := m.activeMap()
	if prev, ok := target[key]; ok {
		previous, loaded = prev, true
	}
	target[key] = value
	return
}

// CompareAndSwap swaps the old and new values for an existing key if the value
// stored in the map is equal to old.
func (m *Map) CompareAndSwap(key, old, new any) (swapped bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	ep := poolEpoch()
	if ep <= 1 {
		if m.m == nil {
			return false
		}
		value, ok := m.m[key]
		if !ok || value != old {
			return false
		}
		m.m[key] = new
		return true
	}
	var value any
	var ok bool
	if m.epoch == ep && m.postMark != nil {
		value, ok = m.postMark[key]
	}
	if !ok && m.m != nil {
		value, ok = m.m[key]
	}
	if !ok || value != old {
		return false
	}
	target := m.activeMap()
	target[key] = new
	return true
}

// CompareAndDelete deletes the entry for key if its value is equal to old.
func (m *Map) CompareAndDelete(key, old any) (deleted bool) {
	m.lock.Lock()
	defer m.lock.Unlock()
	ep := poolEpoch()
	if ep <= 1 {
		if m.m == nil {
			return false
		}
		value, ok := m.m[key]
		if !ok || value != old {
			return false
		}
		delete(m.m, key)
		return true
	}
	if m.epoch != ep || m.postMark == nil {
		return false
	}
	value, ok := m.postMark[key]
	if !ok || value != old {
		return false
	}
	delete(m.postMark, key)
	return true
}
