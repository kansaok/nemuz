# nemuz

An agent framework whose every turn can be replayed.

nemuz records everything an agent does — the model request, the response, each
tool call and its result — to an append-only journal, then replays it exactly.
Bugs become reproducible. Regressions become testable without spending a single
API call. And skills the agent teaches itself can be proven before they are
trusted.

> Status: early. The turn loop, provider adapters, tool plugins, memory,
> background review, learned skills with an eval gate and a curator, journal,
> replay, the Landlock sandbox, an OpenAI-compatible HTTP API, ACP and a
> Telegram channel all work and are tested end to end.

## Why

Two mature agent frameworks already exist, and both are good at things nemuz
does not try to beat them at. Hermes Agent ships 22 chat platforms and 138
tools; OpenClaw ships a plugin SDK with formal TLA+ security models. Neither can
answer a simpler question:

**Why did my agent do that, and will it do it again?**

Agents are non-deterministic by nature, so today you cannot reproduce a bad run,
cannot write a regression test against it, and cannot prove an upgrade did not
make things worse. nemuz treats that as the problem worth solving, and lets the
rest follow from it.

## How replay works

Every turn is journaled before anything observes it:

```
$ nemuz journal show 01M1ZTPQYX
SEQ  KIND            SIZE   PAYLOAD
1    turn.start      27     {"prompt":"ringkas README"}
2    model.request   38     {"messages":1,"model":"claude-opus-5"}
3    model.response  33     {"arg":"README.md","tool":"read"}
4    tool.call       33     {"arg":"README.md","tool":"read"}
5    tool.result     10810  blob:9412d5d751d0 (10810 bytes)
6    model.response  40     {"done":true,"say":"Ringkasan selesai."}
7    turn.end        21     {"ok":true,"steps":1}

7 events, digest ec149bbf424f1009c873f78ac635ada0642c59583c1ed465191a4bb1f79d8261
```

Replaying runs the real agent loop with the real tools, but the model adapter
draws from the recording instead of calling a provider. The loop cannot tell the
difference, so any divergence in the resulting events was caused by nemuz — not
by the model answering differently this time.

The digest is what makes that checkable. It covers event order, kinds, and
payloads, and deliberately excludes wall-clock time: two runs of the same turn
differ in timing, and that must not count as a difference.

## What follows from it

- **Regression tests that cost nothing.** The whole suite runs from recordings,
  with network egress blocked in CI.
- **An eval gate for learned skills.** A skill the agent writes for itself
  enters quarantine and stays there until it passes recorded scenarios. Learning
  without a gate means trusting the agent's own homework.
- **An audit trail by construction.** The record already exists; nothing extra
  has to be written to get one.

## Design commitments

| | |
|---|---|
| **Single static binary** | No CGO, no runtime, no container required. Currently 13 MB, most of it SQLite. |
| **Non-root always** | Tool confinement uses Landlock and seccomp, which need no privilege. |
| **Capabilities, not guards** | A tool declares the paths and hosts it needs; the kernel refuses the rest. |
| **No file over 800 lines** | Enforced in CI. Large files cannot be reviewed, unit-tested, or merged cleanly. |
| **Big payloads never in the database** | They go to a content-addressed blob store; everything else holds hashes. |

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/kansaok/nemuz/master/install.sh | sh
```

```powershell
irm https://raw.githubusercontent.com/kansaok/nemuz/master/install.ps1 | iex
```

Neither script needs root or administrator: they install to a user-level
directory (`~/.local/bin`, or `%LOCALAPPDATA%\nemuz\bin` on Windows) and verify
the download's SHA-256 against the release's own `checksums.txt` before
installing anything. Pass `--system` to install.sh to use `/usr/local/bin`
instead, or `--version vX.Y.Z` to either script to pin a release.

Or download an archive directly from
[releases](https://github.com/kansaok/nemuz/releases), or build from source:

```bash
go install github.com/kansaok/nemuz/cmd/nemuz@latest
```

Builds are static and cgo-free for Linux, macOS and Windows on amd64 and arm64,
with checksums and an SBOM. Landlock confinement is Linux only; `nemuz doctor`
says so on the others rather than implying otherwise.

## Try it

Requires Go 1.25 or newer to build from source — `modernc.org/sqlite` sets that floor.

```bash
make            # lint, test, build
./bin/nemuz doctor
```

`doctor` reports whether your kernel can enforce the sandbox:

```
  ok    platform   linux/amd64, go1.27.1
  ok    sandbox    Landlock ABI v1 — tool confinement enforced by the kernel
  ok    state dir  /home/you/.nemuz
