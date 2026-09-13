package player

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/candy-tools/govi/internal/logging"
)

// startVolume is the level mpv starts at: the configured one when the user has a
// saved level, otherwise mpv's own default (which the player must not override —
// a hardcoded 100 would make every fresh install start at full blast).
func TestStartVolumeUsesTheConfiguredLevel(t *testing.T) {
	v := 45
	got, ok := startVolume(&v)
	if !ok {
		t.Fatal("a configured level was not applied")
	}
	if got != "45" {
		t.Errorf("startVolume(45) = %q, want \"45\"", got)
	}
}

func TestStartVolumeUnsetLeavesMpvAlone(t *testing.T) {
	if _, ok := startVolume(nil); ok {
		t.Error("an unset level still set mpv's volume option")
	}
}

// A hand-edited config can hold anything; the level is clamped into the range the
// slider can show rather than rejected, so a typo cannot leave the knob pinned
// off the end of its track.
func TestStartVolumeClampsIntoTheSliderRange(t *testing.T) {
	tests := []struct {
		name string
		vol  int
		want string
	}{
		{"above the maximum", volumeMax + 50, "100"},
		{"negative", -20, "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := startVolume(&tc.vol)
			if !ok {
				t.Fatal("a configured level was not applied")
			}
			if got != tc.want {
				t.Errorf("startVolume(%d) = %q, want %q", tc.vol, got, tc.want)
			}
		})
	}
}

// A save is due once the level has been still for volumeSaveDelay: a drag fires a
// set-volume per pointer move, and writing the config on each would mean dozens of
// rewrites per drag.
func TestVolumeSaveDueOnlyAfterTheChangeSettles(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		pending time.Time
		want    bool
	}{
		{"nothing changed", time.Time{}, false},
		{"just changed", now, false},
		{"still mid-drag", now.Add(-volumeSaveDelay / 2), false},
		{"settled", now.Add(-volumeSaveDelay - time.Millisecond), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumeSaveDue(now, tc.pending); got != tc.want {
				t.Errorf("volumeSaveDue(now, %v) = %v, want %v", tc.pending, got, tc.want)
			}
		})
	}
}

// Every keyboard and menu path to the volume arms the save; noteVolumeChanged is
// the one place they all pass through (see docs/agents/player.md).
func TestChangingTheVolumeArmsASave(t *testing.T) {
	p := headlessPlayer(t)

	p.noteVolumeChanged()

	if p.volumeSavePending.IsZero() {
		t.Error("a volume change did not arm a save")
	}
}

// The knob drag does not go through noteVolumeChanged (it already knows the level
// it just set), so it has to arm the save itself.
func TestDraggingTheVolumeArmsASave(t *testing.T) {
	p := headlessPlayer(t)

	p.setVolume(0.4)

	if p.volumeSavePending.IsZero() {
		t.Error("a volume drag did not arm a save")
	}
}

// saveVolumeIfDue writes the observed level and the mute flag through the injected
// callback, and disarms so an idle player does not rewrite the file every frame.
func TestSaveVolumeIfDueWritesTheLevelOnce(t *testing.T) {
	var gotVol []int
	var gotMuted []bool
	p := &Player{
		log:               slog.Default(),
		muted:             true,
		volumeSavePending: time.Now().Add(-volumeSaveDelay - time.Second),
		saveAudioState: func(vol int, muted bool) error {
			gotVol = append(gotVol, vol)
			gotMuted = append(gotMuted, muted)
			return nil
		},
	}
	p.noteVolumeProp(45.0)

	p.saveVolumeIfDue(time.Now())
	p.saveVolumeIfDue(time.Now()) // nothing left to save

	if len(gotVol) != 1 {
		t.Fatalf("the callback ran %d times, want 1 (the save must disarm)", len(gotVol))
	}
	if gotVol[0] != 45 {
		t.Errorf("saved volume = %v, want 45", gotVol[0])
	}
	if !gotMuted[0] {
		t.Error("saved muted = false, want true")
	}
}

func TestSaveVolumeIfDueWaitsForTheDelay(t *testing.T) {
	saved := false
	p := &Player{
		log:               slog.Default(),
		volumeSavePending: time.Now(),
		saveAudioState:    func(int, bool) error { saved = true; return nil },
	}

	p.saveVolumeIfDue(time.Now())

	if saved {
		t.Error("the level was saved before it settled")
	}
}

// A player built without the callback (unit tests, and any future embedding) must
// not panic, and must not keep an armed save forever.
func TestSaveVolumeIfDueWithoutACallbackIsANoop(t *testing.T) {
	p := &Player{log: slog.Default(), volumeSavePending: time.Now().Add(-volumeSaveDelay - time.Second)}

	p.saveVolumeIfDue(time.Now()) // must not panic

	if !p.volumeSavePending.IsZero() {
		t.Error("the save stayed armed with no callback to run, so it would retry every frame")
	}
}

