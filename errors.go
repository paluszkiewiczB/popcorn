package popcorn

import (
	"errors"
	"fmt"
)

var (
	// ErrNilModule is returned when a nil module is provided.
	ErrNilModule = errors.New("nil module")
	// ErrDuplicateModuleID is returned when two modules share an id.
	ErrDuplicateModuleID = errors.New("duplicate module id")
	// ErrUnknownDependency is returned when a module depends on an unknown module.
	ErrUnknownDependency = errors.New("module depends on unknown module")
	// ErrSelfDependency is returned when a module depends on itself.
	ErrSelfDependency = errors.New("module depends on itself")
	// ErrDuplicateDependency is returned when a module lists a dependency twice.
	ErrDuplicateDependency = errors.New("duplicate dependency")
	// ErrCircularDependency is returned when module dependencies form a cycle.
	ErrCircularDependency = errors.New("circular dependency")
	// ErrModuleIDNotSet is returned when a module has an empty id.
	ErrModuleIDNotSet = errors.New("module id not set")
	// ErrModuleIDReserved is returned when a module uses a reserved id.
	ErrModuleIDReserved = errors.New("module id is reserved by the kernel")
	// ErrModuleStartNotSet is returned when a recipe has no Start function.
	ErrModuleStartNotSet = errors.New("start function not set for module")
	// ErrKernelStarted is returned by Start when called on an already-started kernel.
	ErrKernelStarted = errors.New("kernel already started")
	// ErrKernelStopped is returned (wrapped) by Start on a graceful shutdown.
	ErrKernelStopped = errors.New("kernel stopped")
)

// KernelUnhealthyError is returned by Kernel.Start when a module reports NOK.
type KernelUnhealthyError struct {
	// ModuleID is the module that reported the unhealthy state.
	ModuleID string
	// Cause is the underlying reason.
	Cause error
}

// Error implements the error interface.
func (e KernelUnhealthyError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("module %q unhealthy", e.ModuleID)
	}
	return fmt.Sprintf("module %q unhealthy: %s", e.ModuleID, e.Cause)
}

// Unwrap returns the cause.
func (e KernelUnhealthyError) Unwrap() error { return e.Cause }

// causeError preserves the health cause for errors.Is and errors.As while also
// matching by message. The contract compares the reported cause against an
// independently constructed error with the same text, so message equality is
// deliberate here, not incidental.
type causeError struct{ err error }

func (w causeError) Error() string { return w.err.Error() }
func (w causeError) Unwrap() error { return w.err }
func (w causeError) Is(target error) bool {
	return target != nil && target.Error() == w.err.Error()
}

func wrapCause(cause error) error {
	if cause == nil {
		return nil
	}
	return causeError{err: cause}
}