```

Run a turn against a live model, then replay it:

```bash
export ANTHROPIC_API_KEY=...
nemuz run "what is in this workspace?" --workspace .
nemuz replay <turn-id> --workspace .
```

For a back-and-forth conversation instead of one command per message, use
`nemuz chat`. It opens the model and the toolset once, then reads a line at a
time:

```bash
nemuz chat --workspace .
```

Each line you type is still its own recorded turn — replayable, indexed,
reviewed — `chat` just keeps the connection open instead of making you type
`nemuz run "..."` for every message. Type `/exit` or `/quit`, or press
Ctrl+D, to leave.

Or reach the same agent from Telegram, with `nemuz channel telegram`. It
long-polls the Bot API, so it needs no public URL or TLS certificate of its
own — the bot dials out to Telegram rather than the other way around:

```bash
export TELEGRAM_BOT_TOKEN=<from @BotFather>
nemuz channel telegram --allow-user 123456789
```

Each message becomes its own recorded turn, replayable exactly like one from
`run` or `chat`. `--allow-user` is required and names every Telegram user id
allowed to talk to the bot; anyone else is ignored, so a leaked token alone
never opens the agent. One workspace is shared across every allowed user —
this is a single-tenant channel, not yet a per-user sandbox.

Replay needs no provider at all — the model can be unreachable, the key revoked,
the vendor gone. Or record a turn with no API key at all, using a scripted model
over real tools:

```bash
export NEMUZ_HOME=/tmp/nemuz
TURN=$(go run ./bench/seed -workspace .)
./bin/nemuz journal show $TURN
./bin/nemuz replay $TURN --workspace .
```

```
  ok  12 events reproduced exactly
      digest 26a24e1cf674c8194b83fb33a810dee776a26c161205d6561c10cc07259fafa8
