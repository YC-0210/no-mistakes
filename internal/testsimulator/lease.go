// Package testsimulator validates an operator-issued lease for one newly
// provisioned default-registry iOS simulator. It never creates or controls a
// device. Its allowance is prompt steering, not an operating-system sandbox.
package testsimulator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// ErrUnavailable is returned only by pre-launch scope validation, never by a
// provider after tools may have run. It permits an evidence-only fallback.
var ErrUnavailable = errors.New("native simulator scope unavailable before agent launch")

const Version = 1
const MaxBytes = 16384
const AppleController = "/Applications/Xcode.app/Contents/Developer/usr/bin/simctl"

// Lease is supplied explicitly over the local operator API, never by repository
// configuration or agent output. Creation is an operator attestation backed by
// its retained receipt, not cryptographic proof of who ran simctl.
type Lease struct {
	Version          int             `json:"version"`
	RunID            string          `json:"run_id"`
	RepoID           string          `json:"repo_id"`
	OwnerUID         *int            `json:"owner_uid"`
	Controller       string          `json:"controller"`
	ControllerSHA256 string          `json:"controller_sha256"`
	XcodebuildSHA256 string          `json:"xcodebuild_sha256"`
	Registry         string          `json:"registry"`
	Runtime          string          `json:"runtime"`
	DeviceType       string          `json:"device_type"`
	UDID             string          `json:"udid"`
	Name             string          `json:"name"`
	CreatedAt        int64           `json:"created_at"`
	ExpiresAt        int64           `json:"expires_at"`
	Project          string          `json:"project"`
	Scheme           string          `json:"scheme"`
	Creation         CreationReceipt `json:"creation"`
}

type CreationReceipt struct {
	UDID         string `json:"udid"`
	AbsentBefore bool   `json:"absent_before"`
	ExitCode     *int   `json:"exit_code"`
}

type Binding struct {
	RunID     string
	RepoID    string
	CreatedAt int64
	OwnerUID  int
	Now       time.Time
}

var slug = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var uuid = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)
var iosRuntime = regexp.MustCompile(`^com\.apple\.CoreSimulator\.SimRuntime\.iOS-[0-9]+-[0-9]+(?:-[0-9]+)?$`)
var deviceType = regexp.MustCompile(`^com\.apple\.CoreSimulator\.SimDeviceType\.[A-Za-z0-9-]+$`)

