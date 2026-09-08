// Package nemuz is the public API for building agents whose every turn can be
// replayed.
//
// Everything an agent does is written to an append-only journal before anything
// observes it. A recorded turn can then be run again against its recording,
// which makes bugs reproducible, makes regression tests free, and lets a skill
// the agent wrote for itself be proven before it is trusted.
//
// # Getting started
//
//	provider, err := nemuz.OpenProvider(nemuz.ProviderSpec{Provider: "anthropic"})
//	if err != nil {
//		return err
//	}
//
//	agent, err := nemuz.Open(nemuz.Options{
//		Provider:  provider,
//		Model:     "claude-opus-5",
//		Workspace: ".",
//	})
//	if err != nil {
//		return err
//	}
//	defer agent.Close()
//
//	out, err := agent.Run(ctx, "what is in this workspace?")
//	fmt.Println(out.Text, out.TurnID)
//
// Later, with no provider and no network:
//
//	err := agent.Replay(ctx, out.TurnID)
//
// # Adding a tool
//
// Implement [Tool]. A tool declares what it needs to touch; the sandbox grants
// that and refuses the rest, so declare the narrowest set that works.
//
//	type Clock struct{}
//
//	func (Clock) Name() string                 { return "now" }
//	func (Clock) Description() string          { return "The current time, in RFC 3339." }
//	func (Clock) Schema() json.RawMessage      { return json.RawMessage(`{"type":"object"}`) }
//	func (Clock) Capabilities() nemuz.Capabilities { return nemuz.Capabilities{} }
//
//	func (Clock) Run(ctx context.Context, args json.RawMessage) (nemuz.Result, error) {
//		return nemuz.Result{Content: time.Now().Format(time.RFC3339)}, nil
//	}
//
// A tool that returns a different answer every run makes its turns
// unreplayable. That is sometimes the right trade, but make it knowingly.
//
// # Stability
//
// This package is the API. The internal packages beneath it are not, and change
// without notice. Before 1.0 this package may change too, and the CHANGELOG says
// how.
package nemuz