```

Change anything the turn depended on — add a file to the workspace, edit a tool —
and the replay stops at the exact point where the run stopped matching:

```
DIVERGED  llm: replay diverged before model call 2 — the request differs from the recording
```

## Configuring defaults

Typing `--provider anthropic --model claude-opus-5` on every command gets old.
`nemuz config` saves defaults once, in `~/.nemuz/config.json`:

```bash
nemuz config set provider anthropic
nemuz config set model claude-opus-5
nemuz config set sandbox on
nemuz config set allow-exec go,make
nemuz config get model
nemuz config unset sandbox
nemuz config          # list everything currently saved
nemuz config env CORPO_API_KEY=sk-...   # a secret, straight into ~/.nemuz/.env
nemuz config env                        # which secret names are set
```

Secrets never need to be opened by hand: `nemuz config env NAME=VALUE` writes
them into `~/.nemuz/.env` (permission `0600`), never echoes the value back, and
`nemuz config env --unset NAME` forgets one. The file is created next to
`config.json` on that command, so the setup is:

```bash
nemuz config env CORPO_API_KEY=sk-...
nemuz config set model custom-ai-corpo/engine-v1
```

`run`, `serve`, `acp`, `replay`, `consolidate`, `skill eval`/`certify`, and
`skill curate` all read from this file for the flags they take — provider,
model, review-model, base-url, sandbox, allow-exec, allow-net, skills,
memories, delegate-depth. A flag passed explicitly on the command line always
wins over a saved default; the file only fills in what you didn't say this
time. Nothing
here is required — nemuz runs fine with no config file, falling back to its
built-in defaults exactly as before.

The file holds the same nested layout OpenClaw uses, so a machine can be
administered the same way (or driven by that project's migration tooling).
Every key above survives in its familiar `key value` CLI form, and the file
remembers a couple of extra knobs that don't belong on a per-run flag:

```json
{
  "agents": {
    "defaults": {
      "workspace": "/srv/nemuz/ws",
      "model": { "primary": "anthropic/claude-opus-5" },
      "sandbox": "auto"
    }
  },
  "models": {
    "mode": "merge",
    "providers": {
      "custom-ai-corpo": {
        "baseUrl": "https://ai.corpo.internal/v1",
        "api": "openai-completions",
        "apiKey": "${CORPO_API_KEY}",
        "models": [{ "id": "engine-v1" }]
      }
    }
  },
  "gateway": { "auth": { "mode": "token", "token": "${GATEWAY_TOKEN}" } },
  "channels": {
    "telegram": {
      "enabled": true,
      "botToken": "${TELEGRAM_BOT_TOKEN}",
      "allowUsers": [7361357941],
      "groups": { "*": { "requireMention": true } }
    }
  },
  "commands": { "ownerAllowFrom": ["telegram:7361357941"] },
  "plugins": { "entries": { "go-ls": { "enabled": true } } }
}
```

Secrets never live in `config.json`: the value is a reference like `${NAME}`
that is also real — secret, provider keys, bot tokens — and resolved at load
time from `~/.nemuz/.env` (or the process environment). Nothing that names a
secrets file needs to be opened by hand: `nemuz config env NAME=VALUE` puts a
credential there from the command line without echoing it. This is the one
place nemuz deliberately copies OpenClaw: the file is readable and shareable,
and `nemuz config` never prints what it expands. Migrating holds: an old flat
`~/.nemuz/config.yaml` is read once and copied as `config.yaml.bak` the first
time the new file is needed; the CLI's flat keys stay valid for day-to-day tweaks.

## Delegating to a sub-agent

The agent always has a `delegate` tool: it hands a self-contained sub-task to
another agent turn — the same model, the same tools, the same workspace — and
gets back only that turn's final answer, not its whole step-by-step
transcript. It exists so a task can be broken into independent pieces instead
of trying to hold a whole plan in one context window.

```bash
nemuz run "delegate the arithmetic to a sub-agent, then summarise the result"
```

The sub-agent's turn is recorded exactly like any other — its own turn id,
its own entry in `nemuz journal list`, replayable on its own. It carries no
memory of the parent conversation beyond the task description it was given,
and it never reviews or curates on its own: those judge whether talking to the
*operator* was worth remembering, and answering to another agent isn't that.

Delegation nests, so a sub-agent can delegate again — bounded by
`--delegate-depth` (default 2, matching what most tasks need without letting
a confused model delegate indefinitely). `--delegate-depth 0` disables it —
the tool is not even offered to the model. The budget is spent per call
chain: a sub-agent that itself delegates uses one less level than its caller,
not a fresh budget of its own, and a fresh `nemuz chat` line always starts
back at the full depth.

## Memory, and learning without being asked

After a turn finishes, a second smaller turn looks at it and decides whether
anything is worth keeping. This is Hermes Agent's mechanism, and it works.

```
$ nemuz run "bagaimana cara deploy ke staging?"
Deploy ke staging: jalankan scripts/ship.sh staging.

review: remembered 1 fact(s) — nemuz memory ls
```

Next time, that fact comes back on its own:

```
$ nemuz run "ingatkan saya soal deploy staging"
1 memories recalled

Seperti yang saya ingat, jalankan scripts/ship.sh staging.
```

Three things make this safe to leave switched on.

**The reviewer can do exactly two things.** It can remember a fact and draft a
skill. It cannot read files, run commands, or reach the network — not because
the prompt asks it not to, but because those tools are not in its registry.
A test asserts it.

**A drafted skill is quarantined.** The reviewer proposes; the eval gate
decides. There is no argument and no code path that produces an active skill.

**Repeated turns are reviewed once.** Hermes reviews after every turn, which
means paying for the twentieth "run the tests" of the afternoon to rediscover
that there is nothing to learn. nemuz fingerprints the shape of a turn — its
tools and the distinctive words of its prompt, deliberately not the answer — and
skips shapes it has seen recently:

```
review skipped — a turn of this shape was reviewed recently
```

Recall is deterministic: the same query against the same store always returns
the same memories in the same order. That is not tidiness. Memories go into the
system prompt, so a recall that varied between runs would make every turn
unreplayable for reasons that have nothing to do with the agent. The ids that
were recalled are written into the turn's record, so a reader six weeks later
can see what the agent had been reminded of:

```json
{"env": {"memories": "01M201APKB49KG36N2E8HVMSBW", "sandbox": "landlock-v1"}}
```

Memories are **not** gated the way skills are, and the asymmetry is deliberate.
A skill is a procedure whose misuse does damage. A memory is a claim about the
world, corrected the way a person corrects one — `nemuz memory forget <id>`,
which really deletes. Leaving a corrected fact archived would mean the agent
still held a belief its user had explicitly denied.

## Learned skills, and the gate

Agents that write their own skills are not new. Hermes Agent does it well: a
background review after each turn, an idle curator, a rollback ledger. What it
does not do — what nothing does — is check the skill before trusting it. A skill
the model wrote becomes active on the model's own say-so.

nemuz keeps the mechanism and closes that gap. Every skill the agent writes
lands in **quarantine** and stays there until it passes recorded scenarios:

```
$ nemuz skill ls
SKILL        STATE       BY     USES  EVALS  DESCRIPTION
pakai-shout  quarantine  agent  0     1      Use the shout tool when asked to shout.

