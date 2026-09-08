package nemuz

import (
	"github.com/kansaok/nemuz/internal/agent"
	"github.com/kansaok/nemuz/internal/journal"
	"github.com/kansaok/nemuz/internal/llm"
	"github.com/kansaok/nemuz/internal/tool"
)

// Interfaces you implement, and the types their methods use.
//
// These are aliases rather than copies so a tool written against this package
// is the same type the runtime uses — there is no conversion layer to get
// wrong, and no second definition to drift.
type (
	// Tool is one thing an agent can do. Implement it to add your own.
	Tool = tool.Tool
	// Capabilities is what a tool needs permission to touch. The narrowest
	// set that works is the right one; the sandbox refuses the rest.
	Capabilities = tool.Capabilities
	// Result is what a tool returns. Set IsError to report a failure the
	// model should see and recover from, rather than returning a Go error,
	// which ends the turn.
	Result = tool.Result

	// Provider performs model calls. Implement it to support a service the
	// built-in adapters do not cover.
	Provider = llm.Provider
	// Request is one model call, already normalised.
	Request = llm.Request
	// Response is one model reply, already normalised.
	Response = llm.Response
	// Message is one entry in a conversation.
	Message = llm.Message
	// ToolCall is a model's request to run one tool.
	ToolCall = llm.ToolCall
	// ToolDef describes a tool to the model.
	ToolDef = llm.ToolDef
	// Usage reports token consumption.
	Usage = llm.Usage

	// Event is one record in a turn's journal.
	Event = journal.Event
	// EventKind identifies what an event records.
	EventKind = journal.Kind

	// Outcome summarises a completed turn.
	Outcome = agent.Outcome
)

// Roles a message can carry.
const (
	RoleSystem    = llm.RoleSystem
	RoleUser      = llm.RoleUser
	RoleAssistant = llm.RoleAssistant
	RoleTool      = llm.RoleTool
)

// Reasons a model stopped generating.
const (
	StopEnd       = llm.StopEnd
	StopToolUse   = llm.StopToolUse
	StopMaxTokens = llm.StopMaxTokens
)

// Event kinds, in the order a turn produces them.
const (
	EventTurnStart     = journal.KindTurnStart
	EventModelRequest  = journal.KindModelRequest
	EventModelResponse = journal.KindModelResponse
	EventToolCall      = journal.KindToolCall
	EventToolResult    = journal.KindToolResult
	EventError         = journal.KindError
	EventTurnEnd       = journal.KindTurnEnd
)

// Errorf builds a failed Result, for a tool that ran and could not do the job.
func Errorf(format string, args ...any) Result { return tool.Errorf(format, args...) }

// Fact is something the agent remembers.
//
// It is a value type rather than an alias because it is read, not implemented,
// and a stable shape here means the memory store can change beneath it.
type Fact struct {
	// ID identifies the memory and can be passed to Forget.
	ID string
	// Text is the fact itself.
	Text string
	// Kind is "user", "project" or "reference".
	Kind string
	// Tags are labels the agent chose.
	Tags []string
	// Turn is the turn this was learned in, or empty.
	Turn string
	// Uses counts how often it has been recalled into a prompt.
	Uses int
	// Pinned means it is included in every prompt.
	Pinned bool
}

// SkillInfo describes a learned skill.
type SkillInfo struct {
	// Name is the skill's identifier.
	Name string
	// Description says when the skill applies.
	Description string
	// State is "quarantine", "active" or "archived". Only active skills are
	// given to a model.
	State string
	// WrittenBy is "agent" or "user".
	WrittenBy string
	// Uses counts how often the skill has been used.
	Uses int
	// Scenarios is how many eval scenarios the skill carries. A skill needs
	// at least one to leave quarantine.
	Scenarios int
	// PromotedBy names the eval run that activated it, if any.
	PromotedBy string
}

// TurnInfo identifies a recorded turn.
type TurnInfo struct {
	// ID is the turn's identifier, usable with Replay and Events.
	ID string
	// Events is how many events the turn recorded.
	Events int
	// Digest is the hash of the turn's event stream. Two runs that made the
	// same decisions in the same order share a digest.
	Digest string
}
