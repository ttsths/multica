# Multica Agent Runtime Adaptation

The domain of adapting external coding-agent CLIs (Pi, OMP) to run as first-class
task executors inside Multica's daemon-driven agent platform.

## Language

**Runtime Protocol Family**:
The backend abstraction selected by `agent.New(type)` that owns event-stream parsing and launch semantics. Whitelisted in `agent.SupportedTypes` and enforced by the `runtime_profile.protocol_family` CHECK constraint (migration 120).
_Avoid_: provider, backend, engine

**OMP**:
The Oh My Pi CLI distribution (`@oh-my-pi/pi-coding-agent`), a downstream fork of the Pi coding agent with a rewritten discovery layer and `.omp/` config root. Shares the Pi session-v3 JSON event-stream protocol but diverges on session flags and skill discovery paths.
_Avoid_: pi-mono, Oh My Pi (use OMP as the canonical short name in code)

**Pi-mono**:
The original upstream Pi coding agent (`@earendil-works/pi-coding-agent`). The target multica's `piBackend` was originally written against.
_Avoid_: pi, original pi

**Protocol Reuse (OMP adaptation)**:
The decision to implement OMP support by reusing the existing `pi` protocol family rather than registering a new top-level type. The daemon detects the OMP executable at launch time and branches inside `buildPiArgs` and `skillsDirPath` instead of adding a parallel backend.
_Avoid_: omp backend, omp provider, omp type

**Task-Scoped Session Directory**:
A per-task directory (`{workdir}/.omp-sessions/`) passed via OMP's `--session-dir` flag so OMP writes session JSONL into an isolated location instead of its default `~/.omp/agent/sessions/{cwd-slug}/`. Eliminates concurrent-task session collision and gives multica a deterministic resume path.
_Avoid_: session store, session cache

**Skill Injection Path**:
The workdir-relative directory where multica writes agent-bound skill files for the runtime CLI to discover natively. OMP scans `.agents/skills/` and `.claude/skills/` but not `.pi/skills/`.
_Avoid_: skill directory, skill root

**Runtime Brief**:
The auto-managed instruction block (`# Multica Agent Runtime`) written into `AGENTS.md` (for pi/omp) or `CLAUDE.md` (for claude) bounded by `BEGIN/END MULTICA-RUNTIME` markers. Teaches the agent the `multica` CLI surface. Also delivered via `--append-system-prompt`.
_Avoid_: system prompt, meta skill (the brief is a subset of the meta-skill content)

**Launch Variant**:
The per-executable parameter set chosen inside `buildPiArgs` based on the resolved binary name. Pi-mono receives `--session <path>`; OMP receives `--session-dir <dir>` and `--resume <path>` on continuation. Both receive the shared `-p --mode json` base.
_Avoid_: omp mode, pi mode

**Tool Call Visibility Gap (v1 acceptance)**:
The explicitly accepted limitation that OMP tool calls (`assistantMessageEvent.type=toolcall_*` in the JSON stream) are not parsed in v1. OMP tool execution is invisible in the multica UI. Tracked as deferred work, not a regression.
_Avoid_: tool bug, tool parsing defect