1 skill(s) in quarantine. Run `nemuz skill certify <name>` to test and promote one.
```

A scenario is a recorded turn plus what the skill must make happen:

```json
{
  "name": "basic",
  "journal": "turn.jsonl",
  "expect": {
    "must_call": ["example__shout"],
    "must_not_call": ["write_file"],
    "must_succeed": true
  }
}
```

Scenarios are written with `nemuz skill scenario add`, pointing at a recorded
turn where the skill would have applied:

```bash
nemuz skill scenario add project-health-check --turn 01M20T32S3 \
  --must-call read_file --must-not-call write_file --must-succeed
```

```
$ nemuz skill certify pakai-shout --plugin "node ./plugin.mjs"
pakai-shout — 1 of 1 scenarios passed

  ok   basic
         turn 01M1ZXADFYEHSCPDG9AXW63RZB — inspect with: nemuz journal show 01M1ZXADFYEHSCPDG9AXW63RZB

promoted to active, evidence eval-20260908T070434Z
```

**Certification costs nothing.** The model's answers come from the recording, so
no API call is made and no network is needed. A gate that costs money every time
gets switched off, and a gate that is switched off protects nothing.

**The agent does not write its own exam.** It proposes a skill; someone else
says what "working" means. That is deliberate, and it is the whole difference
from trusting the model's own say-so — a skill that graded itself would be
evidence of nothing. The cost is that certification needs a person in the loop,
which is a trade this project makes on purpose.

Three rules hold no matter who is acting:

- **Nothing is ever deleted.** Archiving is the strongest removal, and it is
  reversible. Restoring returns a skill to quarantine, not to service — whatever
  made it wrong may still be true.
- **Active requires evidence.** A skill file claiming `state: active` with no
  promotion record fails validation, so the gate cannot be edited around.
- **Pinned skills are never touched automatically.** Neither is anything a
  person wrote.

Strict replay and the eval gate use the same journal for different questions.
Strict replay asks *did nemuz change its mind?* and demands an identical digest.
The gate asks *does this skill still make the agent do the right thing?* — and
because adding a skill changes the system prompt by construction, it asserts on
behaviour instead: which tools ran, what the answer said, how long it took.

## Finding skills that teach the same thing twice

The curator retires and re-verifies skills without ever calling a model — that
is what lets it run automatically and for free. Whether two skills are *the
same idea written twice* is a different kind of question: a judgement about
meaning, not something a mechanical rule can decide. So it is its own explicit
step.

```
$ nemuz skill consolidate
1 candidate pair(s) considered, 1 merged

merged  cek-struktur-direktori + lihat-isi-proyek → cek-struktur-direktori
        needs certifying: nemuz skill certify cek-struktur-direktori
        reasoning: nemuz journal show 01M223A4T44NJ085FN82YBBKPQ
```

A cheap local pass — how much two descriptions' words overlap — finds candidate
pairs first, so the model is only asked about ones that already look plausibly
related, among active, agent-written, unpinned skills. Most real pairs should
end in **keep separate**, and the model is expected to say so:

```
$ nemuz skill consolidate
  keep  deploy-ke-produksi + deploy-ke-staging — production requires approval
        first, staging does not — merging them risks skipping that requirement.
```

**A merge is not trusted for having been proposed.** The result starts in
quarantine like any skill the agent writes for itself, carrying over both
sources' eval scenarios so it can be certified without writing new ones — the
same gate, the same standard, evidence reused rather than assumed. The sources
are archived, not deleted, with a reason naming what replaced them.

Two bugs live testing found, neither of which a scripted test alone would have:
a model that named the merge after one of the two sources, rather than
inventing a third name, and a model that decided correctly and then closed with
an empty answer, which the agent loop reads as a turn failure. Both are fixed
and covered by regression tests now — proof that testing against a real model,
even briefly, finds a different class of bug than a scripted one ever will.

## Passing once is not passing forever

A skill that was proven in March is not proven in September. Tools change,
plugins change, nemuz changes. So the curator runs the scenarios again — and
because they replay recordings, it costs nothing, which is the only reason it
can be routine rather than an event.

```
$ nemuz skill curate
1 of 2 skill(s) changed

  ok       masih-benar  1 of 1 scenarios passed
 demoted   sudah-rusak  no longer passes its own scenarios: 1 of 1 scenarios failed: dasar
