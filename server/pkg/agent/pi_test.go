package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBuildPiArgsNoToolAllowlist(t *testing.T) {
	// Extension tools registered via Pi's registerTool() must not be
	// filtered out by a hardcoded --tools allowlist. Omitting --tools
	// lets Pi use its full tool registry. See #2379.
	args := buildPiArgs("test prompt", "/tmp/session.jsonl", "", false, ExecOptions{}, slog.Default())
	for i, arg := range args {
		if arg == "--tools" {
			t.Errorf("buildPiArgs emits --tools %q; should not restrict tool registry (see #2379)", args[i+1])
		}
	}
}

func TestBuildPiArgsBasicFlags(t *testing.T) {
	args := buildPiArgs("hello world", "/tmp/s.jsonl", "", false, ExecOptions{
		Model:        "anthropic/claude-sonnet-4-20250514",
		SystemPrompt: "be helpful",
	}, slog.Default())

	joined := strings.Join(args, " ")
	for _, want := range []string{"-p", "--mode json", "--session /tmp/s.jsonl", "--provider anthropic", "--model claude-sonnet-4-20250514", "--append-system-prompt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %v", want, args)
		}
	}

	// Prompt must be the last positional argument.
	if args[len(args)-1] != "hello world" {
		t.Errorf("prompt should be last arg, got %q", args[len(args)-1])
	}
}

func TestBuildPiArgsCustomArgsAppended(t *testing.T) {
	// Users can still restrict tools via custom_args if desired.
	args := buildPiArgs("prompt", "/tmp/s.jsonl", "", false, ExecOptions{
		CustomArgs: []string{"--tools", "read,bash"},
	}, slog.Default())

	found := false
	for i, arg := range args {
		if arg == "--tools" && i+1 < len(args) && args[i+1] == "read,bash" {
			found = true
		}
	}
	if !found {
		t.Errorf("custom --tools should pass through via custom_args, got: %v", args)
	}
}

// TestPiExecuteAttachesStdinPipe verifies that the Pi backend spawns the
// child with an explicit stdin pipe (FIFO) instead of leaving cmd.Stdin
// nil. Without an explicit pipe, Pi has been observed to block under
// systemd waiting for stdin events (#2188); attaching and immediately
// closing a pipe delivers a clean EOF on a FIFO and unblocks Pi.
//
// The probe is structural rather than behavioral: a shell script in
// place of `pi` inspects /proc/self/fd/0 and only emits a valid event
// stream if stdin is a FIFO. If the fix regresses (stdin nil → /dev/null
// char device), the fake exits non-zero and the test fails.
func TestPiExecuteAttachesStdinPipe(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		// /proc/self/fd/0 is Linux-specific; skipping elsewhere keeps
		// the assertion portable without losing CI coverage.
		t.Skip("stdin fd inspection relies on /proc/self/fd/0")
	}

	fakePath := filepath.Join(t.TempDir(), "pi")
	script := "#!/bin/sh\n" +
		"kind=$(stat -c '%F' -L /proc/self/fd/0 2>/dev/null || echo unknown)\n" +
		"case \"$kind\" in\n" +
		"  fifo|*pipe*)\n" +
		"    printf '%s\\n' '{\"type\":\"agent_start\"}'\n" +
		"    printf '%s\\n' '{\"type\":\"turn_end\",\"message\":{\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input\":1,\"output\":1,\"cacheRead\":0,\"cacheWrite\":0,\"totalTokens\":2}}}'\n" +
		"    exit 0\n" +
		"    ;;\n" +
		"esac\n" +
		"printf 'stdin was %s; expected fifo\\n' \"$kind\" >&2\n" +
		"exit 1\n"
	writeTestExecutable(t, fakePath, []byte(script))

	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result, ok := <-session.Result:
		if !ok {
			t.Fatal("result channel closed without a value")
		}
		if result.Status != "completed" {
			t.Fatalf("expected status=completed (stdin attached as fifo), got %q (error=%q)", result.Status, result.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

func TestBuildPiArgsOMPFirstRun(t *testing.T) {
	// OMP first run: --session-dir under the workdir, no --session, no --resume.
	args := buildPiArgs("hi", "", "/work/.omp-sessions", true, ExecOptions{Cwd: "/work"}, slog.Default())
	joined := strings.Join(args, " ")
	for _, want := range []string{"--session-dir /work/.omp-sessions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %v", want, args)
		}
	}
	if strings.Contains(joined, "--session ") {
		t.Errorf("OMP must not emit --session, got: %v", args)
	}
	if strings.Contains(joined, "--resume") {
		t.Errorf("OMP first run must not emit --resume, got: %v", args)
	}
	if args[len(args)-1] != "hi" {
		t.Errorf("prompt should be last arg, got %q", args[len(args)-1])
	}
}