func Decode(raw []byte) (*Lease, error) {
	if len(raw) == 0 || len(raw) > MaxBytes {
		return nil, errors.New("simulator lease must be nonempty and at most 16384 bytes")
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("simulator lease must be valid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var l *Lease
	if err := d.Decode(&l); err != nil {
		return nil, fmt.Errorf("decode simulator lease: %w", err)
	}
	if l == nil {
		return nil, errors.New("simulator lease must be an object")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("simulator lease must contain exactly one JSON object")
	}
	return l, nil
}

func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}

// Validate rejects stale, foreign or unbound receipts before any native call.
func (l *Lease) Validate(b Binding) error {
	if l == nil {
		return errors.New("no simulator lease")
	}
	if l.Version != Version {
		return errors.New("unsupported simulator lease version")
	}
	if !slug.MatchString(l.RunID) || !slug.MatchString(l.RepoID) || l.RunID != b.RunID || l.RepoID != b.RepoID {
		return errors.New("simulator lease belongs to a different run or repository")
	}
	if l.OwnerUID == nil || *l.OwnerUID < 0 || *l.OwnerUID != b.OwnerUID {
		return errors.New("simulator lease has no matching operator uid")
	}
	if l.Controller != AppleController || !validHash(l.ControllerSHA256) || !validHash(l.XcodebuildSHA256) {
		return errors.New("simulator lease requires the absolute Apple controller and exact tool hashes")
	}
	if l.Registry != "default" || !iosRuntime.MatchString(l.Runtime) || !deviceType.MatchString(l.DeviceType) {
		return errors.New("simulator lease requires a default-registry iOS runtime and device type")
	}
	if !uuid.MatchString(l.UDID) || l.Creation.UDID != l.UDID || !l.Creation.AbsentBefore || l.Creation.ExitCode == nil || *l.Creation.ExitCode != 0 || l.Name != "no-mistakes-"+l.RunID {
		return errors.New("simulator lease lacks a newly owned target creation receipt")
	}
	if l.CreatedAt < b.CreatedAt || l.CreatedAt > b.Now.Unix() || l.CreatedAt <= 0 || l.ExpiresAt <= b.Now.Unix() || l.ExpiresAt <= l.CreatedAt || l.ExpiresAt-b.Now.Unix() > 86400 {
		return errors.New("simulator lease is expired or has invalid creation/expiry bounds")
	}
	if filepath.IsAbs(l.Project) || filepath.Clean(l.Project) != l.Project || l.Project == "." || strings.HasPrefix(l.Project, ".."+string(filepath.Separator)) || l.Project == ".." || strings.ContainsAny(l.Project, "\x00\r\n") || len(l.Project) > 1024 || (!strings.HasSuffix(l.Project, ".xcodeproj") && !strings.HasSuffix(l.Project, ".xcworkspace")) {
		return errors.New("simulator lease project must be a relative in-worktree Xcode project or workspace")
	}
	if strings.TrimSpace(l.Scheme) != l.Scheme || l.Scheme == "" || len(l.Scheme) > 128 || strings.ContainsAny(l.Scheme, "\x00\r\n") {
		return errors.New("simulator lease requires a bounded scheme")
	}
	return nil
}

// Steering describes only the external writes this one resource needs. Callers
// validate the binding and run Preflight first; the agent still has shell tools,
// so this wording is not executable confinement or a macOS permission grant.
func (l *Lease) Steering() string {
	return fmt.Sprintf(`
Explicit per-run simulator exception (prompt guidance, not OS enforcement):
- The operator supplied a new-owned default-registry simulator receipt for run %s, repository %s, uid %d, expiring at %s.
- Only this resource is excepted from the intentional external-write restriction: UDID %s; name %s; runtime %s; device type %s; controller %s (SHA256 %s).
- You may boot this exact UDID, wait for its bounded bootstatus, install/launch this run's source-bound app and test runner on it, and drive the accepted synthetic native cases. Address the exact UDID on every operation; never use a device-name shortcut or the booted alias.
- Creation/provisioning was performed separately. This allowance does not authorize another create, reset, erase, deletion, shutdown, cleanup, existing/foreign device control, personal phone, real account traffic, global service/config/cache/security changes, or broad home/disk access.
- Use the default registry with no custom-set flag; clear SIMULATOR_DEVICE_SET_PATH and DEVELOPER_DIR for these absolute native-tool invocations. Do not alter global environment/configuration.
- Project/source writes remain confined to the worktree. This exception is not an evidence-directory allowance. It changes no provider sandbox flags and cannot override an actual OS denial.
- Boot readiness, Xcode discovery, app installation/launch, live native scenarios, and accepted pipeline results are separate. An unavailable prerequisite stays blocked/inconclusive, never a code-failed or green claim; an actually executed application failure remains a real failure.
`, l.RunID, l.RepoID, *l.OwnerUID, time.Unix(l.ExpiresAt, 0).UTC().Format(time.RFC3339), l.UDID, l.Name, l.Runtime, l.DeviceType, l.Controller, l.ControllerSHA256)
}

type commandRunner func(context.Context, string, string, ...string) ([]byte, error)

const maxNativeOutput = 4 * 1024 * 1024

type nativeOutputBuffer struct {
	mu       sync.Mutex
	data     []byte
	overflow bool
}

func (b *nativeOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := maxNativeOutput - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	b.data = append(b.data, p...)
	return n, nil
}

// Use the default registry and paired absolute toolchain even if the daemon's
// login environment contains a custom-device-set/developer override. This is
// scoped to these read-only children; no global environment is changed.
func nativeEnvironment(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "SIMULATOR_DEVICE_SET_PATH" && key != "DEVELOPER_DIR" {
			out = append(out, entry)
		}
	}
	return out
}

