package player

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/candy-tools/govi/internal/logging"
	mpv "github.com/gen2brain/go-mpv"
)

// syncBuffer guards the log buffer: the pump goroutine writes while the test
// goroutine reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestForwardMpvLogs(t *testing.T) {
	m := mpv.New()
	// "v" level: initialization reliably produces verbose messages even in a
	// headless mpv with no file loaded.
	if err := m.RequestLogMessages("v"); err != nil {
		t.Fatal(err)
	}
	if err := m.Initialize(); err != nil {
		t.Skipf("cannot initialize headless mpv: %v", err)
	}

	var buf syncBuffer
	logger := logging.New(&buf, logging.LevelTrace)

	done := make(chan struct{})
	go func() {
		forwardMpvLogs(&Player{mpv: m, log: logger})
		close(done)
	}()

	// libmpv forbids mpv_terminate_destroy while another thread is inside
	// mpv_wait_event: quit and join the pump before destroying, on every
	// exit path.
	stop := func() {
		m.Command([]string{"quit"}) //nolint:errcheck // best-effort shutdown
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("forwardMpvLogs did not return after quit")
		}
		m.TerminateDestroy()
	}

	deadline := time.After(5 * time.Second)
	for !strings.Contains(buf.String(), "component=mpv") {
		select {
		case <-deadline:
			out := buf.String()
			stop()
			t.Fatalf("no mpv log forwarded, buffer:\n%s", out)
		case <-time.After(50 * time.Millisecond):
		}
	}
	stop()
}
