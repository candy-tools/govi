package player

import (
	"github.com/candy-tools/govi/internal/logging"
	mpv "github.com/gen2brain/go-mpv"
	"github.com/go-gl/glfw/v3.3/glfw"
)

// forwardMpvLogs pumps libmpv's event queue, forwarding log messages to the
// logger under component=mpv, observed property changes (idle-active) to the
// player state, and end-file events to the auto-advance flag. It returns when
// mpv shuts down. Run it on its own goroutine: mpv_wait_event must not be
// called from two threads, so this pump is the only consumer of the event
// queue.
func forwardMpvLogs(p *Player) {
	l := p.log.With("component", "mpv")
	for {
		ev := p.mpv.WaitEvent(-1)
		if ev == nil {
			return
		}
		switch ev.EventID {
		case mpv.EventShutdown:
			return
		case mpv.EventLogMsg:
			msg := ev.LogMessage()
			l.Log(nil, logging.MpvLevel(msg.Level), msg.Text, "prefix", msg.Prefix) //nolint:staticcheck // slog accepts a nil context
		case mpv.EventStart:
			// A file starting supersedes any end-file still flagged: mpv queues
			// the two together when one file replaces another.
			p.noteFileStart()
		case mpv.EventEnd:
			// Only flag it here; the main loop loads the next entry (the
			// event goroutine must not call mpv or touch player state).
			p.noteEndFile(ev.EndFile().Reason)
			glfw.PostEmptyEvent() // wake the loop so it advances without waiting out the timeout
		case mpv.EventPropertyChange:
			prop := ev.Property()
			// Position and duration are cached here so the render loop never
			// reads them synchronously: such a read blocks on mpv's 200 ms
			// render timeout after a seek and stutters the video (see
			// playback.go).
			p.notePlaybackProp(prop.Name, prop.Data)
			if prop.Name == "volume" {
				// Cached for the same reason: the control bar's volume slider
				// follows the level on every frame it is visible (volume.go).
				p.noteVolumeProp(prop.Data)
			}
			if prop.Name == "idle-active" {
				// FormatFlag comes back as int (0/1).
				if v, ok := prop.Data.(int); ok {
					p.idle.Store(v != 0)
					glfw.PostEmptyEvent() // wake the loop so the overlay repaints
				}
			}
		}
	}
}
