package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testsimulator"
)

func dispatchTestLease(t *testing.T) *testsimulator.Lease {
	t.Helper()
	uid, zero := os.Getuid(), 0
	if uid < 0 {
		t.Skip("simulator allowances require a Unix owner")
	}
	now := time.Now().Unix()
	return &testsimulator.Lease{
		Version: 1, RunID: "fixture-run", RepoID: "fixture-repo", OwnerUID: &uid,
		Controller: testsimulator.AppleController, ControllerSHA256: strings.Repeat("a", 64), XcodebuildSHA256: strings.Repeat("b", 64),
		Registry: "default", Runtime: "com.apple.CoreSimulator.SimRuntime.iOS-26-2", DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro",
		UDID: "12345678-1234-4234-8234-123456789ABC", Name: "no-mistakes-fixture-run", CreatedAt: now, ExpiresAt: now + 3600,
		Project: "App.xcodeproj", Scheme: "App",
		Creation: testsimulator.CreationReceipt{UDID: "12345678-1234-4234-8234-123456789ABC", AbsentBefore: true, ExitCode: &zero},
	}
}

func assertTerminalSimulatorRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil || errors.Is(err, testsimulator.ErrUnavailable) || !IsTerminalProviderError(err) {
		t.Fatalf("expiry after execution must be terminal, never recoverable: %v", err)
	}
}

func TestSimulatorDispatch_CodexExpiryDuringBackoff(t *testing.T) {
	cwd := t.TempDir()
	bin := writeFakeCodex(t, cwd, "#!/bin/sh\ncat >/dev/null\necho run >> attempts\necho rate_limit_error >&2\nexit 1\n", "")
	lease := dispatchTestLease(t)
	old := transientBackoff
	t.Cleanup(func() { transientBackoff = old })
	backoffs := 0
	transientBackoff = func(context.Context, int) error {
		backoffs++
		lease.ExpiresAt = time.Now().Unix() - 1
		return nil
	}
	other := &recordingAgent{name: "other"}
	chain := NewFallback([]Agent{WithSteering(&codexAgent{bin: bin}, cwd), WithSteering(other, cwd)}, WithQuotaSwitch(""))
	_, err := chain.Run(context.Background(), RunOpts{Prompt: "fixture", CWD: cwd, Purpose: "test-fix", TestSimulatorLease: lease})
	assertTerminalSimulatorRefusal(t, err)
	attempts, readErr := os.ReadFile(filepath.Join(cwd, "attempts"))
	if readErr != nil || strings.TrimSpace(string(attempts)) != "run" || backoffs != 1 || other.runCalls != 0 {
		t.Fatalf("unexpected dispatches: attempts=%q readErr=%v backoffs=%d fallback=%d", attempts, readErr, backoffs, other.runCalls)
	}
}

func TestSimulatorDispatch_PiExpiryBeforePromptFallback(t *testing.T) {
	cwd := t.TempDir()
	refusal := `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"Tool \"no_mistakes_output\" requires JSON-schema constrained sampling, but strict tools are unsupported.","content":[]}}`
	bin := writePiOutputFixture(t, piStrictVersion, []string{refusal}, piTextEvents(`{"ok":true}`), "")
	lease := dispatchTestLease(t)
	fallbacks := 0
	_, err := (&piAgent{bin: bin}).Run(context.Background(), RunOpts{
		Prompt: "fixture", CWD: cwd, Purpose: "test-evidence", TestSimulatorLease: lease,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		OnLifecycle: func(event LifecycleEvent) {
			if event.Phase == LifecyclePhaseFallback {
				fallbacks++
				lease.ExpiresAt = time.Now().Unix() - 1
			}
		},
	})
	assertTerminalSimulatorRefusal(t, err)
	if runs := readPiRuns(t, cwd); runs != 1 || fallbacks != 1 {
		t.Fatalf("runs=%d fallbacks=%d; prompt fallback must never launch", runs, fallbacks)
	}
}

func TestSimulatorDispatch_OpencodeExpiryBeforePromptFallback(t *testing.T) {
	body := strings.Replace(providerRejectedBody, "Thinking mode does not support this tool_choice", "tool_choice 'required' is incompatible with thinking enabled", 1)
	server, sent := opencodeErrorServer(t, body)
	lease := dispatchTestLease(t)
	a := &opencodeAgent{bin: "unused", server: &managedServer{port: mustParsePort(server.URL)}}
	fallbacks := 0
	_, err := a.Run(context.Background(), RunOpts{
		Prompt: "fixture", CWD: t.TempDir(), Purpose: "test-evidence", TestSimulatorLease: lease,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		OnLifecycle: func(event LifecycleEvent) {
			if event.Phase == LifecyclePhaseFallback {
				fallbacks++
				lease.ExpiresAt = time.Now().Unix() - 1
			}
		},
	})
	assertTerminalSimulatorRefusal(t, err)
	if *sent != 1 || fallbacks != 1 {
		t.Fatalf("requests=%d fallbacks=%d; second prompt must never dispatch", *sent, fallbacks)
	}
}

func TestSimulatorDispatch_UnlaunchedExpiryStaysRecoverable(t *testing.T) {
	for name, a := range map[string]Agent{
		"codex": &codexAgent{bin: "must-not-launch"}, "claude": &claudeAgent{bin: "must-not-launch"},
		"pi": &piAgent{bin: "must-not-launch"}, "opencode": &opencodeAgent{bin: "must-not-launch"},
		"rovodev": &rovodevAgent{bin: "must-not-launch"}, "grok": &grokAgent{bin: "must-not-launch"},
		"copilot": &copilotAgent{bin: "must-not-launch"}, "antigravity": &antigravityAgent{bin: "must-not-launch"},
		"acpx": &acpxAgent{bin: "must-not-launch"},
	} {
		t.Run(name, func(t *testing.T) {
			lease := dispatchTestLease(t)
			lease.ExpiresAt = time.Now().Unix() - 1
			_, err := a.Run(context.Background(), RunOpts{Purpose: "test-evidence", TestSimulatorLease: lease, CWD: t.TempDir()})
			if !errors.Is(err, testsimulator.ErrUnavailable) {
				t.Fatalf("unlaunched refusal must remain recoverable: %v", err)
			}
		})
	}
}

type simulatorExpiryTransport struct {
	base   http.RoundTripper
	expire func()
}

func (t simulatorExpiryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if req.URL.Path == "/api/event" {
		t.expire()
	}
	return response, err
}

func TestSimulatorDispatch_OpencodeV2ExpiryDuringSetup(t *testing.T) {
	fixture := &opencodeV2Fixture{}
	server := fixture.server(t)
	lease := dispatchTestLease(t)
	previous := opencodeV2HTTP.Transport
	t.Cleanup(func() { opencodeV2HTTP.Transport = previous })
	base := previous
	if base == nil {
		base = http.DefaultTransport
	}
	opencodeV2HTTP.Transport = simulatorExpiryTransport{base: base, expire: func() { lease.ExpiresAt = time.Now().Unix() - 1 }}
	a := &opencodeAgent{bin: "unused", v2: true, server: &managedServer{port: mustParsePort(server.URL)}}
	_, err := a.Run(context.Background(), RunOpts{Prompt: "fixture", CWD: t.TempDir(), Purpose: "test-evidence", TestSimulatorLease: lease})
	if !errors.Is(err, testsimulator.ErrUnavailable) {
		t.Fatalf("setup without a dispatched prompt must remain recoverable: %v", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, call := range fixture.calls {
		if call == "POST /api/session/ses_fixture/prompt" {
			t.Fatal("expired allowance dispatched a V2 prompt")
		}
	}
}
