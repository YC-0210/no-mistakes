package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testsimulator"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type simulatorReceiptStep struct{ received chan string }

func (*simulatorReceiptStep) Name() types.StepName { return types.StepTest }
func (s *simulatorReceiptStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if !ctx.Fixing {
		return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"native-prerequisite","severity":"warning","action":"ask-user","description":"native runner unavailable"}]}`}, nil
	}
	run, err := ctx.DB.GetRun(ctx.Run.ID)
	if err != nil {
		return nil, err
	}
	if run.TestSimulatorLease == nil {
		return nil, fmt.Errorf("IPC response dropped the simulator allowance")
	}
	s.received <- *run.TestSimulatorLease
	return &pipeline.StepOutcome{}, nil
}

// Drives the actual registered IPC handler, manager, parked executor and DB.
// The synthetic step never invokes a simulator or an external agent.
func TestRespondIPC_BindsSimulatorReceiptBeforeTheTestFixRuns(t *testing.T) {
	uid := os.Getuid()
	if uid < 0 {
		t.Skip("Apple simulator leases require a Unix operator uid")
	}
	p := paths.WithRoot(t.TempDir())
	database, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://example.test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step := &simulatorReceiptStep{received: make(chan string, 1)}
	exec := pipeline.NewExecutor(database, p, &config.Config{}, nil, []pipeline.Step{step}, nil)
	mgr := NewRunManager(database, p, nil)
	mgr.executors[run.ID] = exec
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(pipeline.ErrDaemonShutdown)
	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	defer func() {
		cancel(pipeline.ErrDaemonShutdown)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("fixture executor did not finish")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.Respond(types.StepReview, types.ActionApprove, nil)
		if err != nil && strings.Contains(err.Error(), "step mismatch") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic Test gate did not park")
		}
		time.Sleep(10 * time.Millisecond)
	}
	srv := ipc.NewServer()
	registerHandlers(srv, mgr, database, func() {})
	socketRoot, err := os.MkdirTemp("", "nm-sim-lease-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	if err := srv.Listen(paths.WithRoot(socketRoot).Socket()); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.ServeReady() }()
	defer func() { srv.Close(); <-serverDone }()
	client, err := ipc.Dial(paths.WithRoot(socketRoot).Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var capability struct {
		Version int `json:"version"`
	}
	if err := client.Call(ipc.MethodProbeTestSimulatorLease, nil, &capability); err != nil || capability.Version != 1 {
		t.Fatalf("capability=%+v err=%v", capability, err)
	}
	zero := 0
	lease := &testsimulator.Lease{Version: 1, RunID: run.ID, RepoID: repo.ID, OwnerUID: &uid, Controller: testsimulator.AppleController, ControllerSHA256: strings.Repeat("a", 64), XcodebuildSHA256: strings.Repeat("b", 64), Registry: "default", Runtime: "com.apple.CoreSimulator.SimRuntime.iOS-26-2", DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", UDID: "12345678-1234-4234-8234-123456789ABC", Name: "no-mistakes-" + run.ID, CreatedAt: run.CreatedAt, ExpiresAt: time.Now().Unix() + 3600, Project: "App.xcodeproj", Scheme: "App", Creation: testsimulator.CreationReceipt{UDID: "12345678-1234-4234-8234-123456789ABC", AbsentBefore: true, ExitCode: &zero}}
	request := &ipc.RespondParams{RunID: run.ID, Step: types.StepTest, Action: types.ActionFix, FindingIDs: []string{"native-prerequisite"}, TestSimulatorLease: lease}
	foreign := *lease
	foreign.RunID = "foreign"
	request.TestSimulatorLease = &foreign
	if err := client.Call(ipc.MethodRespond, request, &ipc.RespondResult{}); err == nil {
		t.Fatal("foreign lease was accepted by IPC")
	}
	check, err := database.GetRun(run.ID)
	if err != nil || check.TestSimulatorLease != nil {
		t.Fatal("refused IPC response changed resource authority")
	}
	request.TestSimulatorLease = lease
	var result ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, request, &result); err != nil || !result.OK {
		t.Fatalf("respond=%+v err=%v", result, err)
	}
	select {
	case raw := <-step.received:
		l, err := testsimulator.Decode([]byte(raw))
		if err != nil || l.UDID != lease.UDID {
			t.Fatalf("wrong durable scope: %v %v", l, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Test fix did not receive the explicit receipt")
	}
}
