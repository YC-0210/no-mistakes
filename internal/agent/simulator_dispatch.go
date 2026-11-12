package agent

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testsimulator"
)

type simulatorInvocation struct {
	attempted atomic.Bool
	binding   testsimulator.Binding
}

type simulatorDispatchError struct{ err error }

func (e *simulatorDispatchError) Error() string {
	return fmt.Sprintf("simulator allowance refused after an earlier provider attempt: %v", e.err)
}

func (*simulatorDispatchError) TerminalProviderError() bool { return true }

func withSimulatorInvocation(opts RunOpts) RunOpts {
	if l := opts.TestSimulatorLease; l != nil && opts.simulatorInvocation == nil {
		opts.simulatorInvocation = &simulatorInvocation{binding: testsimulator.Binding{
			RunID: l.RunID, RepoID: l.RepoID, CreatedAt: l.CreatedAt, OwnerUID: os.Getuid(),
		}}
	}
	return opts
}

func validateSimulatorDispatch(opts RunOpts) error {
	l := opts.TestSimulatorLease
	if l == nil {
		return nil
	}
	opts = withSimulatorInvocation(opts)
	binding := opts.simulatorInvocation.binding
	binding.Now = time.Now()
	err := l.Validate(binding)
	if opts.Purpose != "test-fix" && opts.Purpose != "test-evidence" {
		err = fmt.Errorf("simulator exception is Test-only")
	}
	if err == nil {
		return nil
	}
	if opts.simulatorInvocation.attempted.Load() {
		return &simulatorDispatchError{err: err}
	}
	return fmt.Errorf("%w: %v", testsimulator.ErrUnavailable, err)
}

func beginSimulatorDispatch(opts RunOpts) error {
	if err := validateSimulatorDispatch(opts); err != nil {
		return err
	}
	if opts.TestSimulatorLease != nil && opts.simulatorInvocation != nil {
		opts.simulatorInvocation.attempted.Store(true)
	}
	return nil
}
