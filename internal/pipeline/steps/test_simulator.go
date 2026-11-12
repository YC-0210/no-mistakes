package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testsimulator"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Test seam covers native discovery without touching a real simulator. The
// production function only reads registry/destination metadata, never controls.
var testSimulatorPreflight = testsimulator.Preflight

type simulatorTestAgent struct {
	agent.Agent
	lease     *testsimulator.Lease
	binding   testsimulator.Binding
	workDir   string
	blocked   error
	decodeErr error
}

func simulatorTestContext(sctx *pipeline.StepContext) (*pipeline.StepContext, *simulatorTestAgent, error) {
	run := sctx.Run
	var durableErr error
	// A gate response pins the receipt while this Execute's original Run pointer
	// may still be in memory. Re-read the one durable value on normal and recovery
	// execution; never inherit it from a previous/retried run or repository config.
	if sctx.DB != nil {
		fresh, err := sctx.DB.GetRun(run.ID)
		if err != nil {
			return nil, nil, err
		}
		if fresh == nil {
			if run.TestSimulatorLease == nil {
				return sctx, nil, nil
			}
			durableErr = errors.New("simulator allowance requires its original durable run")
		} else {
			run = fresh
		}
	} else if run.TestSimulatorLease != nil {
		durableErr = errors.New("simulator allowance requires a durable run")
	}

	if run.TestSimulatorLease == nil {
		return sctx, nil, nil
	}
	scoped := &simulatorTestAgent{Agent: sctx.Agent, workDir: sctx.WorkDir, binding: testsimulator.Binding{RunID: run.ID, RepoID: run.RepoID, CreatedAt: run.CreatedAt, OwnerUID: os.Getuid()}}
	scoped.lease, scoped.decodeErr = testsimulator.Decode([]byte(*run.TestSimulatorLease))
	if durableErr != nil {
		scoped.decodeErr = durableErr
	}
	copy := *sctx
	copy.Agent = scoped
	return &copy, scoped, nil
}

func (a *simulatorTestAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	// Schema correction is intentionally tool-free. It neither earns a resource
	// exception nor re-runs native discovery or any other external operation.
	if opts.Purpose != "test-fix" && opts.Purpose != "test-evidence" {
		opts.TestSimulatorLease = nil
		return a.Agent.Run(ctx, opts)
	}
	a.binding.Now = time.Now()
	err := a.decodeErr
	if err == nil {
		err = a.lease.Validate(a.binding)
	}
	if err == nil {
		err = testSimulatorPreflight(ctx, a.lease, a.binding, a.workDir)
	}
	a.blocked = err
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		opts.TestSimulatorLease = nil
		if opts.Purpose == "test-fix" {
			// No code repair can supply this prerequisite. The evidence turn still
			// runs unconditionally, derives scenarios, and records the missing proof.
			return &agent.Result{Output: json.RawMessage(`{"summary":"no changes applied: native execution prerequisite unavailable"}`)}, nil
		}
		opts.Prompt = fmt.Sprintf("Native simulator prerequisite blocked: %s.\nNo external simulator exception is active. Do not repair app code to bypass this prerequisite. Derive the intended scenarios and report missing native execution as untested/live:false with an inconclusive verdict; preserve any independently actually executed application failure as no-go. Do not claim this preflight performed boot, install, launch or accepted journeys.\n\n", err) + opts.Prompt
	} else {
		opts.TestSimulatorLease = a.lease
	}
	result, runErr := a.Agent.Run(ctx, opts)
	if errors.Is(runErr, testsimulator.ErrUnavailable) {
		// The steering wrapper refused before launching a provider, so no tool
		// side effects can be replayed. Expiry between preflight and dispatch is
		// still an environment block, not a failed code turn.
		a.blocked = runErr
		if opts.Purpose == "test-fix" {
			return &agent.Result{Output: json.RawMessage(`{"summary":"no changes applied: simulator allowance expired before launch"}`)}, nil
		}
		opts.TestSimulatorLease = nil
		opts.Prompt = "Native simulator allowance became unavailable before launch; no device exception is active. Record missing native execution as untested/live:false and inconclusive, not an application failure or pass.\n\n" + opts.Prompt
		return a.Agent.Run(ctx, opts)
	}
	return result, runErr
}

func applySimulatorPrerequisite(findings *Findings, a *simulatorTestAgent) {
	if a == nil || a.blocked == nil {
		return
	}
	// An actual observed app failure stays no-go. An unavailable prerequisite
	// alone cannot masquerade as either a code failure or a successful Test.
	hasFailure := false
	for _, scenario := range findings.Scenarios {
		if scenario.Result == "fail" {
			hasFailure = true
		}
	}
	if !hasFailure {
		findings.Verdict = "inconclusive"
	}
	findings.Items = append(findings.Items, Finding{ID: "test-native-prerequisite", Severity: "warning", Action: types.ActionAskUser, Description: "Native execution prerequisite is unavailable: " + a.blocked.Error() + ". Supply a valid operator-issued lease for one newly owned default-registry target and prove Xcode discovery before native acceptance; code-only fix retries do not supply this resource. No native pass is established by preflight."})
}
