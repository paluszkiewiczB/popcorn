package popcorn

import "sync/atomic"

// ModuleStateStore stores a [ModuleState] atomically.
type ModuleStateStore atomic.Int32

// Set stores the given state.
func (s *ModuleStateStore) Set(state ModuleState) {
	s.asAtomic().Store(state.asInt())
}

// CAS compares the current state with from and swaps it to if equal.
func (s *ModuleStateStore) CAS(from, to ModuleState) bool {
	return s.asAtomic().CompareAndSwap(from.asInt(), to.asInt())
}

// Get returns the current state.
func (s *ModuleStateStore) Get() ModuleState {
	return ModuleState(s.asAtomic().Load())
}

func (s *ModuleStateStore) asAtomic() *atomic.Int32 {
	return (*atomic.Int32)(s)
}
