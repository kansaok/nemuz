package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kansaok/nemuz/internal/config"
	"github.com/spf13/cobra"
)

// pairFileName holds the in-flight pairing codes a `nemuz channel telegram
// --pair` process handed out, keyed by code and mapped to the Telegram user it
// was given to. `nemuz pairing-code` consumes it.
const pairFileName = "telegram-pair.json"

// Pair codes are deliberately short-lived: they bridge an untrusted chat to
// a privileged local command, so retaining one after the operator has walked
// away turns a convenient handshake into a standing credential.
const pairCodeTTL = 10 * time.Minute

var pairNow = time.Now

type pairCode struct {
	UserID   int64     `json:"user_id"`
	IssuedAt time.Time `json:"issued_at"`
}

// issuePairCode returns the code for a Telegram user — reusing an existing one,
// so a user who messages again during the same pairing session does not get a
// new code each time — and stores it for `nemuz pairing-code`.
func issuePairCode(pairPath string, userID int64) (string, error) {
	m, err := loadPairCodes(pairPath)
	if err != nil {
		return "", err
	}
	changed := discardExpiredPairCodes(m, pairNow())
	for code, entry := range m {
		if entry.UserID == userID {
			if changed {
				if err := savePairCodes(pairPath, m); err != nil {
					return "", err
				}
			}
			return code, nil
		}
	}
	code, err := newPairCode()
	if err != nil {
		return "", err
	}
	m[code] = pairCode{UserID: userID, IssuedAt: pairNow().UTC()}
	if err := savePairCodes(pairPath, m); err != nil {
		return "", err
	}
	return code, nil
}

func newPairCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pairing: random code: %w", err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

func loadPairCodes(pairPath string) (map[string]pairCode, error) {
	body, err := os.ReadFile(pairPath)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]pairCode{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]pairCode
	if err := json.Unmarshal(body, &m); err == nil {
		return m, nil
	}
	// Files written before expiry was introduced contain code -> user id.
	// Treat those credentials as expired rather than silently extending them.
	var legacy map[string]int64
	if err := json.Unmarshal(body, &legacy); err != nil {
		return nil, fmt.Errorf("pairing: %s is unreadable: %w", pairPath, err)
	}
	_ = legacy
	return map[string]pairCode{}, nil
}

func savePairCodes(pairPath string, m map[string]pairCode) error {
	if err := os.MkdirAll(filepath.Dir(pairPath), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pairPath, body, 0o600)
}

func discardExpiredPairCodes(m map[string]pairCode, now time.Time) (changed bool) {
	for code, entry := range m {
		if entry.IssuedAt.IsZero() || now.Sub(entry.IssuedAt) > pairCodeTTL {
			delete(m, code)
			changed = true
		}
	}
	return changed
}

// pairingCodeCmd consumes the code a running `nemuz channel telegram --pair`
// handed to a Telegram user, and writes that user into the config as allowed.
func pairingCodeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pairing-code <code>",
		Short: "Authorize the Telegram user a --pair channel handed this code to",
		Long: "Consumes a pairing code printed by a running\n" +
			"`nemuz channel telegram --pair`: it matched the code to the Telegram\n" +
			"user who messaged the bot, and writes that user id into\n" +
			"channels.telegram.allowUsers and commands.ownerAllowFrom.\n\n" +
			"  nemuz channel telegram --pair      (terminal 1)\n" +
			"  nemuz pairing-code ABC234          (terminal 2, after messaging the bot)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			userID, err := authorizePairCode(paths, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "authorized telegram user %d — restart `nemuz channel telegram` if it is still running\n", userID)
			return nil
		},
	}
}

// authorizePairCode matches a code from the pairing file to the Telegram user
// it was handed to, writes that user into config, and consumes the code.
func authorizePairCode(paths config.Paths, code string) (int64, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	m, err := loadPairCodes(filepath.Join(paths.Root, pairFileName))
	if err != nil {
		return 0, err
	}
	discardExpiredPairCodes(m, pairNow())
	pairing, ok := m[code]
	if !ok {
		_ = savePairCodes(filepath.Join(paths.Root, pairFileName), m)
		return 0, fmt.Errorf("pairing: no pending pairing matches %q — message the bot first so it prints a live code", code)
	}
	userID := pairing.UserID

	settings, err := config.LoadSettings(paths.Config)
	if err != nil {
		return 0, err
	}
	if settings.Channels.Telegram == nil {
		settings.Channels.Telegram = &config.ChannelTelegram{}
	}
	settings.Channels.Telegram.AllowUsers = appendUniqueID(settings.Channels.Telegram.AllowUsers, userID)
	entry := "telegram:" + strconv.FormatInt(userID, 10)
	if !containsString(settings.Commands.OwnerAllowFrom, entry) {
		settings.Commands.OwnerAllowFrom = append(settings.Commands.OwnerAllowFrom, entry)
	}
	if err := config.Save(paths.Config, settings); err != nil {
		return 0, fmt.Errorf("config: save %s: %w", paths.Config, err)
	}

	delete(m, code)
	if err := savePairCodes(filepath.Join(paths.Root, pairFileName), m); err != nil {
		return 0, err
	}
	return userID, nil
}

func appendUniqueID(list []int64, id int64) []int64 {
	for _, v := range list {
		if v == id {
			return list
		}
	}
	return append(list, id)
}
