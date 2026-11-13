package testsimulator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func leaseFixture(t *testing.T) (*Lease, Binding, string) {
	t.Helper()
	uid := os.Getuid()
	zero := 0
	now := time.Now().Truncate(time.Second)
	l := &Lease{Version: 1, RunID: "run-test", RepoID: "repo-test", OwnerUID: &uid, Controller: AppleController, ControllerSHA256: strings.Repeat("a", 64), XcodebuildSHA256: strings.Repeat("b", 64), Registry: "default", Runtime: "com.apple.CoreSimulator.SimRuntime.iOS-26-2", DeviceType: "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", UDID: "12345678-1234-4234-8234-123456789ABC", Name: "no-mistakes-run-test", CreatedAt: now.Unix() - 10, ExpiresAt: now.Unix() + 3600, Project: "App.xcodeproj", Scheme: "App", Creation: CreationReceipt{UDID: "12345678-1234-4234-8234-123456789ABC", AbsentBefore: true, ExitCode: &zero}}
	// UID -1 on Windows cannot own an Apple simulator; hermetic tests use an
	// explicit simulated operator rather than claiming native support there.
	if uid < 0 {
		uid = 501
		l.OwnerUID = &uid
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, l.Project), 0700); err != nil {
		t.Fatal(err)
	}
	return l, Binding{RunID: l.RunID, RepoID: l.RepoID, CreatedAt: now.Unix() - 30, OwnerUID: uid, Now: now}, dir
}

func nativeFixture(l *Lease, inventoryUDID, name, destinations string) commandRunner {
	return func(_ context.Context, _ string, bin string, args ...string) ([]byte, error) {
		if filepath.Base(bin) == "simctl" {
			if strings.Join(args, " ") != "list devices --json" {
				return nil, errors.New("unexpected simulator operation")
			}
			devices := map[string]any{"devices": map[string]any{l.Runtime: []any{map[string]any{"udid": inventoryUDID, "name": name, "deviceTypeIdentifier": l.DeviceType, "isAvailable": true}}}}
			return json.Marshal(devices)
		}
		if filepath.Base(bin) != "xcodebuild" || !strings.Contains(strings.Join(args, " "), "-showdestinations") {
			return nil, errors.New("unexpected native operation")
		}
		return []byte(destinations), nil
	}
}
func fixtureHash(l *Lease) func(string) (string, error) {
	return func(path string) (string, error) {
		if filepath.Base(path) == "simctl" {
			return l.ControllerSHA256, nil
		}
		return l.XcodebuildSHA256, nil
	}
}
func available(l *Lease) string {
	return "Available destinations for the \"App\" scheme:\n { platform:iOS Simulator, arch:arm64, id:" + l.UDID + ", OS:26.2, name:" + l.Name + " }\n"
}

func TestPreflight_AuthorizedOwnedTargetRequiresRealDestinationRows(t *testing.T) {
	l, b, dir := leaseFixture(t)
	if err := preflight(context.Background(), l, b, dir, "darwin", fixtureHash(l), nativeFixture(l, l.UDID, l.Name, available(l))); err != nil {
		t.Fatal(err)
	}
	compat := "Destinations compatible with the \"App\" scheme:\n"+strings.SplitN(available(l), "\n", 2)[1]
	if err := preflight(context.Background(), l, b, dir, "darwin", fixtureHash(l), nativeFixture(l, l.UDID, l.Name, compat)); err != nil {
		t.Fatalf("compatible heading rejected: %v", err)
	}
	for name, output := range map[string]string{
		"echo only":      "xcodebuild -destination id=" + l.UDID,
		"unlabelled row": "{ platform:iOS Simulator, id:" + l.UDID + " }",
		"foreign row":    strings.ReplaceAll(available(l), l.UDID, "00000000-0000-0000-0000-000000000000"),
		"incompatible":   "Destinations incompatible with the \"App\" scheme:\n { platform:iOS Simulator, id:" + l.UDID + ", name:x }",
		"unknown section": "Other destinations for the \"App\" scheme:\n { platform:iOS Simulator, id:" + l.UDID + ", name:x }",
		"uuid prefix":    "Destinations compatible with the \"App\" scheme:\n { platform:iOS Simulator, id:" + l.UDID + "0, name:x }",
		"compatible then incompatible target": "Destinations compatible with the \"App\" scheme:\n { platform:iOS Simulator, id:AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE, name:x }\nDestinations incompatible with the \"App\" scheme:\n { platform:iOS Simulator, id:" + l.UDID + ", name:x }",
		"ineligible":     "Ineligible destinations for the \"App\" scheme:\n { platform:iOS Simulator, id:" + l.UDID + ", error:runtime missing }",
	} {
		t.Run(name, func(t *testing.T) {
			if err := preflight(context.Background(), l, b, dir, "darwin", fixtureHash(l), nativeFixture(l, l.UDID, l.Name, output)); err == nil {
				t.Fatal("unsupported destination was accepted")
			}
		})
	}
}

