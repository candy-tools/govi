package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/candy-tools/govi/app/player"
	"github.com/candy-tools/govi/internal/logging"
)

// Logs emitted while the command runs must land in the ring buffer (so a crash
// report can include them) as well as on stderr.
func TestRootCommandTeesLogsToRingBuffer(t *testing.T) {
	rb := logging.NewRingBuffer(50)
	orig := runPlayer
	runPlayer = func(ctx context.Context, path string, cfg player.Config) error {
		slog.Default().Warn("player warning marker")
		return nil
	}
	t.Cleanup(func() { runPlayer = orig })

	cmd := newRootCommand(rb)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "config.yaml")})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(rb.Snapshot(), "player warning marker") {
		t.Fatalf("expected log tee'd into ring buffer, got:\n%s", rb.Snapshot())
	}
	if !strings.Contains(buf.String(), "player warning marker") {
		t.Fatalf("expected log on stderr too, got:\n%s", buf.String())
	}
}

// A panic must produce a crash report and then keep propagating, so the process
// still crashes with the original panic.
func TestReportPanicsReportsThenRepanics(t *testing.T) {
	var gotReason, gotDetail, gotStack string
	called := false

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected the panic to propagate after reporting")
		}
		if !called {
			t.Fatal("expected the report callback to be invoked")
		}
		if gotReason != "panic" {
			t.Fatalf("reason = %q, want panic", gotReason)
		}
		if !strings.Contains(gotDetail, "boom") {
			t.Fatalf("detail = %q, want it to contain boom", gotDetail)
		}
		if gotStack == "" {
			t.Fatal("expected a non-empty stack trace")
		}
	}()

	reportPanics(func(reason, detail, stack string) {
		called, gotReason, gotDetail, gotStack = true, reason, detail, stack
	}, func() {
		panic("boom")
	})
}