```

A skill that stops passing goes back to quarantine with the reason attached. It
is not deleted, and not archived — it simply stops being trusted until it earns
that back.

The curator also retires skills nobody has used, and clears out drafts that sat
in quarantine without ever being certified, so quarantine does not become the
drawer where the agent's bad ideas accumulate. It runs once a day while the
agent is idle, or on demand with `--force`; `--dry-run` says what it would do.

Three things it will never do: touch a skill a person wrote, touch a pinned
skill, or delete anything.

The judgement it does *not* make is consolidating two overlapping skills into
one. That needs a model, and it is a real gap — the checks above are the ones
that can be made from evidence already on disk.

## Searching your own history

Every turn is journaled, so every turn is searchable.

```
$ nemuz search deploy staging
01M20QRSATF6MSH6R987Z34785  2026-09-08 21:46  claude-opus-5
  bagaimana cara «deploy» ke «staging»?
  replay: nemuz replay 01M20QRSATF6MSH6R987Z34785
```

Words are taken literally rather than as an FTS5 expression, so an ordinary
question with an apostrophe or a hyphen finds things instead of failing to
parse. The agent's own background reviews are excluded unless you ask for them:
every reviewed turn has a review quoting it back, and including both would
double the results while adding nothing.

```
$ nemuz usage --days 30
last 30 days · 142 turns (61 were background reviews) · 3 failed
1.9M in / 84.2k out · 1.1M of the input was cached

MODEL          TURNS  INPUT  OUTPUT  CACHED
claude-opus-5  81     1.6M   71.0k   980.1k
claude-haiku   61     286k   13.2k   140.3k

TOOL        CALLS  FAILED
read_file   204    3
list_dir    97     0
```

Tokens, not money. Turning tokens into a bill needs a price list, and nemuz
ships none: prices change without notice, and a confident figure computed from a
stale table is worse than no figure at all.

**The index is derived, never authoritative.** It holds nothing the journal does
not, so the answer to a corrupt or missing database is `nemuz index rebuild` —
no backup, no migration, nothing lost. `nemuz index status` says whether it has
fallen behind.

That is also why SQLite arrives through `modernc.org/sqlite`, a pure-Go
translation rather than a binding: the static cgo-free binary is worth more than
the megabytes it costs. It happens to ship SQLite 3.53.4 with FTS5 — the version
Hermes Agent has to compile from source at image build time to get.

## Providers

Three wire formats cover thirteen providers, because most vendors serve the
OpenAI shape and only need a base URL:

```
$ nemuz providers
PROVIDER    DEFAULT MODEL   FORMAT · KEY · NOTES
anthropic   claude-opus-5   anthropic · ANTHROPIC_API_KEY
gemini      gemini-2.5-pro  gemini · GEMINI_API_KEY
openai      gpt-5           openai-compatible · OPENAI_API_KEY
groq        (pass --model)  openai-compatible · GROQ_API_KEY
ollama      llama3.2        openai-compatible · local; needs no API key
…
```

Anthropic and Gemini get real adapters because they genuinely differ. Anthropic
puts the system prompt at the top level and carries tool results inside a user
message. Gemini calls the assistant "model", nests tool declarations a level
deeper, puts the model in the URL — and issues no tool-call ids at all, so the
adapter synthesises ids from position, because a random id would change the
journal digest on every replay.

Adding a vendor that speaks the OpenAI shape is one line in a table, not an
adapter.

Pointing `--base-url` at a custom OpenAI-compatible gateway means picking some
preset's name just to get the right wire format — often `openai`, the most
generic one — even though the key has nothing to do with OpenAI itself. Rather
than requiring the key to live in a variable literally named after that
choice, any provider's key can also come from the generic `NEMUZ_API_KEY`,
which is tried whenever the preset-specific variable is unset:

```bash
nemuz config set provider openai
nemuz config set base-url https://your-gateway.example/v1
export NEMUZ_API_KEY=...       # instead of OPENAI_API_KEY
nemuz chat --workspace .
```

A provider-specific variable, when set, always wins over `NEMUZ_API_KEY` — the
generic one only fills in when nothing more specific was said.

## Plugins

A plugin is any process that speaks newline-delimited JSON-RPC on stdio. It gets
its own capability grant, its own resource limits, and its own crash — and it
can be written in any language. There is a TypeScript SDK in
[`sdk/typescript`](sdk/typescript) and reference plugins in
[`examples/`](examples) for both Go and JavaScript.

```js
import { definePlugin } from "@nemuz/sdk";

