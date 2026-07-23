package popcorn

import "sync/atomic"

// moduleStateStore stores a [ModuleState] atomically.
type moduleStateStore atomic.Int32

func (s *moduleStateStore) asAtomic() *atomic.Int32 {
	return (*atomic.Int32)(s)
}

// Set stores the given state.
func (s *moduleStateStore) Set(state ModuleState) {
	s.asAtomic().Store(state.asInt())
}

// CAS compares the current state with from and swaps it to to if equal.
func (s *moduleStateStore) CAS(from, to ModuleState) bool {
	return s.asAtomic().CompareAndSwap(from.asInt(), to.asInt())
}

// Get returns the current state.
func (s *moduleStateStore) Get() ModuleState {
	return ModuleState(s.asAtomic().Load())
}
