package pipeline

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/testsimulator"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_SimulatorLeaseIsValidatedBeforeDispatchAndSurvivesRecoveryRead(t *testing.T) {
	database, p, run, _ := setupTest(t)
	sr, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	gate := `{"findings":[{"id":"native","severity":"warning","action":"ask-user","description":"native prerequisite missing"}]}`
	round, err := database.InsertStepRound(sr.ID, 1, "initial", &gate, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ex := NewExecutor(database, p, nil, nil, nil, nil)
	ex.waiting = true
	ex.waitingStep = types.StepTest
	ex.waitingStepResultID = sr.ID
	ex.waitingRoundID = round.ID
	ex.waitingFindings = gate
	uid := os.Getuid()
	if uid < 0 {
		t.Skip("Apple simulator leases require a Unix operator uid")
	}
	zero := 0
	lease := &testsimulator.Lease{Version: 1, RunID: run.ID, RepoID: run.RepoID, OwnerUID: &uid, Controller: testsimulator.AppleController, ControllerSHA256: strings.Repeat("a", 64), XcodebuildSHA256: strings.Repeat("b", 64), Registry: "default", Runtime: "com.apple.CoreSimulator.SimRuntime.iOS-26-2", DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", UDID: "12345678-1234-4234-8234-123456789ABC", Name: "no-mistakes-" + run.ID, CreatedAt: run.CreatedAt, ExpiresAt: time.Now().Unix() + 3600, Project: "App.xcodeproj", Scheme: "App", Creation: testsimulator.CreationReceipt{UDID: "12345678-1234-4234-8234-123456789ABC", AbsentBefore: true, ExitCode: &zero}}
	foreign := *lease
	foreign.RunID = "foreign"
	if _, err := ex.RespondWithOverrides(types.StepTest, types.ActionFix, []string{"native"}, nil, nil, nil, "", ResponseOptions{TestSimulatorLease: &foreign}); err == nil {
		t.Fatal("foreign binding was accepted")
	}
	if !ex.waiting || len(ex.approvalCh) != 0 {
		t.Fatal("invalid lease released the parked gate")
	}
	rounds, err := database.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rounds[0].SelectedFindingIDs != nil {
		t.Fatal("invalid lease recorded a fix decision")
	}
	if _, err := ex.RespondWithOverrides(types.StepTest, types.ActionFix, []string{"native"}, nil, nil, nil, "", ResponseOptions{TestSimulatorLease: lease}); err != nil {
		t.Fatal(err)
	}
	if ex.waiting || len(ex.approvalCh) != 1 {
		t.Fatal("accepted receipt did not dispatch exactly once")
	}
	recovered, err := db.OpenReadOnly(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	got, err := recovered.GetRun(run.ID)
	if err != nil || got.TestSimulatorLease == nil {
		t.Fatalf("recovery lost lease: run=%v err=%v", got, err)
	}
	loaded, err := testsimulator.Decode([]byte(*got.TestSimulatorLease))
	if err != nil || loaded.UDID != lease.UDID {
		t.Fatalf("wrong persisted scope: %v %v", loaded, err)
	}
	another := *lease
	another.UDID = "00000000-0000-0000-0000-000000000000"
	another.Creation.UDID = another.UDID
	ex.waitingStepResultID = sr.ID
	if _, err := ex.prepareTestSimulatorLease(types.StepTest, types.ActionFix, &another); err == nil {
		t.Fatal("existing run's resource identity was replaced")
	}
	// Another run, even on the same branch, has no resource allowance.
	newRun, err := database.InsertRun(run.RepoID, run.Branch, run.HeadSHA, run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if newRun.TestSimulatorLease != nil {
		t.Fatal("run-bound permission leaked into another run")
	}
	raw, _ := json.Marshal(lease)
	if !strings.Contains(*got.TestSimulatorLease, string(raw)) {
		t.Fatal("stored receipt changed")
	}
}
