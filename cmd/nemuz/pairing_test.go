package main

import (
	"path/filepath"
	"testing"

	"github.com/kansaok/nemuz/internal/config"
)

func TestPairCodeRoundTripAndReuse(t *testing.T) {
	pairPath := filepath.Join(t.TempDir(), pairFileName)

	first, err := issuePairCode(pairPath, 7361357941)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 6 {
		t.Errorf("code length = %d, want 6", len(first))
	}
	again, err := issuePairCode(pairPath, 7361357941)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("second issue for the same user got a different code (%s vs %s)", again, first)
	}
	other, err := issuePairCode(pairPath, 999)
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("two users share one code")
	}
}

func TestAuthorizePairCodeWritesConfigAndConsumesCode(t *testing.T) {
	paths := config.At(t.TempDir())
	code, err := issuePairCode(filepath.Join(paths.Root, pairFileName), 7361357941)
	if err != nil {
		t.Fatal(err)
	}

	userID, err := authorizePairCode(paths, code)
	if err != nil {
		t.Fatal(err)
	}
	if userID != 7361357941 {
		t.Errorf("authorized %d", userID)
	}

	s, err := config.LoadSettings(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if s.Channels.Telegram == nil {
		t.Fatal("channel not created")
	}
	if len(s.Channels.Telegram.AllowUsers) != 1 || s.Channels.Telegram.AllowUsers[0] != 7361357941 {
		t.Errorf("allowUsers = %v", s.Channels.Telegram.AllowUsers)
	}
	want := "telegram:7361357941"
	if len(s.Commands.OwnerAllowFrom) != 1 || s.Commands.OwnerAllowFrom[0] != want {
		t.Errorf("ownerAllowFrom = %v", s.Commands.OwnerAllowFrom)
	}

	if _, err := authorizePairCode(paths, code); err == nil {
		t.Error("a consumed code was accepted again")
	}
}

func TestAuthorizePairCodeRejectsUnknownCode(t *testing.T) {
	paths := config.At(t.TempDir())
	if _, err := authorizePairCode(paths, "XXXXXX"); err == nil {
		t.Error("an unknown code was accepted")
	}
	if _, err := authorizePairCode(config.At(t.TempDir()), "ABCDEF"); err == nil {
		t.Error("an unknown code with no pairing file was accepted")
	}
}

func TestIssuePairCodeReusesSameCodeAfterConsume(t *testing.T) {
	paths := config.At(t.TempDir())
	pairPath := filepath.Join(paths.Root, pairFileName)
	code, err := issuePairCode(pairPath, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorizePairCode(paths, code); err != nil {
		t.Fatal(err)
	}
	next, err := issuePairCode(pairPath, 7)
	if err != nil {
		t.Fatal(err)
	}
	if next == code {
		t.Error("a consumed code was handed out again")
	}
}