func TestBuildPiArgsOMPResume(t *testing.T) {
	// OMP resume: --session-dir plus --resume pointing at the resolved jsonl.
	args := buildPiArgs("hi", "/work/.omp-sessions/20260101T000000.000_abc.jsonl", "/work/.omp-sessions", true, ExecOptions{Cwd: "/work"}, slog.Default())
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--session-dir /work/.omp-sessions") {
		t.Errorf("expected --session-dir, got: %v", args)
	}
	if !strings.Contains(joined, "--resume /work/.omp-sessions/20260101T000000.000_abc.jsonl") {
		t.Errorf("expected --resume <jsonl>, got: %v", args)
	}
}

func TestBuildPiArgsOMPBlocksResumeCustomArg(t *testing.T) {
	// User custom_args must not override the daemon-managed --session-dir/--resume.
	args := buildPiArgs("hi", "", "/work/.omp-sessions", true, ExecOptions{
		Cwd:        "/work",
		CustomArgs: []string{"--resume", "user/managed.jsonl", "--continue"},
	}, slog.Default())
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "user/managed.jsonl") {
		t.Errorf("custom --resume must be blocked, got: %v", args)
	}
	if strings.Contains(joined, "--continue") {
		t.Errorf("--continue must be blocked on OMP, got: %v", args)
	}
}

func TestIsOMPExecutable(t *testing.T) {
	cases := map[string]bool{
		"omp":                  true,
		"/usr/local/bin/omp":   true,
		"omp.exe":              true,
		"./omp.exe":            true,
		"pi":                   false,
		"/home/u/.nvm/.../pi":  false,
		"":                     false,
		"omp-cli":              false,
	}
	for in, want := range cases {
		if got := isOMPExecutable(in); got != want {
			t.Errorf("isOMPExecutable(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestScanOMPSessionFile(t *testing.T) {
	dir := t.TempDir()
	// Empty dir: no session yet.
	if _, ok := scanOMPSessionFile(dir); ok {
		t.Fatal("expected ok=false on empty dir")
	}
	// Non-jsonl files are ignored.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := scanOMPSessionFile(dir); ok {
		t.Fatal("non-jsonl must be ignored")
	}
	// A jsonl is picked up.
	session := filepath.Join(dir, "20260101T000000.000_abc.jsonl")
	if err := os.WriteFile(session, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := scanOMPSessionFile(dir)
	if !ok {
		t.Fatal("expected ok=true after writing a jsonl")
	}
	if got != session {
		t.Errorf("got %q, want %q", got, session)
	}
	// Newest wins.
	newer := filepath.Join(dir, "20260102T000000.000_def.jsonl")
	if err := os.WriteFile(newer, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ensure mtime ordering on filesystems with coarse mtime resolution.
	os.Chtimes(newer, time.Now(), time.Now().Add(time.Second))
	got, _ = scanOMPSessionFile(dir)
	if got != newer {
		t.Errorf("expected newest jsonl %q, got %q", newer, got)
	}
}

func TestStripPiToolCallMarkup(t *testing.T) {
	tests := map[string]string{
		`before call:bash{command:<|"|>cd repo/path && ls -F<|"|>}<tool_call|> after`:                           "before  after",
		`before call:read{path:<|"|>repo/path/roles/example/verify.yml<|"|>} after`:                             "before  after",
		`before response:bash{command:<|"|>multica issue comment list issue-id --all --output json<|"|>} after`: "before  after",
		`before call:bash{command:<|"|>printf '{"key":"value"}'<|"|>} after`:                                    "before  after",
		`before <|turn>model after`: "before  after",
	}
	for in, want := range tests {
		got := stripPiToolCallMarkup(in)
		if got != want {
			t.Fatalf("unexpected stripped text: %q, want %q", got, want)
		}
	}
}

func TestDrainPiTextBufferSplitToolCall(t *testing.T) {
	chunks := []string{
		"before ca",
		`ll:bash{command:<|"|>ls -R repo/path`,
		`/roles/example<|"|>}`,
		" after",
	}
	var buf strings.Builder
	var got strings.Builder
	for _, chunk := range chunks {
		got.WriteString(drainPiTextBuffer(&buf, chunk))
	}
	got.WriteString(flushPiTextBuffer(&buf))
	if got.String() != "before  after" {
		t.Fatalf("unexpected streamed text: %q", got.String())
	}
}

func TestDrainPiTextBufferSplitControlToken(t *testing.T) {
	chunks := []string{"before <|tu", "rn>model after"}
	var buf strings.Builder
	var got strings.Builder
	for _, chunk := range chunks {
		got.WriteString(drainPiTextBuffer(&buf, chunk))
	}
	got.WriteString(flushPiTextBuffer(&buf))
	if got.String() != "before  after" {
		t.Fatalf("unexpected streamed text: %q", got.String())
	}
}

func TestFlushPiTextBufferKeepsUnmatchedToolPrefixes(t *testing.T) {
	tests := []string{
		"plain response: see below",
		"plain call: see below",
		`plain call:bash{command:<|"|>unterminated`,
	}
	for _, want := range tests {
		var buf strings.Builder
		got := drainPiTextBuffer(&buf, want)
		got += flushPiTextBuffer(&buf)
		if got != want {
			t.Fatalf("unexpected flushed text: %q, want %q", got, want)
		}
	}
}
