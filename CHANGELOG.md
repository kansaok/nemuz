# Changelog

All notable changes to nemuz are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**Before 1.0, minor versions may break things.** The journal format, the plugin
protocol, and the skill file layout are all still settling. Breaking changes are
listed under **Changed** with what to do about them.

## [Unreleased]

Nothing yet.

## [0.4.0] — 2026-09-08

nemuz becomes importable, and its claims become enforced.

### Added

- **A public API.** The root package `github.com/kansaok/nemuz` exposes
  `Open`, `Run`, `Replay`, `Turns`, `Events`, `Remember`, `Recall`, `Forget`,
  `Skills` and `OpenProvider`, plus the `Tool` and `Provider` interfaces to
  implement. Until now every package was under `internal/`, which meant nobody
  could build on nemuz at all — a framework nobody can import is not a
  framework. Its tests live in an external test package, so anything they need
  is by definition part of the API.
- **CI.** Three jobs: build and test with the race detector; the same suite with
  outbound traffic cut at the firewall, which turns "no test touches the
  network" from an intention into a fact; and a job that runs `nemuz doctor` and
  requires it to report the sandbox verified.
- **A gofmt check**, which immediately found two files that were not formatted.

### Fixed

- **A plugin that died during startup lost its own error message.** `cmd.Wait`
  ran concurrently with the goroutines reading the plugin's pipes, so Wait
  closed them out from under the readers and the host reported a broken pipe
  instead of what the plugin had said. Go's own documentation warns against
  this; the race detector under CI-like load is what surfaced it. Wait now runs
  only after both pipes are drained.
- **A flaky plugin test.** Two probes slept for the same duration as the
  handshake timeout, so on a loaded machine they exited at the moment the host
  gave up. They now block on stdin, which is what a real plugin does.

### Changed

- Removed a stale roadmap line that claimed memory, skills and the eval gate
  were still outstanding.

## [0.3.0] — 2026-09-08

The agent starts learning without being asked.

### Added

- **Memory.** Facts the agent keeps from past turns, stored one Markdown file
  each with YAML frontmatter, and recalled into the system prompt by relevance.
  Recall is deterministic — the same query against the same store always returns
  the same memories in the same order — because memories go into the prompt, and
  a recall that varied between runs would make every turn unreplayable for
  reasons unrelated to the agent.
- **Background review.** After a turn, a second smaller turn decides whether
  anything was worth keeping. It is journaled as its own turn, so every decision
  is auditable, and it runs on a separate model call so the conversation's
  prompt cache is untouched.
- **A two-tool reviewer.** The reviewer can `remember` a fact and `draft_skill`.
  It cannot read files, run commands, or reach the network, because those tools
  are not in its registry — a structural guarantee rather than a prompt asking
  it to behave. Drafted skills are quarantined; the reviewer proposes and the
  eval gate decides.
- **Novelty filtering.** Turn shapes are fingerprinted from their tools and the
  distinctive words of their prompt — deliberately not the answer — and a shape
  reviewed recently is skipped. Hermes Agent reviews after every turn; this is
  where nemuz diverges, because paying to rediscover that the twentieth "run the
  tests" of the afternoon taught nothing is pure waste.
- **`nemuz memory`** — `ls`, `show`, `recall`, `forget`, `pin`, `unpin`.
  `recall` runs exactly what a turn would run, so a surprising recall can be
  traced.
- **`--review`, `--review-model`, `--memories`** on `nemuz run`. Review is on by
  default; the novelty ledger is what makes that affordable.

### Changed

- `turn.start` now records the ids of the memories recalled into the prompt, so
  a turn's answer can be traced to what the agent had been reminded of.
- Memories are deletable, unlike skills, which archive. A skill's removal changes
  what the agent can do, so it stays reversible; a wrong fact should simply stop
  being there rather than linger where the agent still believes it.

## [0.2.0] — 2026-09-08

The sandbox stops being a claim and becomes a fact.

### Added

