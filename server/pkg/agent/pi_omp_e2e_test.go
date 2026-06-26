package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOMPScript is a stand-in for the `omp` CLI that honours the
// --session-dir / --resume contract and emits a session-v3 event stream.
// On a first run (no --resume) it writes a jsonl into the session dir;
// on a resume it echoes a "resumed:<basename>" text delta so the test
// can confirm the resume path was taken.
const fakeOMPScript = `#!/bin/sh
SESSION_DIR=""
RESUME=""
PROMPT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session-dir) SESSION_DIR="$2"; shift 2;;
    --resume) RESUME="$2"; shift 2;;
    --mode|-p) shift; [ "$1" = "json" ] && shift 2>/dev/null || true;;
    --provider|--model|--append-system-prompt) shift 2;;
    *) PROMPT="$1"; shift;;
  esac
done
[ -n "$SESSION_DIR" ] && mkdir -p "$SESSION_DIR"
if [ -n "$RESUME" ]; then
  printf '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"resumed:%s"}}\n' "$(basename "$RESUME")"
else
  F="$SESSION_DIR/$(date -u +%Y%m%dT%H%M%S)_fake.jsonl"
  printf '{"type":"session","version":3}\n' > "$F"
  printf '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"first-run"}}\n'
fi
printf '{"type":"turn_end","message":{"role":"assistant","model":"omp-test","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2}}}\n'
`

// TestPiBackendOMPLaunchAndResume is an end-to-end check of the OMP
// launch variant: the daemon creates a task-scoped --session-dir, OMP
// writes a jsonl there, the daemon resolves it as the SessionID, and a
// second run with that SessionID resumes via --resume. Exercises
// verification steps 1 (launch compatibility) and 2 (session resume)
// from docs/omp-adaptation.md.
func TestPiBackendOMPLaunchAndResume(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end OMP harness")
	}
	fakePath := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fakePath, []byte(fakeOMPScript))

	work := t.TempDir()
	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}

	drain := func(sess *Session) Result {
		go func() {
			for range sess.Messages {
			}
		}()
		select {
		case r, ok := <-sess.Result:
			if !ok {
				t.Fatal("result channel closed")
			}
			return r
		case <-time.After(15 * time.Second):
			t.Fatal("timeout waiting for result")
			return Result{}
		}
	}

	// First run: no resume id. The daemon must create the task-scoped
	// .omp-sessions dir and resolve the jsonl OMP wrote as SessionID.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := backend.Execute(ctx, "hello", ExecOptions{Cwd: work, Timeout: 12 * time.Second})
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	first := drain(sess)
	if first.Status != "completed" {
		t.Fatalf("first run status=%q err=%q", first.Status, first.Error)
	}
	if first.SessionID == "" || !strings.HasSuffix(first.SessionID, ".jsonl") {
		t.Fatalf("first run should resolve an OMP jsonl as SessionID, got %q", first.SessionID)
	}
	if _, err := os.Stat(filepath.Join(work, ".omp-sessions")); err != nil {
		t.Fatalf("task-scoped .omp-sessions not created: %v", err)
	}

	// Second run: hand back the resolved SessionID; the fake omp echoes
	// "resumed:<basename>" so we can confirm --resume was wired through.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	sess2, err := backend.Execute(ctx2, "again", ExecOptions{Cwd: work, Timeout: 12 * time.Second, ResumeSessionID: first.SessionID})
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	second := drain(sess2)
	if second.Status != "completed" {
		t.Fatalf("second run status=%q err=%q", second.Status, second.Error)
	}
	want := "resumed:" + filepath.Base(first.SessionID)
	if !strings.Contains(second.Output, want) {
		t.Fatalf("resume output missing %q; got %q", want, second.Output)
	}
}
