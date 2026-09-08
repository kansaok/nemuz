// Command seed records one real agent turn into NEMUZ_HOME.
//
// The model is scripted rather than remote, so this runs with no API key and no
// network — but everything else is real: real tools over a real workspace, a
// real agent loop, and a real journal. That makes it the fixture the journal
// tooling, the replay command, and the benchmarks are exercised against.
//
// Usage:
//
//	NEMUZ_HOME=/tmp/nemuz go run ./bench/seed -workspace .
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"

	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/blob"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/tool"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("seed: ")
	workspace := flag.String("workspace", ".", "workspace the tools operate on")
	prompt := flag.String("prompt", "apa isi workspace ini?", "the turn's prompt")
	flag.Parse()

	paths, err := config.Resolve()
	must(err)
	must(paths.EnsureDirs())

	bs, err := blob.Open(paths.Blobs)
	must(err)
	ws, err := tool.NewWorkspace(*workspace)
	must(err)

	tools := tool.NewRegistry()
	must(tools.Register(tool.NewReadFile(ws), tool.NewWriteFile(ws), tool.NewListDir(ws)))

	turnID, err := journal.NewTurnID()
	must(err)
	w, err := journal.Create(paths.Journal, turnID, bs)
	must(err)
	defer w.Close()

	provider := &llm.Static{Label: "seed", Responses: []llm.Response{
		{
			StopReason: llm.StopToolUse,
			ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "list_dir", Args: raw(map[string]string{"path": "."})}},
			Usage:      llm.Usage{InputTokens: 140, OutputTokens: 22},
		},
		{
			StopReason: llm.StopToolUse,
			ToolCalls:  []llm.ToolCall{{ID: "c2", Name: "read_file", Args: raw(map[string]string{"path": "README.md"})}},
			Usage:      llm.Usage{InputTokens: 190, OutputTokens: 26},
		},
		{
			Text:       "Workspace ini berisi sebuah README dan beberapa direktori sumber.",
			StopReason: llm.StopEnd,
			Usage:      llm.Usage{InputTokens: 320, OutputTokens: 41},
		},
	}}

	a := &agent.Agent{
		Provider: llm.Record(provider, w),
		Tools:    tools,
		Journal:  w,
		Model:    "claude-opus-5",
		System:   "Kamu asisten yang ringkas.",
	}

	out, err := a.Run(context.Background(), *prompt)
	must(err)
	must(w.Close())

	fmt.Printf("%s\n", out.TurnID)
	fmt.Fprintf(flag.CommandLine.Output(), "  %d steps, %d tool calls, %d tokens in / %d out\n",
		out.Steps, out.ToolCalls, out.Usage.InputTokens, out.Usage.OutputTokens)
}

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	must(err)
	return b
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
