# Changelog

All notable changes to nemuz are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**Before 1.0, minor versions may break things.** The journal format, the plugin
protocol, and the skill file layout are all still settling. Breaking changes are
listed under **Changed** with what to do about them.

## [Unreleased]

Nothing yet.

## [0.12.0] — 2026-09-09

seccomp, layered under Landlock.

### Added

- **A hand-rolled seccomp filter**, applied in the confined tool worker
  alongside Landlock. Landlock says which files a process may touch; this says
  which syscalls it may make at all. Once `--allow-exec` lets an agent run real
  programs, that gap matters — Landlock's file rules say nothing about `ptrace`,
  `mount`, or loading a kernel module.

  It is a fixed denylist, not an allowlist — enumerating every syscall `go
  build` or `bash` might make is as impractical as enumerating binaries was for
  Landlock, and would break on first real use. The denylist covers process
  introspection and injection, filesystem namespace manipulation, kernel module
  and code loading, and a handful of privileged operations. Ordinary
  networking, deliberately, is not touched: distinguishing a raw socket from a
  normal one needs inspecting `socket()`'s arguments, which doubles the risk of
  a subtly wrong BPF program for a narrower win. Stated in the README as a gap,
  not hidden.

  Written by hand rather than via a cgo binding to libseccomp, the same choice
  already made for Landlock, metrics, and the journal: syscall numbers come
  from `golang.org/x/sys/unix`, which resolves them correctly per architecture
  at compile time, so the BPF program itself is the only part written from
  scratch.

- **`nemuz doctor` checks seccomp separately from Landlock.** A build that
  computes the right BPF program and never installs it, or installs it
  incorrectly, would pass every other check. The probe spawns the confined
  worker and has it attempt `ptrace(PTRACE_TRACEME)` — a call that succeeds by
  default, so any refusal at all proves the filter is doing something. Reported
  as a warning rather than a failure: an old kernel without
  `CONFIG_SECCOMP_FILTER` should not read as "the sandbox is broken" the way a
  Landlock leak would, since Landlock's file confinement still holds on its own.

- **The plugin protocol's `Manifest` gained an optional `Sandbox` field.**
  Whether seccomp actually installed is something only the worker process can
  know, so it reports its own achieved confinement — `landlock-v1+seccomp+exec`
  — back to the host through the same handshake that already carries its name
  and tools, rather than the host computing a status string before the worker
  has even run.

### Fixed, before it shipped

Real per-architecture testing caught two things a single-platform build would
have missed entirely:

- **`iopl` and `ioperm` do not exist on arm64.** They are x86-specific raw I/O
  port syscalls with no ARM equivalent; `unix.SYS_IOPL` simply is not a defined
  constant there. `make cross` — added in 0.11.1 for exactly this reason —
  caught the arm64 build failure before it could reach a release. The two
  syscalls now live in a small per-arch file, denied only where they exist.

## [0.11.1] — 2026-09-09

### Fixed

- **nemuz stopped compiling for macOS and Windows, and only the release said
  so.** The sandbox constants added in 0.11.0 lived in the Linux-only file, so
  every non-Linux target failed to build. CI compiles one target and passed;
  the release builds six and failed, which is the worst place for a portability
  break to surface.

  The non-Linux sandbox now declares the same names, empty, so callers compile
  everywhere. And `make cross` builds all six targets, in CI, so the next such
  break fails in a pull request rather than at a tag.

## [0.11.0] — 2026-09-09

The agent can run commands, and the sandbox still holds.

### Added