definePlugin({
  name: "example",
  tools: [{
    name: "shout",
    description: "Uppercase a string.",
    schema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
    run: ({ text }) => text.toUpperCase(),
  }],
});
```

```bash
nemuz run "teriakkan sesuatu" --plugin "node ./plugin.mjs"
```

**A plugin's declared capabilities are a request, not a grant.** The host clamps
filesystem paths to the workspace and refuses network and process access the
operator has not approved — before the model is ever shown the tool:

```
$ nemuz plugin inspect "node ./greedy.mjs"
TOOL        ASKED FOR                                  GRANTED
exfiltrate  read / · net evil.example.com · exec curl  REFUSED — tool "exfiltrate"
                                                       wants network access to
                                                       "evil.example.com", which
                                                       is not allowed
```

The process boundary is also the sandbox boundary: Landlock is a thread
credential, so confining tool execution needs a process that exists to be
confined. Plugins provide exactly that.

## Sandboxing

Tools declare what they need; the kernel enforces it. `write_file` asks for the
workspace and nothing else, so a confined process cannot read `/etc/passwd` even
if the model asks it to — the refusal is `EACCES` from Landlock, not a string
check in Go.

```go
func (t *WriteFile) Capabilities() Capabilities {
	return Capabilities{FSRead: []string{t.ws.Root()}, FSWrite: []string{t.ws.Root()}}
}
```

The agent takes the union of its tools' declarations and hands that to the
sandbox before the model speaks. Nothing can widen it afterwards: Landlock is
one-way by design, and needs no privilege, which is why nemuz never runs as root.

Landlock is a thread credential, so confining tool execution needs a process
whose whole job is to be confined. nemuz therefore runs its own built-in tools
in a subprocess — `nemuz tool-worker` — that applies Landlock to itself before
serving anything, over the same JSON-RPC protocol the plugins use. Confining the
gateway instead would confine the journal, the config, and the provider
connection along with the tools, which is the opposite of useful.

| `--sandbox` | Behaviour |
|---|---|
| `auto` (default) | Confine where the kernel allows it; say so plainly when it cannot. |
| `on` | Require confinement; refuse to run without it. |
| `off` | Run the tools in-process, unconfined. Recorded in the journal. |

`nemuz doctor` now checks two layers separately:

```
  ok    kernel     Landlock ABI v1 available
  ok    sandbox    verified — the tool worker cannot read outside its workspace
  ok    seccomp    verified — ptrace and similar syscalls are refused by the kernel
```

**seccomp is layered under Landlock.** Landlock answers *which files* a process
may touch; seccomp answers *which syscalls* it may make at all, regardless of
path. Once `--allow-exec` lets an agent run real programs, Landlock alone says
nothing about `ptrace`, `mount`, or loading a kernel module — none of those are
about a file.

It is a denylist, not an allowlist: a short, fixed set of syscalls with
essentially no legitimate use inside a coding-agent tool call and a clear
history in exploits — process introspection (`ptrace`, cross-process memory
access), filesystem namespace manipulation (`mount`, `chroot`), kernel module
and code loading, and a handful of privileged operations. Everything else,
ordinary networking included, is left alone. An allowlist would need enumerating
every syscall `go build` or `bash` might ever make — as impractical as
enumerating binaries was for Landlock, and certain to break on the first real
use.

Stated rather than implied: this does not distinguish a raw socket from an
ordinary TCP connection, which would need inspecting `socket()`'s arguments.
Defense in depth, not a complete network policy.

The status the journal records — `landlock-v1+seccomp+exec` — comes from the
confined worker's own report of what it achieved, not a guess made before it
ran. A kernel without `CONFIG_SECCOMP_FILTER` still gets Landlock's file
confinement; that gap is reported, not hidden.

## Running commands

With `--allow-exec`, the agent gets a `run_command` tool. Without it, there is
no way to run anything at all.

```bash
nemuz run "run the tests and tell me what broke" \
  --allow-exec go --allow-exec make
