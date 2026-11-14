package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A real owned helper waits until the second inspection and then finishes
// naturally. Only inspecting irrelevant rows or command text is made slow.
// The oracle is the helper's completion marker inside the fixed settle bound,
// plus the completed result from the public native-agent interface.
func TestNativeCompletionObserverCounterfactual(t *testing.T) {
	for _, mode := range []string{"unrelated-scan", "command-label"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, "release.fifo")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			leader := filepath.Join(dir, "leader.pid")
			first := filepath.Join(dir, "first-probe")
			released := filepath.Join(dir, "released")
			finished := filepath.Join(dir, "helper-finished")
			snapshot := filepath.Join(dir, "snapshot")
			observationCancelled := filepath.Join(dir, "observation-cancelled")
			argsFile := filepath.Join(dir, "inspection.args")
			inspector := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
leader=$(cat %q)
for field do :; done
if [ ! -e %q ]; then
  COMMAND_MODE=unix2003 /bin/ps -g "$leader" -o "$field" > %q
  : > %q
  cat %q
  exit 0
fi
if [ ! -e %q ]; then
  : > %q
  printf 'finish\n' > %q
fi
case %q in
  unrelated-scan) if [ "$1" = -eo ]; then trap 'touch %q; exit 1' TERM; sleep 5; fi ;;
  command-label) case "$field" in *command=*|*comm=*) trap 'touch %q; exit 1' TERM; sleep 5 ;; esac ;;
esac
COMMAND_MODE=unix2003 exec /bin/ps -g "$leader" -o "$field"
`, argsFile, leader, first, snapshot, first, snapshot, released, released, fifo, mode, observationCancelled, observationCancelled)
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(inspector), 0700); err != nil {
				t.Fatal(err)
			}
			nativeScript := fmt.Sprintf(`#!/bin/sh
cat >/dev/null
echo $$ > %q
/bin/sh -c 'IFS= read -r release < "$1"; : > "$2"' helper %q %q >/dev/null 2>&1 &
printf '%%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
exit 0
`, leader, fifo, finished)
			bin := filepath.Join(dir, "codex")
			if err := os.WriteFile(bin, []byte(nativeScript), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			native, err := agent.New(types.AgentCodex, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			start := time.Now()
			result, err := native.Run(ctx, agent.RunOpts{Prompt: "return the completed result", CWD: dir, JSONSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`), RequireCompletedChildren: true})
			done, statErr := os.Stat(finished)
			if statErr != nil {
				t.Fatalf("helper did not naturally finish: %v; agent error: %v", statErr, err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				cancelled, cancelErr := os.Stat(observationCancelled)
				if cancelErr != nil || !done.ModTime().Before(cancelled.ModTime()) {
					t.Fatalf("fixture did not prove natural helper completion before the observer deadline: %v", cancelErr)
				}
				t.Logf("helper finished %v before the actual observer cancellation", cancelled.ModTime().Sub(done.ModTime()))
			}
			args, readErr := os.ReadFile(argsFile)
			if readErr != nil || len(strings.Split(strings.TrimSpace(string(args)), "\n")) < 2 {
				t.Fatalf("fixture did not exercise a good probe followed by another inspection: %v %q", readErr, args)
			}
			t.Logf("helper naturally finished in %v; invocation returned in %v; mode=%s", done.ModTime().Sub(start), time.Since(start), mode)
			if err != nil || result == nil || string(result.Output) != `{"ok":true}` {
				t.Fatalf("completed checks rejected by irrelevant inspection work: result=%+v err=%v", result, err)
			}
		})
	}
}
