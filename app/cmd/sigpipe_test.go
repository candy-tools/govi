//go:build linux

package cmd

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/candy-tools/govi/app/player"
)

// A desktop launcher (e.g. KDE) can hand govi a stderr pipe whose reader then
// goes away. The Go runtime kills a process that writes to a broken fd 1/2 with
// SIGPIPE unless SIGPIPE handling is installed, so the first startup log line to
// that dead pipe would take govi down ("window flashes, then nothing"). Execute
// must survive it.
//
// Re-exec subprocess pattern: the child drives the real Execute() with the
// player stubbed to point fd 2 at a reader-less pipe and then log the way govi
// does at startup; the parent asserts the child exits cleanly instead of dying
// from SIGPIPE (exit 128+13 = 141).
func TestExecuteSurvivesBrokenStderrPipe(t *testing.T) {
	if os.Getenv("GOVI_TEST_SIGPIPE_CHILD") == "1" {
		runBrokenStderrChild()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestExecuteSurvivesBrokenStderrPipe$")
	cmd.Env = append(os.Environ(), "GOVI_TEST_SIGPIPE_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			t.Fatalf("govi was killed by %v writing to a broken stderr pipe "+
				"(SIGPIPE regression)\nchild output:\n%s", ws.Signal(), out)
		}
	}
	t.Fatalf("child exited with error: %v\nchild output:\n%s", err, out)
}

// runBrokenStderrChild is the child half of TestExecuteSurvivesBrokenStderrPipe.
// It never returns to the test framework — it exits the process directly.
func runBrokenStderrChild() {
	// A config path that does not exist: loadConfig treats it as defaults, so
	// Execute reaches the (stubbed) player without needing a real config.
	tmp := filepath.Join(os.TempDir(), "govi-sigpipe-nonexistent-config.yaml")
	os.Args = []string{"govi", "--config", tmp}

	runPlayer = func(ctx context.Context, path string, cfg player.Config) error {
		// Point fd 2 at a pipe with no reader, then log the way govi does at
		// startup. Without SIGPIPE handling this write kills the process.
		r, w, err := os.Pipe()
		if err != nil {
			os.Exit(2)
		}
		if err := syscall.Dup3(int(w.Fd()), 2, 0); err != nil {
			os.Exit(2)
		}
		_ = r.Close() // no readers remain on the pipe
		slog.Default().Warn("govi startup log line to a broken stderr pipe")
		return nil
	}

	Execute()
	// Reached only if Execute survived the broken-pipe write.
	os.Exit(0)
}