func nativeOutput(ctx context.Context, cwd, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = nativeEnvironment(os.Environ())
	var output nativeOutputBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	shellenv.ConfigureShellCommand(cmd)
	err := shellenv.RunShellCommand(cmd)
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.overflow {
		return nil, errors.New("native prerequisite output exceeds the 4 MiB limit")
	}
	return output.data, err
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() > 256*1024*1024 {
		return "", errors.New("native controller is not a bounded regular file")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Preflight performs bounded read-only discovery, never boot/install/launch.
// Creation freshness is an explicit operator attestation; live inventory and
// destination rows independently verify target identity and discoverability.
func Preflight(ctx context.Context, l *Lease, b Binding, workDir string) error {
	return preflight(ctx, l, b, workDir, runtime.GOOS, hashFile, nativeOutput)
}

func preflight(ctx context.Context, l *Lease, b Binding, workDir, platform string, hash func(string) (string, error), run commandRunner) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.Validate(b); err != nil {
		return err
	}
	if platform != "darwin" {
		return errors.New("native simulator prerequisite unavailable: requires macOS")
	}
	project, err := filepath.EvalSymlinks(filepath.Join(workDir, l.Project))
	if err != nil {
		return fmt.Errorf("native simulator project unavailable: %w", err)
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, project)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("native simulator project escapes the worktree")
	}
	xcodebuild := filepath.Join(filepath.Dir(l.Controller), "xcodebuild")
	for _, tool := range []struct{ path, want string }{{l.Controller, l.ControllerSHA256}, {xcodebuild, l.XcodebuildSHA256}} {
		got, err := hash(tool.path)
		if err != nil || got != tool.want {
			return errors.New("native simulator controller identity changed or is unreadable")
		}
	}
	// The same bounded budget covers both native calls and process cleanup.
	raw, err := run(ctx, workDir, l.Controller, "list", "devices", "--json")
	if err != nil {
		return fmt.Errorf("native simulator default-registry inventory unavailable: %w", err)
	}
	var inventory struct {
		Devices map[string][]struct {
			UDID       string `json:"udid"`
			Name       string `json:"name"`
			DeviceType string `json:"deviceTypeIdentifier"`
			Available  bool   `json:"isAvailable"`
		} `json:"devices"`
	}
	if len(raw) > 4*1024*1024 || json.Unmarshal(raw, &inventory) != nil {
		return errors.New("native simulator inventory is unreadable")
	}
	matched := 0
	for rt, devices := range inventory.Devices {
		for _, d := range devices {
			if d.UDID != l.UDID {
				continue
			}
			if rt != l.Runtime || d.Name != l.Name || d.DeviceType != l.DeviceType || !d.Available {
				return errors.New("native simulator target is foreign, changed or unavailable")
			}
			matched++
		}
	}
	if matched != 1 {
		return errors.New("native simulator newly owned target is missing or ambiguous")
	}
	kind := "-project"
	if strings.HasSuffix(l.Project, ".xcworkspace") {
		kind = "-workspace"
	}
	raw, err = run(ctx, workDir, xcodebuild, kind, project, "-scheme", l.Scheme, "-showdestinations", "-destination-timeout", "5")
	if err != nil {
		return fmt.Errorf("native simulator Xcode destination discovery unavailable: %w", err)
	}
	if len(raw) > 4*1024*1024 {
		return errors.New("native simulator Xcode destination listing exceeds the output limit")
	}
	if !destinationFound(string(raw), l.UDID) {
		return errors.New("native simulator UDID is absent from Xcode destination rows")
	}
	return nil
}

func destinationFound(output, udid string) bool {
	available := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Available destinations for ") || strings.HasPrefix(line, "Destinations compatible with ") {
			available = true
			continue
		}
		if strings.HasSuffix(line, ":") {
			// Ineligible, incompatible or any unrecognized section never counts.
			available = false
			continue
		}
		if !available {
			continue
		}
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		fields := map[string]string{}
		for _, part := range strings.Split(strings.Trim(line, "{}"), ",") {
			k, v, ok := strings.Cut(part, ":")
			if ok {
				fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		if fields["platform"] == "iOS Simulator" && fields["id"] == udid && fields["error"] == "" {
			return true
		}
	}
	return false
}

func ReadFile(path string) (*Lease, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("--test-simulator-lease-file requires a file path")
	}
	before, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read simulator lease: %w", err)
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("simulator lease must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read simulator lease: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("simulator lease must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	return Decode(raw)
}
