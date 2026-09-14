package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/candy-tools/govi/app/player"
)

// The player must be handed the command's context, otherwise the SIGINT
// handler installed by Execute swallows Ctrl-C and nothing ever quits.
func TestRunPassesCancellableContextToPlayer(t *testing.T) {
	var got context.Context
	orig := runPlayer
	runPlayer = func(ctx context.Context, path string, cfg player.Config) error {
		got = ctx
		return nil
	}
	t.Cleanup(func() { runPlayer = orig })

	cmd := newRootCommand(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "config.yaml")})

	ctx, cancel := context.WithCancel(context.Background())
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("player did not receive a context")
	}
	if got.Err() != nil {
		t.Fatalf("context already cancelled: %v", got.Err())
	}
	cancel()
	if got.Err() == nil {
		t.Fatal("context passed to player does not observe cancellation")
	}
}

func TestVersionCmdPrintsVersionInfo(t *testing.T) {
	cmd := newRootCommand(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Version:", "Build date:", "Commit sha:", "Compiler:"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("expected %q in output, got:\n%s", want, buf.String())
		}
	}
}

func TestUnknownFlagPrintsHelp(t *testing.T) {
	cmd := newRootCommand(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--bogus"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Usage:") {
		t.Fatalf("expected help output on unknown flag, got:\n%s", buf.String())
	}
}

func TestLogFlagInvalidLevelFails(t *testing.T) {
	cmd := newRootCommand(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--log", "bogus", "version"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for --log bogus, got nil")
	}
}

func TestLogFlagAcceptedOnVersion(t *testing.T) {
	cmd := newRootCommand(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--log", "debug", "version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected --log debug to be accepted: %v", err)
	}
	if !strings.Contains(buf.String(), "Version:") {
		t.Fatalf("expected version output, got:\n%s", buf.String())
	}
}

func TestHelpFlagPrintsHelp(t *testing.T) {
	cmd := newRootCommand(nil)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Usage:") {
		t.Fatalf("expected help output, got:\n%s", buf.String())
	}
}
