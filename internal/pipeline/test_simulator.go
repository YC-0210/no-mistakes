package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testsimulator"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (e *Executor) prepareTestSimulatorLease(step types.StepName, action types.ApprovalAction, l *testsimulator.Lease) (func() error, error) {
	if l == nil {
		return nil, nil
	}
	if step != types.StepTest || action != types.ActionFix {
		return nil, errors.New("simulator lease applies only to a parked Test fix")
	}
	if e.db == nil || e.waitingStepResultID == "" {
		return nil, errors.New("simulator lease requires a durable run and parked round")
	}
	sr, err := e.db.GetStepResult(e.waitingStepResultID)
	if err != nil {
		return nil, err
	}
	if sr == nil {
		return nil, errors.New("simulator allowance has no durable step")
	}
	run, err := e.db.GetRun(sr.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, errors.New("simulator allowance has no durable run")
	}
	if err := l.Validate(testsimulator.Binding{RunID: run.ID, RepoID: run.RepoID, CreatedAt: run.CreatedAt, OwnerUID: os.Getuid(), Now: time.Now()}); err != nil {
		return nil, err
	}
	if run.TestSimulatorLease != nil {
		old, err := testsimulator.Decode([]byte(*run.TestSimulatorLease))
		if err != nil {
			return nil, fmt.Errorf("stored simulator lease is unreadable: %w", err)
		}
		// Expiry may be renewed explicitly; changing device/tool/source identity is
		// not a renewal and must not silently transfer an existing run's allowance.
		old.ExpiresAt = l.ExpiresAt
		before, _ := json.Marshal(old)
		after, _ := json.Marshal(l)
		if string(before) != string(after) {
			return nil, errors.New("simulator lease renewal cannot replace the run's target or tool binding")
		}
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	return func() error { return e.db.SetRunTestSimulatorLease(run.ID, string(raw), run.TestSimulatorLease) }, nil
}
