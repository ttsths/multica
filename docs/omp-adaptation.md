# OMP runtime adaptation

Adapt the OMP CLI (`@oh-my-pi/pi-coding-agent`, the "Oh My Pi"
distribution) to run as a first-class agent runtime inside Multica, reusing
the existing `pi` protocol family.

This document is the implementation contract. The architectural rationale
lives in [ADR-0001](./adr/0001-omp-via-pi-protocol-reuse.md); domain
terminology lives in [CONTEXT.md](../CONTEXT.md).

## TL;DR

OMP shares Pi-mono's session-v3 JSON event-stream wire format but diverges
on three surfaces. We reuse the `pi` protocol family and branch inside
`buildPiArgs` / `skillsDirPath` based on the resolved executable name.
No new agent type, no DB migration.

## Background: what is OMP

OMP (`omp/16.x`) is a downstream fork of the Pi coding agent. The
operator's machine has `omp` at `/Users/ttsths/.bun/bin/omp` and the
older Pi-mono at `~/.nvm/.../bin/pi` (v0.80.2). Both speak the same
`session v3` JSON event protocol on stdout (`message_start` →
`message_update` → `message_end` → `turn_end` → `agent_end`), so
multica's `piBackend` event parser is reusable as-is.

The divergences that matter:

| Surface | Pi-mono | OMP |
|---|---|---|
| Session flag | `--session <path>` (file reused on resume) | no `--session`; uses `--session-dir <dir>` + `--resume <path>` / `--continue` |
| Session storage | file at the path multica manages | auto under `~/.omp/agent/sessions/{cwd-slug}/{ts}_{uuid}.jsonl` unless `--session-dir` overrides |
| Skill discovery | `.pi/skills/` (workdir), `~/.pi/skills` (user) | `.agents/skills/`, `.claude/skills/` (workdir walk-up); `~/.omp/agent/skills`, `~/.claude/skills` (user). Does **not** scan `.pi/skills`. |
| Tool-call events | top-level `type: tool_execution_start/end` with `toolName`/`args` | nested `assistantMessageEvent.type = toolcall_start/delta/end` inside `message_update`; args arrive as streaming JSON deltas |

## Decision: protocol reuse, not a new type

A new supported type would touch `agent.SupportedTypes`, the
`runtime_profile.protocol_family` CHECK constraint (migration 120),
`execenv/context.go`, `runtime_config.go`, `local_skills.go`, and the
CLI validator — 6+ files plus a DB migration — for a divergence that lives
entirely in launch parameters and file paths. The event parser is shared.

The runtime-profile mechanism (`MUL-3284`) already exists for exactly this
case: same protocol family, different command binary.

### Operator setup (no code change)

```sh
multica runtime profile create \
  --display-name "OMP" \
  --protocol-family pi \
  --command-name omp \
  --output json
```

If the daemon cannot find `omp` on `PATH` (common for desktop-launched
daemons), pin the absolute path per machine:

```sh
multica runtime profile set-path <profile-id> --path /Users/ttsths/.bun/bin/omp
```

Then bind agents to this profile's `runtime_id` as usual.

## Implementation contract

Five source edits, grouped by the three divergences. File:line references
are against the current tree; re-read before editing.

### 1. Session resume via `--session-dir` (the hardest piece)

**Files:** `server/pkg/agent/pi.go`

Pi-mono's flow is: multica pre-creates an empty file at a managed path
(`ensurePiSessionFile`, `pi.go:559`), passes `--session <path>`, and on the
next task for the same `(agent, issue)` pair reuses that path so the
runtime resumes the conversation. OMP has no `--session` flag at all.

**OMP flow:**

- multica creates a task-scoped directory `{workdir}/.omp-sessions/` and
  passes `--session-dir <dir>`. OMP writes its session JSONL there instead
  of its default `~/.omp/agent/sessions/{cwd-slug}/`.
