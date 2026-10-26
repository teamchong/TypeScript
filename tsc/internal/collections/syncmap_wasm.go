//go:build wasm

package collections

import (
	"iter"
	"maps"
)

type SyncMap[K comparable, V any] struct {
	_ [0]K
	_ [0]V
	m map[K]V
}

func (s *SyncMap[K, V]) Load(key K) (value V, ok bool) {
	if s.m == nil {
		return value, false
	}
	val, ok := s.m[key]
	return val, ok
}

func (s *SyncMap[K, V]) Store(key K, value V) {
	if s.m == nil {
		s.m = make(map[K]V)
	}
	s.m[key] = value
}

func (s *SyncMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	if s.m == nil {
		s.m = make(map[K]V)
	} else if existing, ok := s.m[key]; ok {
		return existing, true
	}
	s.m[key] = value
	return value, false
}

func (s *SyncMap[K, V]) Delete(key K) {
	if s.m != nil {
		delete(s.m, key)
	}
}

func (s *SyncMap[K, V]) Clear() {
	if s.m != nil {
		clear(s.m)
	}
}

func (s *SyncMap[K, V]) Range(f func(key K, value V) bool) {
	for k, v := range s.m {
		if !f(k, v) {
			break
		}
	}
}

func (s *SyncMap[K, V]) Size() int {
	return len(s.m)
}

func (s *SyncMap[K, V]) ToMap() map[K]V {
	if s.m == nil {
		return make(map[K]V)
	}
	return maps.Clone(s.m)
}

func (s *SyncMap[K, V]) Keys() iter.Seq[K] {
	return func(yield func(K) bool) {
		for k := range s.m {
			if !yield(k) {
				return
			}
		}
	}
}

func (s *SyncMap[K, V]) Clone() *SyncMap[K, V] {
	if s.m == nil {
		return &SyncMap[K, V]{}
	}
	return &SyncMap[K, V]{m: maps.Clone(s.m)}
}
