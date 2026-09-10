package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/kansaok/nemuz/internal/cli"
	"github.com/kansaok/nemuz/internal/config"
	"github.com/kansaok/nemuz/internal/llm/provider"
	"github.com/kansaok/nemuz/internal/telegram"
)

// tokenEnv is the secrets-file slot a bot token is saved into during channel
// setup, and what channels.telegram.botToken refers to with ${...}.
const tokenEnv = "TELEGRAM_BOT_TOKEN"

// The two network touches the wizard makes, as variables so a test can stub
// them without a gateway or a live bot token.
var (
	wizardFetchModels = fetchModels
	wizardBotCheck    = func(ctx context.Context, token string) (telegram.User, error) {
		return (&telegram.Client{Token: token}).GetMe(ctx)
	}
)

// configWizard is the interactive `nemuz config` setup. It writes the same
// nested config.json (and the .env next to it) a person would type by hand,
// asking the questions a flag cannot: which provider, where its gateway lives,
// proof the key works, which of its models to use — then how to reach the
// agent (a channel) and who may talk to it.
func configWizard(io *cli.IO, paths *config.Paths, s *config.Config) error {
	fmt.Fprintf(io.Out, "nemuz configuration — edits %s (+ a .env, if secrets are set)\n", paths.Config)
	if s.Agents.Defaults.Provider != "" || s.Agents.Defaults.Model.Primary != "" {
		model := s.Agents.Defaults.Model.Primary
		if model == "" {
			model = "(none)"
		}
		fmt.Fprintf(io.Out, "currently saved — provider: %s · model: %s\n", s.Agents.Defaults.Provider, model)
	}
	for {
		choice, err := io.Pick("\nWhat do you want to set up?", []string{
			"model\nwhich provider talks, and which model it uses",
			"channel\na way to reach the agent (telegram)",
			"done\nreload config and go use it",
		})
		if err != nil {
			return interrupt(err)
		}
		switch choice {
		case 0:
			if err := setupModel(io, paths, s); err != nil {
				if errors.Is(err, errWizardStop) {
					continue
				}
				return err
			}
		case 1:
			if err := setupChannel(io, paths, s); err != nil {
				if errors.Is(err, errWizardStop) {
					continue
				}
				return err
			}
		default:
			return finishWizard(io)
		}
	}
}

// errWizardStop unwinds one setup section back to the main menu without
// losing anything already saved.
var errWizardStop = errors.New("stop this section")

func interrupt(err error) error {
	if errors.Is(err, io.EOF) {
		fmt.Println("wizard interrupted; nothing more was changed")
		return nil
	}
	return err
}

func finishWizard(io *cli.IO) error {
	fmt.Fprintln(io.Out, `Setup finished. Every command loads config fresh, so nothing needs a
          restart for plain `+"`nemuz run`"+` — only a process already running
          (nemuz serve, nemuz channel telegram, nemuz chat) has to be
          restarted to pick the new settings up.

  nemuz providers   see what's configured
  nemuz run "..."   try it`)
	return nil
}

// ---------------------------------------------------------------- model setup

func setupModel(io *cli.IO, paths *config.Paths, s *config.Config) error {
	names := provider.PresetNames()
	options := make([]string, 0, len(names)+2)
	for _, n := range names {
		p, _ := provider.Lookup(n)
		options = append(options, n+"\n"+p.Describe())
	}
	options = append(options,
		"custom provider\npaste an OpenAI-compatible base URL, e.g. https://ai.corpo.internal/v1",
		"cancel\ndon't change the model")

	choice, err := io.Pick("Which provider?", options)
	if err != nil {
		return interrupt(err)
	}
	if choice >= len(names) {
		if choice == len(names)+1 {
			fmt.Fprintln(io.Out, "model setup cancelled")
			return errWizardStop
		}
		return setupCustomProvider(io, paths, s)
	}
	return setupPresetProvider(io, paths, s, names[choice])
}