- On continuation (the daemon already threads `PriorWorkDir` for the
  reuse path), multica scans `.omp-sessions/` — exactly one file, no
  collision possible — and passes `--session-dir <dir> --resume <path>`.

Task-scoping via `--session-dir` is load-bearing: without it, concurrent
tasks on the same agent share one `cwd-slug` and the "newest file" scan
race would cross-wire sessions between tasks (`max_concurrent_tasks`
defaults to 6).

**Edits in `pi.go`:**

| Symbol | Line | Change |
|---|---|---|
| `buildPiArgs` | `pi.go:494-522` | Detect `omp` executable. On the OMP branch: drop `--session`, emit `--session-dir {workdir}/.omp-sessions/`; on resume, append `--resume <resolved-path>`. Needs a new `workDir` parameter threaded from `ExecOptions`/`Config`. |
| `piBlockedArgs` | `pi.go:474-479` | The `--session` guard applies to the Pi-mono branch only. OMP branch blocks `--session-dir` and `--resume` instead. |
| `ensurePiSessionFile` | `pi.go:556-568` | Skip on the OMP branch. OMP creates its own session file; pre-creating an empty one would make `--resume` target an empty file. |
| `newPiSessionPath` caller | `pi.go` Execute | First run: `MkdirAll(.omp-sessions)`. Resume run: `os.ReadDir(.omp-sessions)` → pick the single `.jsonl`. |

**Workdir plumbing:** `buildPiArgs` currently takes `prompt,
sessionPath string, opts ExecOptions`. The OMP branch needs the task
workdir to construct the session-dir path. Add `workDir` to `ExecOptions`
(or read it from `Config`), then thread it through. `daemon.go` already
has `env.WorkDir` at the call site.

### 2. Skill discovery path (two files)

