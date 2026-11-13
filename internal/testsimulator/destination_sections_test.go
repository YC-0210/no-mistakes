package testsimulator

import (
"context"
"strings"
"testing"
)

// The compatible/incompatible labels and row layout come from the retained
// Xcode destination output. All device/name/scheme values below are synthetic.
func TestPreflight_CapturedXcodeDestinationSections(t *testing.T) {
l, binding, dir := leaseFixture(t)
row := "  { platform:iOS Simulator, arch:arm64, id:" + l.UDID + ", OS:26.2, name:" + l.Name + " }\n"
legacy := "Available destinations for the \"App\" scheme:\n"
compatible := "Destinations compatible with the \"App\" scheme:\n"
ineligible := "Ineligible destinations for the \"App\" scheme:\n"
incompatible := "Destinations incompatible with the \"App\" scheme:\n"
foreign := strings.ReplaceAll(row, l.UDID, "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
cases := []struct {
name, output  string
wantAvailable bool
}{
{"legacy", legacy + row, true},
{"captured-compatible", compatible + "\n" + row + "\n" + incompatible, true},
{"legacy-then-incompatible-target", legacy + foreign + incompatible + row, false},
{"compatible-then-incompatible-target", compatible + foreign + incompatible + row, false},
{"compatible-then-legacy-ineligible-target", compatible + foreign + ineligible + row, false},
{"incompatible-only", incompatible + row, false},
{"compatible-after-incompatible", incompatible + foreign + compatible + row, true},
{"foreign-only", compatible + foreign, false},
{"unlabelled", row, false},
{"echo-only", "xcodebuild -destination id=" + l.UDID, false},
{"unknown-section", "Unknown destinations:\n" + row, false},
{"unknown-after-legacy", legacy + foreign + "Unknown destinations:\n" + row, false},
{"unknown-after-compatible", compatible + foreign + "Unknown destinations:\n" + row, false},
{"wrong-platform", compatible + strings.ReplaceAll(row, "platform:iOS Simulator", "platform:iOS"), false},
{"error-row", compatible + strings.ReplaceAll(row, " }", ", error:runtime unavailable }"), false},
{"UUID-prefix-only", compatible + strings.ReplaceAll(row, l.UDID, l.UDID+"-extra"), false},
{"malformed-row", compatible + strings.ReplaceAll(row, " }", ""), false},
{"empty-compatible", compatible, false},
}
for _, tc := range cases {
t.Run(tc.name, func(t *testing.T) {
err := preflight(context.Background(), l, binding, dir, "darwin", fixtureHash(l), nativeFixture(l, l.UDID, l.Name, tc.output))
if (err == nil) != tc.wantAvailable {
t.Fatalf("preflight error = %v; want available %v", err, tc.wantAvailable)
}
})
}
}
