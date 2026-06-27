# OMP runs as a Pi-protocol launch variant, not a new agent type

## Context

Multica's `piBackend` (`server/pkg/agent/pi.go`) was written against
Pi-mono (`@earendil-works/pi-coding-agent`). We need to support OMP
(`@oh-my-pi/pi-coding-agent`, the "Oh My Pi" distribution) because it is
the CLI installed on the operator's machine. OMP shares the Pi session-v3
JSON event-stream protocol but diverges on three surfaces: session flags,
skill discovery paths, and tool-call event encoding.

## Decision

Reuse the existing `pi` protocol family. Do **not** register a new
`agent.SupportedTypes` entry, do **not** add a DB migration, and do **not**
create a parallel `omp.go` backend. Instead, detect the OMP executable at
launch time inside `buildPiArgs` / `skillsDirPath` and branch the
parameters. A custom runtime profile (`protocol_family=pi`,
`command_name=omp`) routes tasks to the OMP binary while keeping the
`piBackend` event parser as the single owner of stream decoding.

Three concrete consequences:

1. **Session resume** — OMP has no `--session <path>` flag. The daemon
   passes `--session-dir {workdir}/.omp-sessions/` so OMP writes its
   session JSONL into a task-scoped directory instead of its default
   `~/.omp/agent/sessions/{cwd-slug}/`. On continuation the daemon scans
   that directory (one file, no collision) and passes
   `--resume <path>`. This replaces multica's current `--session` +
   `ensurePiSessionFile` pre-creation flow for the OMP branch.

2. **Skill discovery** — OMP does not scan `.pi/skills/`. The pi branch of
   `skillsDirPath` writes agent-bound skills to `.agents/skills/` (which
   OMP's `agents` provider discovers by default). `local_skills.go` gains
   `~/.omp/agent/skills` and `~/.claude/skills` as user-level scan roots
   so the multica UI surfaces OMP-discoverable skills.

3. **Tool-call visibility is deferred (v1)** — OMP encodes tool calls as
   `assistantMessageEvent.type=toolcall_start/delta/end` inside
   `message_update` events, whereas multica's `piStreamEvent` expects
   top-level `type=tool_execution_start` with structured `toolName`/`args`.
   v1 accepts this gap: OMP tool execution is invisible in the multica UI.

## Rationale

- A new supported type would touch `agent.SupportedTypes`, the
  `runtime_profile.protocol_family` CHECK constraint (migration 120),
  `execenv/context.go`, `runtime_config.go`, `local_skills.go`, and
  `cmd_runtime_profile.go`'s validator — 6+ files plus a DB migration —
  for a divergence that lives entirely in launch parameters and file
  paths. The event parser is shared.
- The runtime profile mechanism (`MUL-3284`) already exists for exactly
  this case: same protocol family, different command binary.
- The session-v3 JSON wire format is identical between Pi-mono and OMP
  (verified by event-distribution analysis of a live OMP session file).
  Only the launch contract and side-channel conventions differ.

## Consequences

- `pi.go` gains an executable-name branch in `buildPiArgs` and
  `piBlockedArgs`. The `--session` blocked-arg guard applies to the
  Pi-mono branch only.
- `ensurePiSessionFile` is skipped on the OMP branch (OMP creates its own
  session file; pre-creating an empty one would confuse `--resume`).
- v1 OMP tasks have no tool-call visibility. A follow-up can add a
  `toolcall_*` event parser to the pi stream decoder without changing the
  launch-variant decision.
- OMP error classification lands as `unknown` in `taskfailure.Classify`
  until omp-specific patterns are added (deferred; not a v1 blocker).