```

**There is no shell.** Arguments go to the operating system directly, so pipes,
globs and `&&` do not work and there is no quoting to get wrong. A model that
wants two things done asks twice.

Programs are named, not pathed, and resolved using *your* environment — so a
toolchain under your home directory is found, and the sandbox is granted the
directories it actually lives in. `go vet` needs `go/pkg/tool`, not just
`go/bin`, so the toolchain root comes along.

Commands get a private scratch directory as their `HOME` and `TMPDIR`, under the
state directory rather than in your project. A build leaves no litter in the
repository it was asked to check, and cannot read your caches or credentials
through `$HOME`.

### What the sandbox still guarantees, and what it does not

Even with exec fully allowed, a command can write to exactly two places: the
workspace and that scratch directory. Reading the system and running programs is
permitted; changing anything else is not — verified by a test that watches the
kernel refuse it.

The workspace is writable and **not** executable. An agent can write to the
project it was asked to work on and cannot then run what it wrote, so the
repository never becomes a launcher. Project scripts still run through an
allowed interpreter: `--allow-exec bash`, then `bash ./scripts/check.sh`.

Be clear about what the allowlist is, though. Allowing `bash`, or `go`, or any
compiler grants arbitrary execution by construction — `bash -c` is a program
that runs programs. The allowlist limits what the model can casually reach for.
What bounds the damage is the sandbox, and that is the part worth trusting.

Whichever applied is written into the turn's `turn.start` event, so a recording
says whether its tools were confined:

```json
{"env":{"sandbox":"landlock-v1"}, "model":"...", "prompt":"..."}
```

`nemuz doctor` does not take the kernel's word for it. It spawns the worker and
has it try to read a file outside its workspace:

```
  ok    platform   linux/amd64, go1.27.1
  ok    kernel     Landlock ABI v1 available
  ok    sandbox    verified — the tool worker cannot read outside its workspace
```

A build that computed the right grants and never applied them would pass every
other check and fail this one.

## Talking to it

### From anything that speaks OpenAI

```bash
nemuz serve --provider anthropic
```

```bash
curl -s http://127.0.0.1:8642/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"what changed today?"}]}'
```

Point an existing client's base URL at `http://127.0.0.1:8642/v1` and it works
unchanged. `stream: true` is supported, though the answer is produced whole and
sent as one chunk — `/health/detailed` says so rather than letting anyone infer
a streaming pipeline that does not exist.

**The completion id is the turn id.** An answer that looks wrong goes straight
to `nemuz replay <id>`, with no provider and no network. No other
OpenAI-compatible endpoint offers that, because no other one records the turn.

The server binds to loopback by default and refuses to listen anywhere else
without `NEMUZ_API_KEY`. An agent endpoint with no key is a remote shell with
extra steps.

`/metrics` serves Prometheus text: turns by outcome, tool calls, tokens by
direction, and a duration histogram whose buckets run to ten minutes, because
agent turns are not web requests. It publishes counts and durations — never a
prompt, an answer, or a key.

### From an editor

```bash
nemuz acp --provider anthropic
```