// A failing write is logged and dropped, like every other runtime error in the
// player: losing the saved level must not take the UI down, and a retry every
// frame would flood the log.
func TestSaveVolumeIfDueLogsAFailedWrite(t *testing.T) {
	var buf syncBuffer
	p := &Player{
		log:               logging.New(&buf, logging.LevelTrace),
		volumeSavePending: time.Now().Add(-volumeSaveDelay - time.Second),
		saveAudioState:    func(int, bool) error { return errors.New("disk on fire") },
	}

	p.saveVolumeIfDue(time.Now())

	if !strings.Contains(buf.String(), "disk on fire") {
		t.Errorf("a failed volume save was not logged; log:\n%s", buf.String())
	}
	if !p.volumeSavePending.IsZero() {
		t.Error("a failed save stayed armed, so it would retry on every frame")
	}
}

// Quitting within the debounce window must not lose the change: the shutdown path
// flushes a pending save regardless of how long ago it was armed.
func TestFlushVolumeSaveWritesAPendingChange(t *testing.T) {
	var got int
	p := &Player{
		log:               slog.Default(),
		volumeSavePending: time.Now(), // just changed: saveVolumeIfDue would skip it
		saveAudioState:    func(vol int, _ bool) error { got = vol; return nil },
	}
	p.noteVolumeProp(45.0)

	p.flushVolumeSave()

	if got != 45 {
		t.Errorf("saved volume = %v on shutdown, want 45", got)
	}
}

// Nothing pending means nothing written: quitting must not rewrite the config on
// every run just because the player was open.
func TestFlushVolumeSaveWithNothingPendingWritesNothing(t *testing.T) {
	saved := false
	p := &Player{log: slog.Default(), saveAudioState: func(int, bool) error { saved = true; return nil }}

	p.flushVolumeSave()

	if saved {
		t.Error("shutdown wrote the config with no volume change pending")
	}
}

// Mute is persisted too, so a player quit while muted starts muted. Toggling it
// arms the save like a level change does.
func TestTogglingMuteArmsASave(t *testing.T) {
	p := headlessPlayer(t)

	p.toggleMute()

	if p.volumeSavePending.IsZero() {
		t.Error("toggling mute did not arm a save")
	}
}

// The saved level is applied as an mpv *option* before Initialize (so playback
// starts at the level rather than stepping up to it audibly), which is a different
// mechanism from the set-volume command the slider uses. initMpv itself needs a
// GLFW context for the render context it builds, so this covers the option map it
// assembles — the part that carries the decision.
func TestMpvOptionsCarryTheSavedVolume(t *testing.T) {
	vol := 45

	opts := mpvOptions(&vol)

	if got := opts["volume"]; got != "45" {
		t.Errorf("mpv volume option = %q, want \"45\"", got)
	}
}

// With nothing saved the option is absent, so mpv keeps its own default — forcing
// a level would override whatever mpv (or the user's own mpv config) chose.
func TestMpvOptionsOmitVolumeWhenNothingIsSaved(t *testing.T) {
	opts := mpvOptions(nil)

	if got, ok := opts["volume"]; ok {
		t.Errorf("mpv volume option = %q with nothing saved, want it absent", got)
	}
}

// The rest of the option map has to survive alongside it: dropping volume-max
// would silently uncouple the slider's range from mpv's ceiling.
func TestMpvOptionsKeepTheFixedOptions(t *testing.T) {
	vol := 45
	opts := mpvOptions(&vol)

	for _, k := range []string{"vo", "volume-max", "idle", "hwdec", "hwdec-codecs"} {
		if opts[k] == "" {
			t.Errorf("mpv option %q missing", k)
		}
	}
	if got := opts["volume-max"]; got != volumeMaxOption() {
		t.Errorf("volume-max = %q, want %q", got, volumeMaxOption())
	}
}

// The configured mute state is applied to mpv at startup, not just held on the
// Player: a restored muted flag with audible sound would be the worst of both.
// Read back as a string, since mpv renders a flag property as "yes"/"no" and
// propInt returns 0 for it.
func TestApplyAudioStateMutesMpv(t *testing.T) {
	p := headlessPlayer(t)

	p.applyAudioState(true)

	if !p.muted {
		t.Error("p.muted = false after restoring a muted state")
	}
	if got := p.propStr("mute"); got != "yes" {
		t.Errorf("mpv mute = %q, want \"yes\" — the restored state never reached mpv", got)
	}
}

func TestApplyAudioStateUnmutedLeavesSoundOn(t *testing.T) {
	p := headlessPlayer(t)
	p.applyAudioState(true) // start from muted, so "no" is a real change

	p.applyAudioState(false)

	if p.muted {
		t.Error("p.muted = true after restoring an unmuted state")
	}
	if got := p.propStr("mute"); got != "no" {
		t.Errorf("mpv mute = %q, want \"no\"", got)
	}
}