OMP's discovery layer (`src/discovery/agents.ts`, `src/discovery/claude.ts`)
scans `.agents/skills/` and `.claude/skills/` by default. It does **not**
scan `.pi/skills/`. Verified empirically: a `SKILL.md` placed at every
candidate OMP path was `Unknown skill` except under `~/.claude/skills/`
(which OMP's claude provider loads with `enableClaudeUser=true`).

**Edit A — injected skills (`server/internal/daemon/execenv/context.go`):**

`skillsDirPath` (`context.go:200-203`) currently returns
`{workDir}/.pi/skills` for `case "pi"`. Change it to
`{workDir}/.agents/skills` so OMP's `agents` provider discovers the
skills multica injects from the DB.

```go
case "pi":
    // OMP's agents provider scans .agents/skills/ (project walk-up).
    // Pi-mono scanned .pi/skills/; OMP does not.
    return filepath.Join(workDir, ".agents", "skills")
```

This change also applies to any remaining Pi-mono users: Pi-mono's own
docs reference `.pi/skills`, but since multica is standardizing on OMP and
the injected skills are multica-owned (not user-authored), moving the
injection target is safe. If Pi-mono compatibility must be preserved,
write to both directories (the cost is one extra `MkdirAll` + copy).

**Edit B — locally discovered skills (`server/internal/daemon/local_skills.go`):**

`localSkillRootsForProvider` (`local_skills.go:103-143`) returns
provider-specific user-level scan roots. Add OMP's roots so the multica UI
surfaces the same skills OMP will discover at runtime:

```go
// For the pi protocol family, scan the directories OMP actually reads.
// Keep ~/.pi/skills for backward compat with Pi-mono installs.
roots := []localSkillRoot{
    {path: expandHome("~/.pi/skills"), kind: "pi-user"},       // legacy
    {path: expandHome("~/.omp/agent/skills"), kind: "omp-user"},
    {path: expandHome("~/.claude/skills"), kind: "claude-user"},
    {path: expandHome("~/.agents/skills"), kind: "agents-user"},
}
```

The universal `~/.agents/skills` root is already documented as
cross-tool; OMP's `agents` provider scans it with
`enableAgentsUser=true` by default.

### 3. Runtime brief — no change needed

`runtimeConfigPath` (`runtime_config.go:186-194`) already maps `pi` →
`AGENTS.md`. OMP loads `AGENTS.md` as a context file natively. No edit
required.

The runtime brief is also delivered via `--append-system-prompt`
(`buildPiArgs:517`). This means the brief reaches the agent through two
channels: the `AGENTS.md` file and the flag. This is the existing
behavior for all pi-family runtimes and is not a regression. The
`BEGIN/END MULTICA-RUNTIME` marker block keeps the `AGENTS.md` injection
idempotent across re-runs in the same workdir.

## Out of scope (v1)

Explicitly deferred — tracked here so they are not silently dropped.

### Tool-call visibility

OMP encodes tool calls as `assistantMessageEvent.type =
toolcall_start/delta/end` inside `message_update` events, with tool
arguments arriving as streaming JSON deltas. multica's `piStreamEvent`
(`pi.go:396-415`) expects top-level `type: tool_execution_start/end`
with structured `toolName`/`args`. The two are incompatible.

**v1 acceptance:** OMP tool execution is invisible in the multica UI.
The agent still runs correctly — tools execute inside OMP — but operators
cannot see what the agent is doing in real time. A follow-up can add a
`toolcall_*` event parser to the pi stream decoder; the launch-variant
decision does not block it.

When implemented, the parser must buffer `toolcall_delta` fragments,
reconstruct the full JSON, then emit multica `Message{Type:
MessageTypeToolCall}` events. It cannot stream deltas directly because
multica's message model expects whole tool-call records.

### Error classification

`taskfailure.Classify` pattern-matches stderr/stdout text against known
signatures (timeout, oom, crash). OMP's error message format differs from
Pi-mono, so every OMP task failure currently classifies as `unknown`.
This affects observability only, not task execution.

### Cross-implementation session portability

OMP session JSONL carries a `type:session, version:3` header; Pi-mono
does not. The two formats are not interchangeable. This is not a problem
for the adapted flow because each runtime reads and writes its own
sessions — multica never asks OMP to resume a Pi-mono session or vice
versa. The `--session-dir` isolation makes this guarantee structural.

## Verification plan

Run these after implementation; they are the minimum evidence the
adaptation works end to end.

1. **Launch compatibility** — `omp -p --mode json --session-dir /tmp/x
   "say hi"` emits the standard session-v3 event stream. Confirms the
   flag combination is accepted.

2. **Session resume** — run twice into the same `--session-dir`; the
   second run with `--resume <first-run-jsonl>` must see prior context.

3. **Skill discovery** — place a `SKILL.md` at
   `{workdir}/.agents/skills/test/SKILL.md`; confirm OMP lists it and
   `skill://test` resolves.

4. **End to end** — create the runtime profile, bind an agent, assign an
   issue, and watch the daemon launch OMP. The issue comment/write-back
   path (`multica issue comment add`) must succeed. Tool calls will be
   invisible — that is the accepted v1 gap.

## File map

```
server/pkg/agent/pi.go                         # buildPiArgs, piBlockedArgs, ensurePiSessionFile
server/internal/daemon/execenv/context.go      # skillsDirPath, case "pi"
server/internal/daemon/local_skills.go         # localSkillRootsForProvider
server/internal/daemon/daemon.go               # threads workDir into ExecOptions (call site ~3260)
```

## References

- [ADR-0001 — OMP via Pi protocol reuse](./adr/0001-omp-via-pi-protocol-reuse.md)
- [CONTEXT.md — adaptation glossary](../CONTEXT.md)
- [Custom runtimes](./custom-runtimes.md) — operator-facing runtime-profile docs
- `server/pkg/agent/pi.go` — piBackend, the reused event parser
- `server/internal/daemon/execenv/runtime_config.go:186` — `runtimeConfigPath`
- OMP discovery sources: `src/discovery/{agents,claude,builtin}.ts` in
  `@oh-my-pi/pi-coding-agent`