- **`run_command`**, offered only when `--allow-exec` names programs. Without it
  the agent can read, write and list, and cannot run anything — which is where
  0.10.1 left it, and why a skill the agent wrote for itself ("run make lint,
  make test, make bench") could not be carried out.

  **There is no shell.** Arguments go to the operating system directly: no
  pipes, no globs, no `&&`, no quoting to get wrong, and no way for an argument
  to become a second command.

  Programs are named rather than pathed and resolved with the operator's
  environment, so a toolchain under a home directory is found. Commands get a
  private scratch directory as `HOME` and `TMPDIR`, under the state directory
  rather than in the project.

### What the sandbox guarantees now

Even with exec fully allowed, a command may write to exactly two places: the
workspace and that scratch directory. The workspace is writable and **not**
executable, so an agent cannot write a script into the project and then run it.
Project scripts still work through an allowed interpreter.

Be clear about the allowlist, though: allowing `bash` or any compiler grants
arbitrary execution by construction. The allowlist limits what the model
casually reaches for; the sandbox bounds the damage. Only the second is worth
trusting, and the README now says so.

### Five things real use taught, none of which a test would have

Every one of these was found by pointing the agent at this repository and asking
it to check the project's health.

- **Granting execute on a binary is not enough to run it.** `/usr/bin/ls` is
  started by its ELF interpreter, and the kernel needs execute on the
  interpreter too — read is not enough. Since interpreters live under the
  library directories, allowing exec at all means allowing execute across the
  system tree. Granting `/usr/bin/ls` exactly fails; granting `/usr` works.
- **Landlock rejects directory rights on a file.** A rule for `/dev/null`
  carrying `MAKE_DIR` and `READ_DIR` fails with `EINVAL` — and fails the whole
  ruleset, not the one rule. Access is now masked to what a file can have.
- **`exec.Command` resolves programs with the parent's `PATH`, not `cmd.Env`.**
  The worker runs with an empty environment on purpose, so lookup found nothing
  at all. The host resolves names now and passes absolute paths.
- **A toolchain is not one file.** `go vet` runs `go/pkg/tool/.../vet`, so
  granting `go/bin` finds the entry point and fails on the first thing it calls.
  The parent of a `bin` directory comes along.
- **`/etc/resolv.conf` is often a symlink out of `/etc`** — to `/mnt/wsl` here,
  to `/run` under systemd-resolved. Landlock follows it to the real inode, finds
  it ungranted, and the resolver falls back to localhost. It surfaces as a DNS
  error that has nothing to do with DNS.

## [0.10.1] — 2026-09-08

### Fixed

- **Provider errors were dumped as raw JSON when a gateway did not use the
  OpenAI error shape.** The decoder looked only for `{"error":{"message":...}}`;
  a gateway answering `{"status":401,"message":"API Key tidak valid"}` fell
  through to printing the whole body. All three adapters now try several shapes
  — nested, flat, and `detail` — before giving up, and a plain-text body still
  survives intact.

  `nemuz: openai: HTTP 401: {"status":401,"message":"API Key tidak valid","data":{}}`
  became
  `nemuz: openai: HTTP 401: API Key tidak valid`

### Noted, not fixed

- **There is no exec tool.** The capability model has an `Exec` field, the
  sandbox can enforce it, and no built-in tool uses either — so an agent can
  read, write and list, and cannot run anything. This surfaced when a skill the
  agent wrote for itself said "run make lint, make test, make bench" and the
  model correctly answered that it could not. Added to the roadmap rather than
  rushed: an exec tool is the one that most needs the sandbox to be right.

## [0.10.0] — 2026-09-08

### Fixed

- **The learning loop could never close.** Skills the agent wrote for itself
  landed in quarantine and needed scenarios to be certified — and nothing in
  nemuz could write one. `SaveScenario` existed with no caller, the reviewer had
  no way to produce one, and there was no command for a person to add one. Every
  agent-written skill was born, waited, and was archived by the curator thirty
  days later. The gate was one nothing could pass.

  Found by running against a real model: the agent wrote a genuinely useful
  skill, and there was no route from there to using it.

### Added

- **`nemuz skill scenario add|ls|rm`.** A scenario is built from a recorded
  turn, and the recording is copied in beside it so the two travel together when
  a skill is shared or moved.
- A scenario that asserts nothing is refused. One that passes whatever the skill
  does is worse than none, because it makes the gate look satisfied.
- When certification fails for want of scenarios, the report now says how to
  write one and lists recent turns to build it from.

### A position, not a gap

The reviewer still cannot write its own scenarios, and that is deliberate. A
skill that graded itself would be evidence of nothing. nemuz's claim is that a
learned skill has been checked against what *someone else* said "working" means,
and that claim only holds while the agent proposes and a person decides.

The cost is a person in the loop before a skill goes live. That is the trade,
stated rather than engineered around.

## [0.9.2] — 2026-09-08

### Fixed

- **Memories were written in the wrong language and then never found again.**
  The reviewer wrote a fact in English about a conversation held in Indonesian.
  It stored correctly, it was accurate, and recall — which matches on the words
  themselves — never returned it, because the two shared no terms. The agent
  learned something and could not use it.

  The reviewer is now told to write in the language the user was writing in, and
  told why: a fact stored in a language the questions will not be asked in is a
  fact that will never be found again.

  Every test in this repository ran against a scripted model that spoke one
  language on both sides, so nothing here could have caught it. It appeared
  within minutes of the first run against a real provider, which is the argument
  for doing that at all.

  A deeper fix is embeddings, so recall matches meaning rather than words. That
  remains future work; this one costs nothing and closes the common case.

## [0.9.1] — 2026-09-08

### Fixed

- **CI was testing a Go version it did not name, and 0.9.0 shipped with it
  red.** Adding `modernc.org/sqlite` raised the `go` directive from 1.24 to
  1.25, because the driver itself requires it. Two of the three CI jobs passed
  anyway: Go's default `GOTOOLCHAIN` quietly downloads whatever go.mod asks for,
  so they were building with 1.25 while claiming 1.24. The offline job could not
  download a toolchain, so it alone failed — which is the job doing exactly what
  it exists for, on a mismatch nothing else could see.

  Three changes, in order of how much they matter:

  - `GOTOOLCHAIN: local` in both workflows. CI now builds with the version it
    names, and a mismatch fails immediately rather than in one job hours later.
  - A step that compares go.mod's `go` directive against the version CI installs
    and fails with an explanation. A dependency raising the floor is a normal
    thing to happen; noticing it should not depend on which job runs first.
  - CI and the release workflow both moved to Go 1.25.

### Changed

- **nemuz now requires Go 1.25** to build from source, up from 1.24, because
  `modernc.org/sqlite` does. This should have been stated in 0.9.0 and was not:
  it happened silently when the dependency was added, which is precisely the
  failure the guard above now prevents.

## [0.9.0] — 2026-09-08

Your own history, searchable.

### Added

- **`nemuz search`** — full-text search over every question and answer ever
  recorded, backed by SQLite FTS5. Words are treated literally rather than as an
  FTS5 expression: someone searching their history is asking a question, not
  writing a query language, and `what's the deploy script?` should find things
  rather than fail to parse. The agent's own background reviews are excluded
  unless `--reviews` is passed, since every reviewed turn has a review quoting
  it back.
- **`nemuz usage`** — tokens by model and tool, with background reviews counted
  separately because those are nemuz spending on its own behalf rather than work
  anyone asked for.

  Tokens only, no money. A bill needs a price list, and nemuz ships none:
  prices change without notice, and a confident figure from a stale table is
  worse than no figure. This is a deliberate omission, not an oversight.
- **`nemuz index rebuild` and `nemuz index status`.** The index is derived and
  never authoritative — it holds nothing the journal does not — so the answer to
  a corrupt or missing database is to rebuild it. Nothing is lost, there is no
  migration, and an unreadable journal is skipped and named rather than aborting
  the whole rebuild.
- Turns are indexed as they finish. An indexing failure is reported and dropped,
  because the journal already has the turn and the user was waiting for an
  answer, not for bookkeeping.
- `config.Paths.DB` finally points at something. It has been a dead field since
  0.1.0, named for a database that did not exist.

### Changed

- **The binary is 13 MB, up from 3.2 MB.** Almost all of that is SQLite, which
  arrives through `modernc.org/sqlite` — a pure-Go translation rather than a
  cgo binding, so the static cgo-free binary survives. That trade is worth
  naming: a quarter of the size claim was spent on this one feature. It stays
  well inside the 40 MB budget, and against Hermes Agent's 2.68 GB image the
  comparison is not close, but a reader deserves the number rather than a
  reassurance.

  A side effect worth noting: this gets SQLite 3.53.4 with FTS5 and no cgo,
  which is the version Hermes has to compile from source at image build time.

### Fixed

- **A schema older than the current one was kept rather than rebuilt.** Version
  0 was treated as "fresh database", but it also means one written before the
  schema was versioned. Any version that is not current is now dropped and
  rebuilt, which is safe precisely because the index is derived.

## [0.8.0] — 2026-09-08

Installable, and observable.

### Added

- **Releases.** `.goreleaser.yaml` and a workflow that runs on a `v*` tag,
  producing static cgo-free binaries for Linux, macOS and Windows on amd64 and
  arm64, with SHA-256 checksums and an SBOM. Every target was verified to
  cross-compile, and the config was validated with `goreleaser check` and a real
  build rather than assumed correct — a release config only fails on the tag
  that needed it.
- **`/metrics`**, in Prometheus text format: turns by outcome, model rounds,
  tool calls, tokens by direction with cached reported separately, a duration
  histogram, HTTP responses by status, and uptime.

  It is written by hand, with no metrics library. The exposition format is a few
  lines of text and nemuz publishes about a dozen numbers; a client library
  would have added megabytes and a dependency tree to a binary whose whole
  argument is that it is one small file. The tests assert on the emitted bytes,
  including that histogram buckets are cumulative and that nothing renders in
  scientific notation.

  Two deliberate choices: a label that has never been observed is absent rather
  than zero, because a counter reading zero for something that never happened
  invites the wrong conclusion; and `/metrics` needs no API key, for the same
  reason `/health` does not — a scraper is usually a sidecar with no
  credentials, and what is published is counts, never content.

### Not done, and why

- **OpenTelemetry traces.** The OTel SDK and an OTLP exporter would add several
  megabytes and a large dependency tree. For a project whose first claim is a
  small single binary, that trade needs a concrete reason, and right now there
  is none: the journal already records every turn in more detail than a trace
  would, and `/metrics` covers aggregate health. This stays open rather than
  quietly dropped.

## [0.7.0] — 2026-09-08

Two ways in: any OpenAI client, and any ACP editor.

### Added

- **`nemuz serve`** — an OpenAI-compatible HTTP API. Point an existing client's
  base URL at it and nothing else changes. `stream: true` is supported, though
  the answer is produced whole and sent as one chunk; `/health/detailed` says so
  rather than implying a streaming pipeline that does not exist.

  **The completion id is the turn id**, so an answer that looks wrong goes
  straight to `nemuz replay`. No other OpenAI-shaped endpoint can offer that,
  because no other one records the turn.

  The server binds to loopback by default and *refuses to start* on any other
  address without `NEMUZ_API_KEY`. An agent endpoint with no key is a remote
  shell with extra steps, and that should be impossible to reach by accident
  rather than merely discouraged.
- **`nemuz acp`** — the Agent Client Protocol v1 over stdio, so Zed and other
  ACP editors can drive nemuz directly. Method names and message bodies follow
  the published v1 schema, which was fetched and read rather than recalled.

  Editors get more than the final answer: the tool calls the turn actually made
  are reported as `tool_call` updates, read back from the journal in the order
  they happened. A session opened for a directory the agent is not bound to is
  refused, because answering confidently about the wrong project is worse than
  refusing.
- **`Registry.Tools`**, so a caller can hand a built toolset somewhere else.

Both servers are built on the public API rather than on internals, which is the
most convincing check that the public API is actually complete.

### Fixed

- **ACP cancellation could never arrive.** Prompts ran on the read loop, so the
  loop was sitting inside the very prompt whose cancellation it was supposed to
  read. Prompts now run on their own goroutine; everything else stays inline,
  where ordering is free. The test that caught this hung rather than failed,
  which is its own kind of useful.

## [0.6.0] — 2026-09-08

### Added

- **`Agent.Review` and `Agent.Curate` on the public API.** Self-learning was
  reachable only from the CLI, so anyone embedding nemuz as a library got a
  runtime with no memory and no curation — half the framework, behind a door
  they could not open.

  Neither runs automatically. When to review is the caller's decision, and
  doing it after every turn is usually the wrong one; the novelty ledger still
  skips shapes seen recently, and the result says when it did.
- **`Options.ReviewProvider` and `Options.ReviewModel`**, so the review can run
  on a smaller and cheaper model than the turn did. Deciding what was worth
  keeping is a much easier job than the turn was.
- **`ReviewResult`, `CurationReport` and `CurationAction`** as value types, so
  the shape callers read stays stable while the stores beneath it change.

## [0.5.1] — 2026-09-08

### Fixed

- **The offline CI job hung instead of reporting.** It cut all outbound traffic
  on the runner with an iptables rule, which also cuts the runner's own
  connection to GitHub — so the job could never report a result or upload logs,
  and sat until the six-hour timeout. A block that kills its own reporter proves
  nothing.

  The suite now runs inside a container with no network interface but loopback,
  which isolates the tests rather than the machine. It finishes in about twenty
  seconds, and a companion step confirms that container genuinely cannot reach
  the internet — otherwise the job would pass by being wrong in both directions.
- Added `timeout-minutes` to every job, so a hang fails in twenty minutes rather
  than six hours.
- Pinned CI to Go 1.24, matching the go directive, instead of 1.27.

## [0.5.0] — 2026-09-08

Skills have to keep earning their place.

### Added

- **The curator.** Three checks that need no model, run once a day while the
  agent is idle, or on demand:
  - **Re-verification.** Active skills are run against their own scenarios
    again. A skill that passed in one version is not proven forever — tools
    change, plugins change, nemuz changes — and one that no longer passes is
    returned to quarantine with the reason attached. This is only affordable
    because scenarios replay recordings, which is what makes it routine rather
    than an event.
  - **Retirement.** Skills nobody has used for 90 days are archived.
  - **Quarantine expiry.** Drafts that sat 30 days without being certified are
    archived, so quarantine does not become the drawer where the agent's bad
    ideas accumulate.
- **`nemuz skill curate`** with `--dry-run`, `--force`, `--pause` and
  `--resume`. `--force` bypasses the interval but deliberately not a pause: a
  flag should not quietly overrule a decision someone made.
- **`Store.Demote`** — active back to quarantine. It narrows what the agent can
  do, so unlike promotion it needs no evidence; the burden of proof runs one
  way, toward trusting the agent less.
- **`--curate` on `nemuz run`**, on by default and rate limited to once a day.

### Changed

- The skill frontmatter field `archived_reason` is now `reason`, because it
  carries the reason for a demotion as well as an archival. Existing skill files
  keep working; the old field is simply ignored, and the reason is rewritten on
  the next transition. `nemuz skill show` labels it to match the state.

### Fixed

- **Idleness was measured from the wrong field.** The curator fell back to
  `UpdatedAt` when a skill had never been used, but that field changes on every
  write to the store — bumping a use counter, pinning, even the curator saving
  the skill back. A skill could look busy for having been touched by
  bookkeeping. It now falls back to `CreatedAt`.

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

[Unreleased]: https://github.com/kansaok/nemuz/compare/v0.12.0...HEAD
[0.12.0]: https://github.com/kansaok/nemuz/compare/v0.11.1...v0.12.0
[0.11.1]: https://github.com/kansaok/nemuz/compare/v0.11.0...v0.11.1
[0.11.0]: https://github.com/kansaok/nemuz/compare/v0.10.1...v0.11.0
[0.10.1]: https://github.com/kansaok/nemuz/compare/v0.10.0...v0.10.1
[0.10.0]: https://github.com/kansaok/nemuz/compare/v0.9.2...v0.10.0
[0.9.2]: https://github.com/kansaok/nemuz/compare/v0.9.1...v0.9.2
[0.9.1]: https://github.com/kansaok/nemuz/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/kansaok/nemuz/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/kansaok/nemuz/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/kansaok/nemuz/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/kansaok/nemuz/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/kansaok/nemuz/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/kansaok/nemuz/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/kansaok/nemuz/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/kansaok/nemuz/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/kansaok/nemuz/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/kansaok/nemuz/releases/tag/v0.1.0
