# nemuz

An agent framework whose every turn can be replayed.

nemuz records everything an agent does — the model request, the response, each
tool call and its result — to an append-only journal, then replays it exactly.
Bugs become reproducible. Regressions become testable without spending a single
API call. And skills the agent teaches itself can be proven before they are
trusted.

> Status: early. The turn loop, provider adapters, tool plugins, learned skills
> with an eval gate, journal, replay, and the Landlock sandbox work and are
> tested end to end. Memory, the idle curator, and chat channels are not built
> yet.

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
| **Single static binary** | No CGO, no runtime, no container required. Currently 3.5 MB. |
| **Non-root always** | Tool confinement uses Landlock and seccomp, which need no privilege. |
| **Capabilities, not guards** | A tool declares the paths and hosts it needs; the kernel refuses the rest. |
| **No file over 800 lines** | Enforced in CI. Large files cannot be reviewed, unit-tested, or merged cleanly. |
| **Big payloads never in the database** | They go to a content-addressed blob store; everything else holds hashes. |

## Try it

Requires Go 1.24 or newer.

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

Landlock is a thread credential, so `Restrict` is meant to be called once, early,
in a process dedicated to confined work — the tool subprocess or plugin host.
That is the same process boundary the plugin architecture already needs.

## Layout

```
cmd/nemuz/          the binary
internal/agent/     the turn loop
internal/llm/       provider IR, plus the record and replay seam
internal/llm/provider/  adapters for the three wire formats, and the catalogue
internal/tool/      tool registry, capabilities, filesystem tools
internal/journal/   append-only events, digests, cassettes, replay
internal/blob/      content-addressed store for large payloads
internal/plugin/    JSON-RPC host, capability policy
internal/skill/     learned skills, lifecycle invariants, the eval gate
internal/sandbox/   Landlock enforcement
internal/config/    where state lives
bench/              size, startup, and file-length budgets, enforced by make
sdk/typescript/     the plugin SDK
examples/           reference plugins in Go and JavaScript
```

## Storage

State lives under `~/.nemuz`, or `NEMUZ_HOME` if set.

```
state.db                  indexes and operational state
journal/<turn>.jsonl      append-only records — greppable, rsync-friendly
blobs/<ab>/<sha256>       large payloads, deduplicated
skills/<name>/SKILL.md    a learned skill: YAML frontmatter plus Markdown
skills/<name>/evals/      its scenarios, and the recordings they replay
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
- [ ] Memory, the idle curator, and background review after each turn
- [ ] seccomp filters and network capabilities
- [ ] Channels: Telegram, Slack, Discord, WhatsApp
- [ ] Memory, skills, curator, and the eval gate
- [ ] OpenAI-compatible HTTP API, ACP, OpenTelemetry

## License

Apache 2.0