func setupPresetProvider(io *cli.IO, paths *config.Paths, s *config.Config, presetName string) error {
	p, _ := provider.Lookup(presetName)

	var (
		key   string
		model string
		err   error
	)
	if p.KeyEnv == "" {
		// local presets (ollama, lmstudio, vllm) need no key and no probing.
		model, err = pickModel(io, presetName, nil, "(type the model id)")
		if err != nil {
			return err
		}
	} else {
		for {
			key, err = io.ReadSecret(fmt.Sprintf("API key for %s (saved as %s):", presetName, p.KeyEnv))
			if err != nil {
				return interrupt(err)
			}
			if key == "" {
				return errWizardStop
			}
			ids, verr := wizardFetchModels(context.Background(), p.Wire(), p.BaseURL, key)
			switch {
			case errors.Is(verr, errBadKey):
				fmt.Fprintf(io.Out, "  %s rejected that key — try again\n", presetName)
				continue
			case errors.Is(verr, errNoModelList):
				fmt.Fprintf(io.Out, "  key accepted (no model list exposed)\n")
				ids = nil
			case verr != nil:
				fmt.Fprintf(io.Out, "  could not check the key: %v — try again\n", verr)
				continue
			default:
				fmt.Fprintf(io.Out, "  key accepted — %d models found\n", len(ids))
			}
			model, err = pickModel(io, presetName, ids, "(type the model id)")
			if err != nil {
				return err
			}
			break
		}
		if err := config.SetEnv(paths.Env, p.KeyEnv, key); err != nil {
			return err
		}
	}

	s.Agents.Defaults.Provider = presetName
	s.Agents.Defaults.Model.Primary = presetName + "/" + model
	if err := config.Save(paths.Config, s); err != nil {
		return fmt.Errorf("config: save %s: %w", paths.Config, err)
	}
	fmt.Fprintf(io.Out, "\nmodel saved: %s/%s\n", presetName, model)
	return nil
}

func setupCustomProvider(io *cli.IO, paths *config.Paths, s *config.Config) error {
	base, err := io.Prompt("Custom provider base URL (e.g. https://ai.corpo.internal/v1):")
	if err != nil {
		return interrupt(err)
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return errWizardStop
	}
	name, err := providerNameFromBase(base, s)
	if err != nil {
		return err
	}
	keyEnv := providerKeyEnv(name)

	var key, model string
	for {
		key, err = io.ReadSecret(fmt.Sprintf("API key for %s (saved as %s):", name, keyEnv))
		if err != nil {
			return interrupt(err)
		}
		if key == "" {
			return errWizardStop
		}
		ids, verr := wizardFetchModels(context.Background(), "openai-completions", base, key)
		switch {
		case errors.Is(verr, errBadKey):
			fmt.Fprintln(io.Out, "  the gateway rejected that key — try again")
			continue
		case errors.Is(verr, errNoModelList):
			fmt.Fprintf(io.Out, "  key accepted (the gateway exposes no model list)\n")
			ids = nil
		case verr != nil:
			fmt.Fprintf(io.Out, "  could not check the key: %v — try again\n", verr)
			continue
		default:
			fmt.Fprintf(io.Out, "  key accepted — %d models found\n", len(ids))
		}
		model, err = pickModel(io, name, ids, "(type the model id)")
		if err != nil {
			return err
		}
		break
	}

	if err := config.SetEnv(paths.Env, keyEnv, key); err != nil {
		return err
	}
	if s.Models.Providers == nil {
		s.Models.Providers = map[string]config.ProviderDef{}
	}
	s.Models.Mode = "merge"
	s.Models.Providers[name] = config.ProviderDef{
		BaseURL: base,
		API:     "openai-completions",
		APIKey:  "${" + keyEnv + "}",
		Models:  []config.ProviderModel{{ID: model}},
	}
	s.Agents.Defaults.Provider = name
	s.Agents.Defaults.Model.Primary = name + "/" + model
	if err := config.Save(paths.Config, s); err != nil {
		return fmt.Errorf("config: save %s: %w", paths.Config, err)
	}
	fmt.Fprintf(io.Out, "\nmodel saved: %s/%s (%s), key → %s\n", name, model, base, keyEnv)
	return nil
}

// pickModel offers the provider's real models (ids, from the validation call)
// with a free-text option at the top, so a gateway without a /models endpoint
// still works by typing. ids may be nil, in which case only the free text is
// offered.
func pickModel(io *cli.IO, where string, ids []string, freeLabel string) (string, error) {
	free := freeLabel
	if free == "" {
		free = "(type a model name)"
	}
	options := []string{free}
	for _, id := range ids {
		options = append(options, id)
	}
	msg := "Which model should the agent use with " + where + "?"
	if len(ids) > 0 {
		msg = fmt.Sprintf("Which model should the agent use with %s? (%d found; 1 = type one)", where, len(ids))
	}
	choice, err := io.Pick(msg, options)
	if err != nil {
		return "", interrupt(err)
	}
	if choice == 0 {
		m, err := io.Prompt("Type the model id:")
		if err != nil {
			return "", interrupt(err)
		}
		if m == "" {
			return "", errWizardStop
		}
		return m, nil
	}
	return options[choice], nil
}

// -------------------------------------------------------------- channel setup

