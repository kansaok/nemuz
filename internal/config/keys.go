package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Keys names every setting `nemuz config` accepts, in the order they are
// listed. Kept as one slice so `config` (with no arguments) and validation of
// an unknown key name agree on exactly the same list.
//
// Each key is a flat name for a slot in the nested Config file — `model`
// writes agents.defaults.model.primary, for example. The nested sections that
// have no natural flat name (channels, gateway, models.providers) are edited
// in the file itself, the way OpenClaw has no per-key setter either.
var Keys = []string{
	"workspace", "provider", "model", "review-model", "base-url",
	"sandbox", "allow-exec", "allow-net", "skills", "memories",
	"review", "curate", "delegate-depth",
}

// Get returns one setting's current value as a string, for `nemuz config get`
// and for listing. A key that has never been set returns "" and true — it is
// a known, valid key with nothing saved, distinct from an unknown key name.
func (c *Config) Get(key string) (value string, known bool) {
	d := &c.Agents.Defaults
	switch key {
	case "workspace":
		return d.Workspace, true
	case "provider":
		return d.Provider, true
	case "model":
		return d.Model.Primary, true
	case "review-model":
		return d.ReviewModel, true
	case "base-url":
		return d.BaseURL, true
	case "sandbox":
		return d.Sandbox, true
	case "allow-exec":
		return strings.Join(d.AllowExec, ","), true
	case "allow-net":
		return strings.Join(d.AllowNet, ","), true
	case "skills":
		return boolPtrString(d.Skills), true
	case "memories":
		return boolPtrString(d.Memories), true
	case "review":
		return boolPtrString(d.Review), true
	case "curate":
		return boolPtrString(d.Curate), true
	case "delegate-depth":
		return intPtrString(d.DelegateDepth), true
	default:
		return "", false
	}
}

// Set stores one setting by name, parsing value according to the key's type.
// allow-exec and allow-net take a comma-separated list; skills, memories,
// review, and curate take "true" or "false"; sandbox takes on, auto, or off.
// An unknown key or an unparseable value is refused, so a typo in a shell
// script fails loudly here rather than being silently ignored the first time a
// command tries to use it.
func (c *Config) Set(key, value string) error {
	d := &c.Agents.Defaults
	switch key {
	case "workspace":
		d.Workspace = value
	case "provider":
		d.Provider = value
	case "model":
		d.Model.Primary = value
	case "review-model":
		d.ReviewModel = value
	case "base-url":
		d.BaseURL = value
	case "sandbox":
		if value != "on" && value != "auto" && value != "off" {
			return fmt.Errorf("config: sandbox must be on, auto, or off, not %q", value)
		}
		d.Sandbox = value
	case "allow-exec":
		d.AllowExec = splitList(value)
	case "allow-net":
		d.AllowNet = splitList(value)
	case "skills":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("config: skills must be true or false, not %q", value)
		}
		d.Skills = &b
	case "memories":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("config: memories must be true or false, not %q", value)
		}
		d.Memories = &b
	case "review":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("config: review must be true or false, not %q", value)
		}
		d.Review = &b
	case "curate":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("config: curate must be true or false, not %q", value)
		}
		d.Curate = &b
	case "delegate-depth":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("config: delegate-depth must be a non-negative integer, not %q", value)
		}
		d.DelegateDepth = &n
	default:
		return fmt.Errorf("config: unknown setting %q; see `nemuz config` for the list", key)
	}
	return nil
}

// Unset clears one setting back to nemuz's built-in default.
func (c *Config) Unset(key string) error {
	d := &c.Agents.Defaults
	switch key {
	case "workspace":
		d.Workspace = ""
	case "provider":
		d.Provider = ""
	case "model":
		d.Model.Primary = ""
	case "review-model":
		d.ReviewModel = ""
	case "base-url":
		d.BaseURL = ""
	case "sandbox":
		d.Sandbox = ""
	case "allow-exec":
		d.AllowExec = nil
	case "allow-net":
		d.AllowNet = nil
	case "skills":
		d.Skills = nil
	case "memories":
		d.Memories = nil
	case "review":
		d.Review = nil
	case "curate":
		d.Curate = nil
	case "delegate-depth":
		d.DelegateDepth = nil
	default:
		return fmt.Errorf("config: unknown setting %q; see `nemuz config` for the list", key)
	}
	return nil
}

// TelegramAllowUsers returns the ids allowed to talk to the Telegram bot:
// channels.telegram.allowUsers directly, falling back to "telegram:<id>"
// entries in commands.ownerAllowFrom the way OpenClaw lists its owners.
func (c *Config) TelegramAllowUsers() []int64 {
	if c == nil {
		return nil
	}
	if tg := c.Channels.Telegram; tg != nil && len(tg.AllowUsers) > 0 {
		return append([]int64(nil), tg.AllowUsers...)
	}
	var out []int64
	for _, entry := range c.Commands.OwnerAllowFrom {
		id, ok := strings.CutPrefix(entry, "telegram:")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func boolPtrString(b *bool) string {
	if b == nil {
		return ""
	}
	if *b {
		return "true"
	}
	return "false"
}

func intPtrString(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var out []string
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