nemuz speaks the [Agent Client Protocol](https://agentclientprotocol.com) v1 on
stdio, so Zed and other ACP clients can drive it directly. The message shapes
follow the published schema rather than guesswork.

Editors get more than the answer: the tool calls the turn actually made are
reported as `tool_call` updates, read back from the journal in the order they
happened. A session opened for a directory the agent is not bound to is refused,
because answering confidently about the wrong project is worse than saying no.

## Using nemuz as a library

The root package is the API. Everything under `internal/` is not, and changes
without notice.

```go
import "github.com/kansaok/nemuz"

provider, _ := nemuz.OpenProvider(nemuz.ProviderSpec{Provider: "anthropic"})
agent, _ := nemuz.Open(nemuz.Options{
    Provider:  provider,
    Model:     "claude-opus-5",
    Workspace: ".",
    Tools:     []nemuz.Tool{myTool{}},
})
defer agent.Close()

out, _ := agent.Run(ctx, "what changed in this repo today?")
err := agent.Replay(ctx, out.TurnID)   // no provider, no network
```

Self-learning is available too, and stays the caller's decision rather than
something that happens behind their back:

```go
result, _ := agent.Review(ctx, out)       // what was worth keeping?
report, _ := agent.Curate(ctx, false)     // do the skills still pass?
```

`Review` can remember a fact and draft a skill, and nothing else — a drafted
skill arrives quarantined. `Curate` re-runs every active skill's scenarios and
sends back the ones that stopped passing. Both cost far less than a turn:
curation costs nothing at all.

A tool is one interface:

```go
func (myTool) Name() string                    { return "now" }
func (myTool) Description() string             { return "The current time." }
func (myTool) Schema() json.RawMessage         { return json.RawMessage(`{"type":"object"}`) }
func (myTool) Capabilities() nemuz.Capabilities { return nemuz.Capabilities{} }
func (myTool) Run(ctx context.Context, args json.RawMessage) (nemuz.Result, error) { … }
```

## Layout

```
nemuz.go, types.go   the public API
cmd/nemuz/          the binary
internal/agent/     the turn loop
internal/llm/       provider IR, plus the record and replay seam
internal/llm/provider/  adapters for the three wire formats, and the catalogue
internal/tool/      tool registry, capabilities, filesystem tools
internal/journal/   append-only events, digests, cassettes, replay
internal/blob/      content-addressed store for large payloads
internal/plugin/    JSON-RPC host, capability policy
internal/skill/     learned skills, lifecycle invariants, the eval gate
internal/memory/    remembered facts and deterministic recall
internal/review/    the post-turn review, and its two-tool registry
internal/curator/   re-verification, retirement, quarantine expiry
internal/consolidate/  finding and merging overlapping skills, with a model
internal/httpapi/   the OpenAI-compatible server
internal/acp/       the Agent Client Protocol server
internal/metrics/   Prometheus exposition, written by hand rather than imported
internal/index/     SQLite index over the journal: search and usage
internal/sandbox/   Landlock enforcement
internal/config/    where state lives
bench/              size, startup, and file-length budgets, enforced by make
sdk/typescript/     the plugin SDK
examples/           reference plugins in Go and JavaScript
```

## Storage

State lives under `~/.nemuz`, or `NEMUZ_HOME` if set.

```
state.db                  the search index, rebuildable from the journal
journal/<turn>.jsonl      append-only records — greppable, rsync-friendly
blobs/<ab>/<sha256>       large payloads, deduplicated
skills/<name>/SKILL.md    a learned skill: YAML frontmatter plus Markdown
skills/<name>/evals/      its scenarios, and the recordings they replay
memories/<id>.md          one remembered fact each
reviewed.txt              turn shapes reviewed recently
```

The journal survives a database reset, because the database holds nothing that
cannot be rebuilt from it.

## Roadmap

- [x] Journal, blob store, digests, cassettes, replay verification
- [x] Landlock enforcement, path layout, CLI, size budgets
- [x] Turn loop, tool registry with capabilities, filesystem tools
- [x] `nemuz replay`, verified end to end against real tools
- [x] Providers: OpenAI-compatible, Anthropic, Gemini — 13 vendors, retries, typed errors
- [x] Plugin host over JSON-RPC, capability policy, and the TypeScript SDK
- [x] Learned skills, lifecycle invariants, and the eval gate
- [x] Sandboxed tool worker, verified end to end by `nemuz doctor`
- [x] Memory with deterministic recall, and background review with novelty filtering
- [x] The idle curator: re-verification, retirement, quarantine expiry
- [x] Consolidating overlapping skills, with a model, as its own explicit step
- [x] A public API and CI that enforces the no-network and sandbox claims
- [ ] Sandbox backends for macOS and Windows
- [ ] Channels: Slack, Discord, WhatsApp
- [x] Telegram channel — long-polling, no public URL needed, replayable turns
- [x] OpenAI-compatible HTTP API, with replayable completion ids
- [x] Agent Client Protocol v1, for Zed and other editors
- [x] A Prometheus endpoint, and multi-platform releases with checksums and an SBOM
- [ ] OpenTelemetry traces — deferred: the SDK would cost more than the whole binary
- [x] Full-text search over every turn, and usage accounting
- [x] An exec tool, confined so a command can write only to the workspace
- [x] seccomp, layered under Landlock: a fixed denylist for syscalls with no
      legitimate use in a tool call

## Changelog

See [CHANGELOG.md](CHANGELOG.md). Before 1.0, minor versions may break things.

## License

Apache 2.0