- **Sandboxed tool worker.** The built-in tools now run in a separate process
  (`nemuz tool-worker`) that applies Landlock to itself before serving anything,
  over the same JSON-RPC protocol plugins use. Landlock is a thread credential,
  so confining tool execution requires a process whose whole job is to be
  confined — and it cannot be the gateway, which needs the journal, the config,
  and the network the tools must not have.
- **`--sandbox` flag** on `run`, `replay`, `skill eval` and `skill certify`,
  with modes `on` (require confinement), `auto` (confine where possible, say so
  when not) and `off` (explicit, and recorded).
- **`nemuz doctor` now verifies confinement** instead of reporting kernel
  support. It spawns the worker and has it attempt a read outside its workspace,
  so a build that computes the right grants and never applies them fails here
  and passes nowhere else.
- **`plugin.Server`** — the serving half of the plugin protocol, so nemuz can
  speak it to itself. Go plugin authors can use it directly.
- **`Client.ToolsAs`** — register a plugin's tools under a chosen namespace, or
  none. Used so the built-in tools keep their canonical names when routed
  through the protocol.
- **`Agent.Environment`** — string pairs recorded in `turn.start`, currently
  carrying which sandbox applied. Values must be identical across runs of a turn
  or the journal digest would change.
- 8 tests covering the wiring, including one that confirms an *unsandboxed*
  worker is *not* refused — without it, a probe that always reported "denied"
  would look identical to one that worked.

### Changed

- `run`, `replay` and the eval gate now share one toolset builder, replacing
  three copies of the same registration code.
- `turn.start` gained an optional `env` field. Turns recorded by 0.1.0 replay
  normally; turns recorded by 0.2.0 will not strict-replay under 0.1.0.

### Fixed

- **The sandbox was never applied.** 0.1.0 implemented and tested
  `sandbox.Restrict`, and computed the capability union of the registered tools,
  but nothing on the production path called either. Tool confinement rested
  entirely on the workspace guard in Go. This was noted in the 0.1.0 commit
  message and is now closed.

## [0.1.0] — 2026-09-08

First working version: a journal, a replay engine, and the pieces that hang off
them.

### Added

- **Journal and replay.** Every turn is written to an append-only JSONL file
  before anything observes it. A recorded turn replays against its recording and
  produces an identical digest. The digest deliberately excludes wall-clock time,
  because two runs of the same turn differ in timing and that must not count as
  a difference.
- **Content-addressed blob store.** Payloads over 4 KiB are stored by SHA-256
  and referenced by hash, so the journal stays readable and identical content
  costs nothing twice.
- **Turn loop** with a tool registry, capability declarations, step limits, and
  filesystem tools confined to a workspace.
- **Three provider wire formats covering thirteen vendors.** Anthropic and
  Gemini get real adapters because they genuinely differ; anything speaking the
  OpenAI shape is one line in a table. Gemini issues no tool-call ids, so the
  adapter synthesises stable ones — a random id would change the journal digest
  on every replay.
- **Tool plugins as separate processes** over newline-delimited JSON-RPC, with a
  TypeScript SDK and reference plugins in Go and JavaScript. A plugin's declared
  capabilities are a request; the host decides what is granted.
- **Learned skills with an eval gate.** Agent-written skills start in quarantine
  and reach active only by passing recorded scenarios, which replay for free.
  Nothing is ever deleted: archiving is reversible, and restoring returns a skill
  to quarantine rather than to service.
- **Landlock enforcement**, with tests confirming the kernel — not a check in Go
  — refuses an ungranted path.
- CLI: `run`, `replay`, `journal`, `plugin`, `skill`, `providers`, `doctor`.
- Size, startup, and file-length budgets enforced by `make`.

### Known limitations

- No memory, no idle curator, and no background review, so skills must still be
  written by hand. The gate that proves them works; the machinery that produces
  them does not exist yet.
- No chat channels.
- No HTTP API, no ACP, no seccomp, no CI.
- Linux only. Landlock has no equivalent on macOS or Windows yet.

[Unreleased]: https://github.com/kansaok/nemuz/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/kansaok/nemuz/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/kansaok/nemuz/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/kansaok/nemuz/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/kansaok/nemuz/releases/tag/v0.1.0
