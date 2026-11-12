package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testsimulator"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func simulatorLeaseForContext(t *testing.T, sctx *pipeline.StepContext) *testsimulator.Lease {
	t.Helper()
	uid := os.Getuid()
	if uid < 0 {
		t.Skip("the operator has no Unix UID for an Apple simulator lease")
	}
	zero := 0
	return &testsimulator.Lease{Version: 1, RunID: sctx.Run.ID, RepoID: sctx.Repo.ID, OwnerUID: &uid, Controller: testsimulator.AppleController, ControllerSHA256: strings.Repeat("a", 64), XcodebuildSHA256: strings.Repeat("b", 64), Registry: "default", Runtime: "com.apple.CoreSimulator.SimRuntime.iOS-26-2", DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", UDID: "12345678-1234-4234-8234-123456789ABC", Name: "no-mistakes-" + sctx.Run.ID, CreatedAt: sctx.Run.CreatedAt, ExpiresAt: time.Now().Unix() + 3600, Project: "App.xcodeproj", Scheme: "App", Creation: testsimulator.CreationReceipt{UDID: "12345678-1234-4234-8234-123456789ABC", AbsentBefore: true, ExitCode: &zero}}
}

func useSimulatorPreflight(t *testing.T, fn func(context.Context, *testsimulator.Lease, testsimulator.Binding, string) error) {
	t.Helper()
	old := testSimulatorPreflight
	testSimulatorPreflight = fn
	t.Cleanup(func() { testSimulatorPreflight = old })
}
func storeSimulatorLease(t *testing.T, sctx *pipeline.StepContext, l *testsimulator.Lease) {
	t.Helper()
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetRunTestSimulatorLease(sctx.Run.ID, string(raw), nil); err != nil {
		t.Fatal(err)
	}
}

