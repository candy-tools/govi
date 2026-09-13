package player

import (
	"strings"
	"testing"

	"github.com/candy-tools/govi/internal/logging"
	mpv "github.com/gen2brain/go-mpv"
)

// scrub runs on the render thread (Gio's drag handling inside layoutControls),
// so it must take the duration from the observed cache like the rest of the seek
// path: a synchronous read there blocks on mpv's 200 ms render timeout and
// stutters the video.
//
// The handle here is deliberately uninitialized: it reports no duration
// synchronously, so a scrub that reads mpv gives up before sending anything,
// while one reading the cache goes on to send the command (and the dead handle
// then fails it). The error log is what separates the two.
func TestScrubUsesObservedDuration(t *testing.T) {
	var buf syncBuffer
	p := &Player{log: logging.New(&buf, logging.LevelTrace), mpv: mpv.New()}
	t.Cleanup(p.mpv.TerminateDestroy)
	p.notePlaybackProp("duration", 120.0)

	p.scrub(0.25)

	if !strings.Contains(buf.String(), "mpv scrub") {
		t.Errorf("scrub never reached mpv, so it read the duration synchronously; log:\n%s", buf.String())
	}
}

// The knob follows playback from the observed values. This is the read that
// matters most for smoothness: it happens on every frame the bar is visible,
// not just on a seek. A Player with no mpv handle pins it — a synchronous read
// would panic.
func TestProgressKnobFollowsObservedPosition(t *testing.T) {
	p := &Player{}
	p.notePlaybackProp("time-pos", 30.0)
	p.notePlaybackProp("duration", 120.0)

	p.syncProgressKnob()

	if got, want := p.progress.Value, float32(0.25); got != want {
		t.Errorf("progress.Value = %v, want %v", got, want)
	}
}
