package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// Reproduce the original operator path with a real CLI process and IPC socket:
// a precise, separately issued resource receipt must have its own supported
// transport instead of being sent as contradictory per-finding prose.
func TestNoMistakesBinary_ExplicitTestSimulatorLeaseReachesTheGate(t *testing.T) {
	var got map[string]json.RawMessage
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{testSimulatorProbe: func(context.Context, json.RawMessage) (interface{}, error) { return map[string]int{"version": 1}, nil }, respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		if err := json.Unmarshal(raw, &got); err != nil {
			return nil, err
		}
		responded.Store(true)
		return &ipc.RespondResult{OK: true, Fixed: []string{"R1"}, Ignored: []string{"R2"}}, nil
	}})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		run := fx.awaiting()
		run.Steps[0].StepName = "test"
		return run, nil
	})
	receipt := `{"version":1,"run_id":"run-timeout","repo_id":"repo-timeout","owner_uid":501,"controller":"/Applications/Xcode.app/Contents/Developer/usr/bin/simctl","controller_sha256":"` + strings.Repeat("a", 64) + `","xcodebuild_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","runtime":"com.apple.CoreSimulator.SimRuntime.iOS-26-2","device_type":"com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro","registry":"default","udid":"12345678-1234-4234-8234-123456789ABC","name":"no-mistakes-run-timeout","created_at":` + jsonNumber(time.Now().Unix()-30) + `,"expires_at":` + jsonNumber(time.Now().Unix()+3600) + `,"project":"App.xcodeproj","scheme":"App","creation":{"udid":"12345678-1234-4234-8234-123456789ABC","absent_before":true,"exit_code":0}}`
	path := filepath.Join(t.TempDir(), "simulator-lease.json")
	if err := os.WriteFile(path, []byte(receipt), 0600); err != nil {
		t.Fatal(err)
	}
	bin := buildNoMistakesBinary(t)
	out, err := execNoMistakesErr(t, bin, "axi", "respond", "--run", "run-timeout", "--action", "fix", "--findings", "R1", "--ignore", "R2", "--test-simulator-lease-file", path, "--wait", "3s")
	if err != nil {
		t.Fatalf("explicit simulator permission was not delivered to the Test gate: %v\n%s", err, out)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(got["test_simulator_lease"], &forwarded); err != nil {
		t.Fatalf("missing typed resource receipt: %v (%s)", err, out)
	}
	if forwarded["run_id"] != "run-timeout" || forwarded["udid"] != "12345678-1234-4234-8234-123456789ABC" {
		t.Fatalf("wrong resource binding: %v", forwarded)
	}
	if len(got["instructions"]) > 0 {
		t.Fatal("resource permission was demoted to driver instructions")
	}
}

func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestAxiRespond_SimulatorLeaseRefusesOlderDaemonBeforeMutation(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(context.Context, json.RawMessage) (interface{}, error) {
		responded.Store(true)
		return &ipc.RespondResult{OK: true}, nil
	}})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		run := fx.awaiting()
		run.Steps[0].StepName = "test"
		return run, nil
	})
	path := filepath.Join(t.TempDir(), "lease.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"run_id":"run-timeout"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--action", "fix", "--findings", "R1", "--test-simulator-lease-file", path, "--wait", "3s")
	if err == nil || responded.Load() || !strings.Contains(out, "no response sent") {
		t.Fatalf("old daemon silently consumed unsupported permission: err=%v responded=%v\n%s", err, responded.Load(), out)
	}
}

func TestAxiRespond_SimulatorLeaseCapabilityProbeHonorsWait(t *testing.T) {
	var responded, probed atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		testSimulatorProbe: func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
			probed.Store(true)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		run := fx.awaiting()
		run.Steps[0].StepName = "test"
		return run, nil
	})
	path := filepath.Join(t.TempDir(), "lease.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"run_id":"run-timeout"}`), 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--action", "fix", "--findings", "R1", "--test-simulator-lease-file", path, "--wait", "500ms")
	if err == nil || responded.Load() || !probed.Load() || !strings.Contains(out, "elapsed") || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v responded=%v probed=%v elapsed=%v\n%s", err, responded.Load(), probed.Load(), time.Since(start), out)
	}
}