// Uses the real Test step, git repository, durable run, phase-specific opts and
// workspace wrapper. Only native discovery and model output are fixtures; this
// is a regression for permission propagation, not live iOS acceptance.
func TestTestStep_SimulatorLeaseReachesRepairAndEvidenceAfterReload(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	var prompts []string
	var purposes []string
	ag := &mockAgent{name: "simulator-scope", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.TestSimulatorLease == nil {
			t.Fatal("typed simulator allowance was lost before the concrete adapter")
		}
		prompts = append(prompts, opts.Prompt)
		purposes = append(purposes, opts.Purpose)
		if opts.Purpose == "test-fix" {
			return &agent.Result{Output: json.RawMessage(`{"summary":"no changes applied"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, agent.WithSteering(ag, t.TempDir()), dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"native-route","severity":"warning","action":"ask-user","description":"native route is unavailable"}]}`
	lease := simulatorLeaseForContext(t, sctx)
	storeSimulatorLease(t, sctx, lease)
	if sctx.Run.TestSimulatorLease != nil {
		t.Fatal("fixture must retain a stale in-memory run to exercise durable reload")
	}
	probes := 0
	useSimulatorPreflight(t, func(_ context.Context, l *testsimulator.Lease, b testsimulator.Binding, work string) error {
		probes++
		if l.RunID != sctx.Run.ID || b.RepoID != sctx.Repo.ID || work != dir {
			t.Fatal("wrong native preflight binding")
		}
		return nil
	})
	out, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if out.NeedsApproval || len(prompts) != 2 || probes != 2 || strings.Join(purposes, ",") != "test-fix,test-evidence" {
		t.Fatalf("out=%+v prompts=%d probes=%d purposes=%v", out, len(prompts), probes, purposes)
	}
	for _, p := range prompts {
		if !strings.Contains(p, "Except for the explicit per-run simulator exception below, confine source") || !strings.Contains(p, "Only add new commits on top") || !strings.Contains(p, lease.UDID) || !strings.Contains(p, "not OS enforcement") {
			t.Fatalf("scope did not reach the preceding workspace policy without relaxing source/history: %s", p)
		}
		if strings.Index(p, "Explicit per-run simulator exception") > strings.Index(p, "Repair scope:") {
			t.Fatal("exception was left in trailing driver guidance")
		}
	}
}

func TestTestStep_UnavailableSimulatorSkipsCodeRepairButStillRunsEvidence(t *testing.T) {
	for _, kind := range []string{"undiscovered", "foreign", "expired", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "unavailable-native", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				if opts.Purpose != "test-evidence" || opts.TestSimulatorLease != nil {
					t.Fatal("unavailable native prerequisite dispatched repair or granted a device")
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"native route unavailable","tested":["read-only native preflight"],"testing_summary":"native journeys not run","artifacts":[],"scenarios":[{"name":"synthetic native journey","result":"untested","live":false,"evidence":"","reason":"no usable native route"}],"verdict":"inconclusive"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, agent.WithSteering(ag, t.TempDir()), dir, base, head, config.Commands{})
			sctx.Fixing = true
			l := simulatorLeaseForContext(t, sctx)
			switch kind {
			case "expired":
				l.ExpiresAt = time.Now().Unix() - 1
			case "foreign":
				l.Creation.AbsentBefore = false
			}
			if kind == "corrupt" {
				if err := sctx.DB.SetRunTestSimulatorLease(sctx.Run.ID, "{", nil); err != nil {
					t.Fatal(err)
				}
			} else {
				storeSimulatorLease(t, sctx, l)
			}
			useSimulatorPreflight(t, func(context.Context, *testsimulator.Lease, testsimulator.Binding, string) error {
				return errors.New("exact UDID absent from Xcode destinations")
			})
			out, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("environment prerequisite became a code-failed step: %v", err)
			}
			findings, err := types.ParseFindingsJSON(out.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if !out.NeedsApproval || out.ExitCode != 0 || findings.Verdict != "inconclusive" || calls != 1 {
				t.Fatalf("out=%+v verdict=%s evidence calls=%d", out, findings.Verdict, calls)
			}
			if findings.Scenarios[0].Live || findings.Scenarios[0].Result != "untested" {
				t.Fatal("unexecuted native case became live or passed")
			}
			found := false
			for _, f := range findings.Items {
				if f.ID == "test-native-prerequisite" && f.Action == types.ActionAskUser {
					found = true
				}
			}
			if !found {
				t.Fatal("missing actionable prerequisite park")
			}
		})
	}
}

func TestTestStep_SimulatorCorrectionTurnDoesNotProbeOrGrant(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls, probes := 0, 0
	ag := &mockAgent{name: "native-correction", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
		}
		if opts.Purpose != "test-correction" || opts.TestSimulatorLease != nil {
			t.Fatal("correction turn acquired a simulator allowance")
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, agent.WithSteering(ag, t.TempDir()), dir, base, head, config.Commands{})
	storeSimulatorLease(t, sctx, simulatorLeaseForContext(t, sctx))
	useSimulatorPreflight(t, func(context.Context, *testsimulator.Lease, testsimulator.Binding, string) error { probes++; return nil })
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || probes != 1 {
		t.Fatalf("calls=%d probes=%d; correction must not rerun preflight", calls, probes)
	}
}

func TestSimulatorPrerequisiteNeverTurnsMissingExecutionGreenOrErasesLiveFailure(t *testing.T) {
	blocked := &simulatorTestAgent{blocked: errors.New("not discovered")}
	f := Findings{Verdict: "go"}
	applySimulatorPrerequisite(&f, blocked)
	if f.Verdict != "inconclusive" {
		t.Fatal("missing native prerequisite stayed green")
	}
	live := Findings{Verdict: "no-go", Scenarios: []types.TestScenario{{Name: "independent observed product failure", Result: "fail", Live: true, Evidence: "actual failure"}}}
	applySimulatorPrerequisite(&live, blocked)
	if live.Verdict != "no-go" {
		t.Fatal("actual live product failure was downgraded")
	}
}

func TestTestStep_SimulatorExpiryBetweenPreflightAndLaunchStillRunsUnscopedEvidence(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "scope-expiry", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if opts.TestSimulatorLease != nil {
			t.Fatal("expired scope reached a concrete adapter")
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"prerequisite expired","tested":["preflight"],"testing_summary":"not executed","artifacts":[],"scenarios":[{"name":"native journey","result":"untested","live":false,"evidence":"","reason":"expired scope"}],"verdict":"inconclusive"}`)}, nil
	}}
	unused := &mockAgent{name: "unused", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("pre-launch refusal must not dispatch another provider")
		return nil, nil
	}}
	chain := agent.NewFallback([]agent.Agent{agent.WithSteering(ag, t.TempDir()), agent.WithSteering(unused, t.TempDir())}, agent.WithQuotaSwitch(""))
	sctx := newTestContextWithDBRecords(t, chain, dir, base, head, config.Commands{})
	sctx.Fixing = true
	storeSimulatorLease(t, sctx, simulatorLeaseForContext(t, sctx))
	useSimulatorPreflight(t, func(_ context.Context, l *testsimulator.Lease, _ testsimulator.Binding, _ string) error {
		l.ExpiresAt = time.Now().Unix() - 1
		return nil
	})
	out, err := (&TestStep{}).Execute(sctx)
	if err != nil || !out.NeedsApproval || calls != 1 {
		t.Fatalf("out=%+v err=%v calls=%d", out, err, calls)
	}
}

func TestTestStep_SimulatorExpiryAfterQuotaFallbackNeverReplays(t *testing.T) {
	for _, fixing := range []bool{false, true} {
		purpose := "test-evidence"
		if fixing {
			purpose = "test-fix"
		}
		t.Run(purpose, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			marker := filepath.Join(dir, "provider-side-effect")
			firstCalls, secondCalls := 0, 0
			first := &mockAgent{name: "codex", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				firstCalls++
				if opts.Purpose != purpose || opts.TestSimulatorLease == nil {
					t.Fatal("first provider did not receive the scoped Test invocation")
				}
				if err := os.WriteFile(marker, []byte("executed"), 0600); err != nil {
					t.Fatal(err)
				}
				opts.TestSimulatorLease.ExpiresAt = time.Now().Unix() - 1
				return nil, errors.New("codex failed: quota exceeded")
			}}
			second := &mockAgent{name: "claude", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				secondCalls++
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			}}
			chain := agent.NewFallback([]agent.Agent{agent.WithSteering(first, t.TempDir()), agent.WithSteering(second, t.TempDir())}, agent.WithQuotaSwitch(""))
			sctx := newTestContextWithDBRecords(t, chain, dir, base, head, config.Commands{})
			sctx.Fixing = fixing
			storeSimulatorLease(t, sctx, simulatorLeaseForContext(t, sctx))
			probes := 0
			useSimulatorPreflight(t, func(context.Context, *testsimulator.Lease, testsimulator.Binding, string) error {
				probes++
				return nil
			})
			out, err := (&TestStep{}).Execute(sctx)
			if err == nil || errors.Is(err, testsimulator.ErrUnavailable) {
				t.Fatalf("partially executed invocation became a recoverable refusal: out=%+v err=%v", out, err)
			}
			if firstCalls != 1 || secondCalls != 0 || probes != 1 {
				t.Fatalf("invocation replayed: first=%d second=%d probes=%d", firstCalls, secondCalls, probes)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "executed" {
				t.Fatalf("missing executed-provider side effect: data=%q err=%v", data, err)
			}
		})
	}
}
