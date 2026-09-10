package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvFileName is the secrets file sitting next to config.json. It uses the
// classic .env format — KEY=VALUE lines — and is the home for the values a
// config file refers to with ${NAME}: it stays out of git, and nemuz loads it
// before expanding those references. This is the same split OpenClaw implies
// (secrets in the environment), just with a convenient file to put them in
// that is not shared the way the config file is.
const EnvFileName = ".env"

// LoadDotEnv reads a KEY=VALUE file and exports each key into the process
// environment unless it is already set there — an explicitly exported variable
// always wins over a file, the same way a flag always wins over a saved
// default. A missing or unreadable file is not an error; secrets simply stay
// wherever they already were.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !isEnvName(key) {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
	return sc.Err()
}

func isEnvName(s string) bool {
	if s == "" || s[0] != '_' && !isLetter(rune(s[0])) {
		return false
	}
	for _, r := range s {
		if r == '_' || isLetter(r) || r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// parseEnvLine reports the key carried by a .env line, returning ok=false for
// blank lines, comments, and anything without a KEY=VALUE shape.
func parseEnvLine(line string) (key string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", false
	}
	s = strings.TrimPrefix(s, "export ")
	k, _, found := strings.Cut(s, "=")
	if !found {
		return "", false
	}
	k = strings.TrimSpace(k)
	return k, isEnvName(k)
}

// EnvNames lists the keys defined in the .env file at path, without their
// values — the names alone are safe to show in a terminal. A missing file is
// an empty list, not an error.
func EnvNames(path string) ([]string, error) {
	lines, err := readEnvLines(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range lines {
		if k, ok := parseEnvLine(l); ok {
			names = append(names, k)
		}
	}
	return names, nil
}

// SetEnv saves (or replaces) one KEY=VALUE line in the .env file at path,
// leaving every other line untouched. A config value can then name it with
// ${NAME}, which is resolved at load. The file is written 0600 regardless of
// what was there before, and parent directories are created on demand.
func SetEnv(path, key, value string) error {
	if !isEnvName(key) {
		return fmt.Errorf("invalid secret name %q — use letters, digits, and _", key)
	}
	lines, err := readEnvLines(path)
	if err != nil {
		return err
	}
	replaced := false
	for i, l := range lines {
		if k, ok := parseEnvLine(l); ok && k == key {
			lines[i] = key + "=" + value
			replaced = true
		}
	}
	if !replaced {
		lines = append(lines, key+"="+value)
	}
	return writeEnv(path, lines)
}

// UnsetEnv deletes every line defining key from the .env file at path. The
// secret is gone from disk; a running process keeps the value it already read
// into its environment until it exits.
func UnsetEnv(path, key string) error {
	if !isEnvName(key) {
		return fmt.Errorf("invalid secret name %q — use letters, digits, and _", key)
	}
	lines, err := readEnvLines(path)
	if err != nil {
		return err
	}
	out := lines[:0]
	for _, l := range lines {
		if k, ok := parseEnvLine(l); ok && k == key {
			continue
		}
		out = append(out, l)
	}
	return writeEnv(path, out)
}

// readEnvLines returns the file's lines; a missing file reads as no lines.
func readEnvLines(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, nil
}

// writeEnv writes the .env atomically with owner-only permissions.
func writeEnv(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func isLetter(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
}