func TestLeaseDeniedBeforeAnyNativeCall(t *testing.T) {
	cases := map[string]func(*Lease){
		"wrong run": func(l *Lease) { l.RunID = "other" }, "wrong repo": func(l *Lease) { l.RepoID = "foreign" },
		"missing uid": func(l *Lease) { l.OwnerUID = nil }, "foreign uid": func(l *Lease) { u := 999; l.OwnerUID = &u },
		"expired": func(l *Lease) { l.ExpiresAt = time.Now().Unix() - 1 }, "no creation receipt": func(l *Lease) { l.Creation.AbsentBefore = false },
		"missing creation exit": func(l *Lease) { l.Creation.ExitCode = nil }, "failed create": func(l *Lease) { n := 70; l.Creation.ExitCode = &n },
		"foreign target": func(l *Lease) { l.UDID = "00000000-0000-0000-0000-000000000000" },
		"foreign name":   func(l *Lease) { l.Name = "existing-personal-device" }, "custom registry": func(l *Lease) { l.Registry = "private" },
		"unknown controller": func(l *Lease) { l.Controller = "/tmp/simctl" }, "missing tool hash": func(l *Lease) { l.XcodebuildSHA256 = "" },
		"escaping project": func(l *Lease) { l.Project = "../App.xcodeproj" }, "future create": func(l *Lease) { l.CreatedAt = time.Now().Unix() + 3600 },
		"before run": func(l *Lease) { l.CreatedAt = 1 }, "unknown protocol": func(l *Lease) { l.Version = 99 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			l, b, dir := leaseFixture(t)
			mutate(l)
			calls := 0
			err := preflight(context.Background(), l, b, dir, "darwin", func(string) (string, error) { calls++; return "", nil }, func(context.Context, string, string, ...string) ([]byte, error) { calls++; return nil, nil })
			if err == nil || calls != 0 {
				t.Fatalf("err=%v native calls=%d; denied scope must not reach native tools", err, calls)
			}
		})
	}
	if err := (*Lease)(nil).Validate(Binding{}); err == nil {
		t.Fatal("absent scope accepted")
	}
}

func TestPreflightRejectsForeignInventoryAndChangedTool(t *testing.T) {
	l, b, dir := leaseFixture(t)
	for _, name := range []string{"foreign-name", "missing-target", "changed-controller", "unavailable-host"} {
		t.Run(name, func(t *testing.T) {
			device, nameIn := l.UDID, l.Name
			hash := fixtureHash(l)
			platform := "darwin"
			switch name {
			case "foreign-name":
				nameIn = "personal-device"
			case "missing-target":
				device = "00000000-0000-0000-0000-000000000000"
			case "changed-controller":
				hash = func(string) (string, error) { return strings.Repeat("c", 64), nil }
			case "unavailable-host":
				platform = "linux"
			}
			if err := preflight(context.Background(), l, b, dir, platform, hash, nativeFixture(l, device, nameIn, available(l))); err == nil {
				t.Fatal("invalid native prerequisite accepted")
			}
		})
	}
}

func TestDecodeFailsClosed(t *testing.T) {
	for _, raw := range []string{"", "null", `{}`, `{"unrecognized":true}`, `{} {}`, strings.Repeat("x", MaxBytes+1)} {
		l, err := Decode([]byte(raw))
		if err == nil {
			if err = l.Validate(Binding{Now: time.Now()}); err == nil {
				t.Fatalf("malformed lease accepted: %q", raw)
			}
		}
	}
}

func TestNativePrerequisiteEnvironmentCannotSelectACustomRegistry(t *testing.T) {
	original := []string{"PATH=/fixture/bin", "SIMULATOR_DEVICE_SET_PATH=/foreign/set", "DEVELOPER_DIR=/foreign/Xcode", "TMPDIR=/fixture/tmp"}
	got := nativeEnvironment(original)
	if strings.Join(got, ",") != "PATH=/fixture/bin,TMPDIR=/fixture/tmp" {
		t.Fatalf("native environment still selects a foreign registry/toolchain: %v", got)
	}
	if len(original) != 4 {
		t.Fatal("native preflight mutated its caller's environment")
	}
}

func TestNativeOutputIsBoundedWithoutBlockingTheChild(t *testing.T) {
	var b nativeOutputBuffer
	n, err := b.Write([]byte(strings.Repeat("x", maxNativeOutput+10)))
	if err != nil || n != maxNativeOutput+10 || len(b.data) != maxNativeOutput || !b.overflow {
		t.Fatalf("n=%d err=%v len=%d overflow=%v", n, err, len(b.data), b.overflow)
	}
}

func TestLeaseInputRejectsInvalidUTF8AndNonregularFiles(t *testing.T) {
	if _, err := Decode([]byte("{\"scheme\":\"\xff\"}")); err == nil {
		t.Fatal("invalid UTF-8 was normalized instead of refused")
	}
	if _, err := ReadFile(t.TempDir()); err == nil {
		t.Fatal("nonregular receipt accepted")
	}
}