func setupChannel(io *cli.IO, paths *config.Paths, s *config.Config) error {
	choice, err := io.Pick("Which channel?", []string{
		"telegram\nlong-polls the Bot API — no public URL or TLS needed",
		"cancel\ndon't touch channels",
	})
	if err != nil {
		return interrupt(err)
	}
	if choice == 1 {
		fmt.Fprintln(io.Out, "channel setup cancelled")
		return errWizardStop
	}

	var token string
	for {
		t, err := io.ReadSecret("Telegram bot token (from @BotFather):")
		if err != nil {
			return interrupt(err)
		}
		if t == "" {
			return errWizardStop
		}
		me, err := wizardBotCheck(context.Background(), t)
		if err != nil {
			fmt.Fprintf(io.Out, "  telegram rejected that token: %v\n", err)
			continue
		}
		fmt.Fprintf(io.Out, "  token ok — bot @%s\n", me.Username)
		token = t
		break
	}

	method, err := io.Pick("Who may talk to the bot?", []string{
		"pairing (recommended)\nmessage the bot, get a pairing code, authorize it here",
		"manual\nI'll type the Telegram user ids myself",
	})
	if err != nil {
		return interrupt(err)
	}

	if s.Channels.Telegram == nil {
		s.Channels.Telegram = &config.ChannelTelegram{}
	}
	enabled := true
	s.Channels.Telegram.Enabled = &enabled
	s.Channels.Telegram.BotToken = "${" + tokenEnv + "}"

	if err := config.SetEnv(paths.Env, tokenEnv, token); err != nil {
		return err
	}

	if method == 1 {
		ids, err := promptUserIDs(io)
		if err != nil {
			return err
		}
		s.Channels.Telegram.AllowUsers = ids
		for _, id := range ids {
			entry := "telegram:" + strconv.FormatInt(id, 10)
			if !containsString(s.Commands.OwnerAllowFrom, entry) {
				s.Commands.OwnerAllowFrom = append(s.Commands.OwnerAllowFrom, entry)
			}
		}
		if err := config.Save(paths.Config, s); err != nil {
			return fmt.Errorf("config: save %s: %w", paths.Config, err)
		}
		fmt.Fprintf(io.Out, "\ntelegram saved — %d user(s) allowed\n\nTo run it:  nemuz channel telegram\n", len(ids))
		return nil
	}

	if err := config.Save(paths.Config, s); err != nil {
		return fmt.Errorf("config: save %s: %w", paths.Config, err)
	}
	fmt.Fprintln(io.Out, `telegram saved in pairing mode — nobody is allowed yet.

  1. run:   nemuz channel telegram --pair
  2. message the bot from Telegram (say anything)
  3. it answers with a pairing code
  4. run:   nemuz pairing-code <code>
     — that Telegram user is authorized.

Finish this wizard (done) and message the bot again: it answers from then on.`)
	return nil
}

func promptUserIDs(io *cli.IO) ([]int64, error) {
	raw, err := io.Prompt("Telegram user id(s), comma separated (your own id is easiest):")
	if err != nil {
		return nil, interrupt(err)
	}
	fields := strings.Fields(strings.ReplaceAll(raw, ",", " "))
	if len(fields) == 0 {
		return nil, errWizardStop
	}
	ids := make([]int64, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("not a Telegram user id: %q", f)
		}
		ids = append(ids, n)
	}
	return ids, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// providerNameFromBase turns a gateway's host into a provider name, reusing an
// existing registered provider when the base already belongs to one.
func providerNameFromBase(base string, s *config.Config) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("%q is not a valid base URL", base)
	}
	if s != nil {
		for n, def := range s.Models.Providers {
			if def.BaseURL != "" && strings.TrimRight(def.BaseURL, "/") == strings.TrimRight(base, "/") {
				return n, nil
			}
		}
	}
	host := strings.TrimPrefix(strings.TrimPrefix(u.Hostname(), "www."), "api.")
	name := sanitizeProviderName(host)
	if name == "" || provider.Known(name) {
		name = "custom-" + name
	}
	for i := 2; provider.Known(name); i++ {
		name = fmt.Sprintf("custom-%s-%d", sanitizeProviderName(host), i)
	}
	return name, nil
}

// sanitizeProviderName turns "ai.sumopod.com" into "ai-sumopod-com" — a
// provider name a config file can carry.
func sanitizeProviderName(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.ToLower(strings.Trim(b.String(), "-"))
}

// providerKeyEnv is the .env slot a provider's key is stored under.
func providerKeyEnv(providerName string) string {
	var b strings.Builder
	for _, r := range providerName {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.ToUpper(b.String()) + "_API_KEY"
}
