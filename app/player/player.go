// Package player implements the govi playback window: a GLFW-owned OpenGL
// context where libmpv renders video frames and Gio draws the control
// overlay into the same framebuffer, so video stays on the GPU end to end.
package player

import (
	"context"
	"fmt"
	"image"
	"log/slog"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"gioui.org/f32"
	"gioui.org/font/gofont"
	"gioui.org/gpu"
	"gioui.org/io/input"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"github.com/candy-tools/govi/internal/logging"
	mpv "github.com/gen2brain/go-mpv"
	"github.com/go-gl/gl/v3.1/gles2"
	"github.com/go-gl/gl/v3.3-core/gl"
	"github.com/go-gl/glfw/v3.3/glfw"
)

const (
	// appID is the window class / Wayland app id; it has to match the basename
	// of the installed desktop entry (zarf/govi.desktop) for the shell to
	// associate window and launcher icon.
	appID         = "govi"
	initialWidth  = 1280
	initialHeight = 720
	// minWidth/minHeight floor the video window. Square and small: the video is
	// letterboxed to fit whatever the aspect ratio, so nothing here breaks at a
	// small size — this only stops the window collapsing to a sliver, where the
	// control bar would have no room to draw.
	minWidth  = 240
	minHeight = 240
	// idleFrame caps how long the loop sleeps when no video frame or input
	// arrives, so the Gio overlay keeps animating even while paused.
	idleFrame = 0.05
)

// desktopGL selects the (core, desktop) OpenGL backend instead of OpenGL ES;
// Gio's renderer expects ES everywhere except macOS.
const desktopGL = runtime.GOOS == "darwin"

func init() {
	// GLFW event handling and GL contexts must stay on the main OS thread.
	runtime.LockOSThread()
}

type Player struct {
	log    *slog.Logger
	window *glfw.Window
	mpv    *mpv.Mpv
	render *mpv.RenderContext

	gpuCtx gpu.GPU
	router input.Router
	ops    op.Ops
	theme  *material.Theme

	// defVAO is the video context's default vertex array object, needed by the
	// forward-compatible core profile (desktopGL) and re-bound every frame
	// before Gio draws — see frame() for why once at init is not enough.
	defVAO uint32

	placeholderSrc paint.ImageOp
	logoSrc        paint.ImageOp
	paused         bool
	muted          bool
	overlay        overlayKind
	keymap         map[keyChord]actionID
	actions        map[actionID]action

	// context-menu state: items rebuilt on open, position in framebuffer px.
	menuPos   f32.Point
	menuItems []*menuItem

	// confirmPath is the file the delete-confirmation overlay is asking about.
	confirmPath string

	// preferences state: user shortcut overrides in config syntax, the save
	// callback injected via Config, the row widgets rebuilt on open, the slot
	// currently capturing a key press, and a user-facing message line.
	// prefsWanted is the requested state of the preferences window; the loop
	// reconciles it with prefsWin (see prefsWindowAction).
	overrides     map[actionID][]string
	saveShortcuts func(map[string][]string) error
	prefsWanted   bool
	prefsWin      *prefsWindow
	prefsRows     []*prefsRow
	prefsList     widget.List
	capture       *prefsCapture
	prefsMsg      string

	// pl lists the sibling videos of the last opened file for next/previous
	// navigation; nil when the source has no folder (URL) or scanning failed.
	pl *playlist

	// pf warms the OS page cache for the entries around the current one, so
	// next/previous does not wait on cold-storage reads.
	pf prefetcher

	// infoCache holds the info overlay's sections, re-read from mpv once per
	// second while the overlay is open (see loop).
	infoCache       []infoSection
	infoRefreshedAt time.Time

	// osdText is the transient status flash (playlist position — its only user
	// now) drawn until osdUntil passes; see osd.go. Playback progress and the
	// volume are not among them — the control bar reports both (controls.go).
	osdText  string
	osdUntil time.Time

	// lastRepeat is when each action last fired, so held-down keys are
	// throttled to the action's own repeat interval (see dispatchKey).
	lastRepeat map[actionID]time.Time

	// control-bar state (see controls.go): the seek and volume sliders' knob
	// positions, the two buttons, when pointer input last arrived (the bar
	// auto-hides controlsHideAfter later) and the fade-in's origin. Zero lastInput
	// means the pointer has not moved yet, so the bar starts hidden.
	progress   widget.Float
	volume     widget.Float
	playBtn    widget.Clickable
	volumeBtn  widget.Clickable
	lastInput  time.Time
	revealedAt time.Time

	// mouse-pointer auto-hide (see cursor.go): when the mouse last moved, and
	// whether the pointer is currently hidden. Only fullscreen hides it, and only
	// once the mouse has rested for cursorHideAfter. Kept apart from lastInput
	// because that one also moves on clicks and on the seek shortcuts, neither of
	// which should bring a hidden pointer back.
	lastPointerMove time.Time
	cursorHidden    bool

	// volume persistence (see volume.go): saveAudioState writes the level and
	// mute flag to disk, injected by app/cmd so the player stays YAML-free.
	// volumeSavePending is when the level last changed; the loop writes it out
	// once it has been still for volumeSaveDelay, so a drag does not rewrite the
	// config on every pointer move. Zero means nothing is waiting to be saved.
	saveAudioState    func(volume int, muted bool) error
	volumeSavePending time.Time

	// windowed geometry remembered while fullscreen, restored on exit.
	winX, winY, winW, winH int

	// needsRender is set from mpv's render thread when a new video frame is
	// ready and consumed on the main loop.
	needsRender atomic.Bool

	// idle mirrors mpv's idle-active property (no file loaded), published
	// from the mpv event goroutine and read by the render loop / overlay.
	idle atomic.Bool

	// eofPending is set from the mpv event goroutine when the current file
	// ended on its own (see autoadvance.go) and consumed on the main loop,
	// which then plays the next playlist entry.
	eofPending atomic.Bool

	// obsPos/obsDur are the observed playback position and duration
	// (see playback.go); obsVol the observed volume level (see volume.go).
	obsPos atomic.Uint64
	obsDur atomic.Uint64
	obsVol atomic.Uint64
}

// overlayKind is which single overlay (if any) is currently shown.
type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayInfo
	overlayHelp
	overlayMenu
	overlayConfirm
)

// Config is the player's runtime configuration, free of any config-library
// or YAML types. Shortcuts holds only the actions the user explicitly set
// (action id -> key strings); absent actions fall back to built-in defaults.
// SaveShortcuts and SaveAudioState, when non-nil, persist to disk; both are
// injected by app/cmd so the player never touches YAML.
//
// Volume is the saved level in percent, or nil when the user has none: a pointer
// rather than an int so an absent setting stays distinguishable from a saved 0,
// which would otherwise start every fresh install in silence.
type Config struct {
	Shortcuts        map[string][]string
	PlaceholderImage string
	Volume           *int
	Muted            bool
	SaveShortcuts    func(map[string][]string) error
	SaveAudioState   func(volume int, muted bool) error
}

// Run opens a player window and plays the given file until the window closes
// or ctx is cancelled (SIGINT/SIGTERM from app/cmd).
func Run(ctx context.Context, path string, cfg Config) error {
	p := &Player{log: slog.Default()}
	p.actions = actionByID()

	overrides := make(map[actionID][]string, len(cfg.Shortcuts))
	for id, keys := range cfg.Shortcuts {
		overrides[actionID(id)] = keys
	}
	km, err := buildKeymap(overrides)
	if err != nil {
		return fmt.Errorf("build keymap: %w", err)
	}
	p.keymap = km
	p.overrides = overrides
	p.saveShortcuts = cfg.SaveShortcuts
	p.saveAudioState = cfg.SaveAudioState

	start := time.Now()

	// Resolved before initWindow, because GLFW may change the working directory
	// during its own initialisation (see the chdir hint there) and mpv resolves a
	// relative path against the cwd it is called with. Belt and braces alongside
	// that hint: whatever a future GLFW or a plugin does to the cwd, the path the
	// user typed is already anchored to the directory they typed it in.
	path = absMediaPath(path, p.log)

	if err := p.initWindow(); err != nil {
		return err
	}
	defer p.timeTeardown("glfw.Terminate", glfw.Terminate)
	defer p.timeTeardown("window.Destroy", p.window.Destroy)
	p.log.Debug("window and GL context ready", "elapsed", time.Since(start))

	if err := p.initGio(); err != nil {
		return err
	}
	defer p.timeTeardown("gio.Release", p.gpuCtx.Release)
	p.log.Debug("gio renderer ready", "elapsed", time.Since(start))

	img := loadPlaceholder(cfg.PlaceholderImage, p.log)
	p.placeholderSrc = paint.NewImageOp(img)
	p.logoSrc = paint.NewImageOp(loadLogo(p.log))

	if err := p.initMpv(cfg.Volume); err != nil {
		return err
	}
	// Mute is a property, not an option: it has to be applied after Initialize.
	p.applyAudioState(cfg.Muted)
	p.log.Debug("mpv initialized", "elapsed", time.Since(start))

	// Pump mpv's event queue for log messages on its own goroutine. libmpv
	// forbids terminate_destroy while a thread is in wait_event, so shut
	// down in order: quit -> pump drains and returns on shutdown event ->
	// terminate. The render context must also be freed before terminate.
	logsDone := make(chan struct{})
	go func() {
		forwardMpvLogs(p)
		close(logsDone)
	}()
	defer func() {
		p.timeTeardown("mpv.quit", func() {
			_ = p.mpv.Command([]string{"quit"}) // best-effort shutdown
			select {
			case <-logsDone:
			case <-time.After(3 * time.Second):
				p.log.Warn("mpv log pump did not stop in time")
			}
		})
		p.timeTeardown("render.Free", p.render.Free)
		p.timeTeardown("mpv.TerminateDestroy", p.mpv.TerminateDestroy)
	}()

	// Stop warming neighbours before mpv goes away; the reads are best-effort
	// and must not outlive the player.
	defer p.pf.stop()

	p.registerCallbacks()

	// A cold start from Finder (double-clicking a video when govi is not running)
	// passes no argument: the path came in as an open-document event while
	// initWindow finished AppKit's launch sequence, and is waiting in the queue.
	// Take it now rather than leaving it to the loop, so the file is loaded before
	// the first frame instead of after a visible flash of the idle screen. Always
	// absolute — it comes from Cocoa's fileSystemRepresentation, not from argv.
	if path == "" {
		path = takePendingOpen()
	}

	// `govi .` or `govi <dir>` opens a folder: resolve it to the folder's first
	// video so the whole folder plays like `govi <that file>` (setPlaylist below
	// lands the playlist on it). A bare `govi` (empty path) stays empty and lands
	// on the idle screen.
	path = resolveStartPath(path)

	if path != "" {
		if err := p.mpv.Command([]string{"loadfile", path}); err != nil {
			return fmt.Errorf("loadfile %q: %w", path, err)
		}
		p.setPlaylist(path)
		p.log.Info("playing", "file", path, "startup", time.Since(start))
	} else {
		p.log.Info("started on idle screen", "startup", time.Since(start))
	}

	return p.loop(ctx)
}

// setMinSize floors a window's size at minW x minH, given in dp.
//
// GLFW takes size limits in screen coordinates, and ScaleToMonitor is set on
// both windows, so the requested size is multiplied by the monitor's content
// scale. The limits get the same multiplication, otherwise a 200% display would
// enforce a floor half the intended size in dp. GetContentScale is read from the
// created window, which is already on its target monitor.
//
// glfw.DontCare leaves the maximum unbounded — a floor must not also cap.
func setMinSize(win *glfw.Window, minW, minH int) {
	sx, sy := win.GetContentScale()
	if sx <= 0 || sy <= 0 { // no monitor info (headless/odd backend): use dp as-is
		sx, sy = 1, 1
	}
	win.SetSizeLimits(int(float32(minW)*sx), int(float32(minH)*sy), glfw.DontCare, glfw.DontCare)
}

func (p *Player) initWindow() error {
	// GLFW's macOS backend chdirs into Contents/Resources when it initialises
	// inside an .app bundle, and that hint defaults to on. Since v0.1.5 govi's
	// executable lives in the bundle, so the default would move the process out
	// of the directory the user ran it from and silently break every relative
	// path handed to mpv or scanPlaylist. No-op off macOS.
	glfw.InitHint(glfw.CocoaChdirResources, glfw.False)
	if err := glfw.Init(); err != nil {
		return fmt.Errorf("init glfw: %w", err)
	}

	// Called here, between glfw.Init and glfw.CreateWindow, and both bounds are
	// load-bearing on macOS:
	//   - after Init, because it grafts onto the NSApplication delegate Init
	//     creates (see openfiles_darwin.m);
	//   - before CreateWindow, because this is also what finishes AppKit's launch
	//     sequence, and CreateWindow is what would otherwise block in [NSApp run]
	//     waiting for a launch notification a bundled govi never receives. The
	//     same call delivers a cold start's open-document event — double-clicking
	//     a video when govi is not already running — so the graft has to be in
	//     place first or that file is dropped.
	// A failure here is fatal on macOS: without a finished launch sequence
	// CreateWindow would hang instead of returning an error, which is the bug
	// this replaced (a bundled govi run from a shell never opened a window).
	if err := installOpenFilesHandler(); err != nil {
		glfw.Terminate()
		return fmt.Errorf("finish appkit launch: %w", err)
	}

	// The backbuffer must stay linear: mpv already outputs sRGB-encoded
	// pixels, so an sRGB-encoding framebuffer would gamma-encode them a
	// second time (washed-out colors). On GLES the sRGB EGL surface cannot
	// be toggled off per draw, so don't request one — Gio detects the
	// linear framebuffer and gamma-corrects its overlay through its own
	// intermediate sRGB FBO. On desktop GL, Gio instead expects an
	// sRGB-capable backbuffer and enables FRAMEBUFFER_SRGB only while it
	// draws; encoding is off while mpv renders (see frame()).
	if desktopGL {
		glfw.WindowHint(glfw.SRGBCapable, glfw.True)
	}
	glfw.WindowHint(glfw.ScaleToMonitor, glfw.True)
	glfw.WindowHint(glfw.CocoaRetinaFramebuffer, glfw.True)
	// A fullscreen window (SetMonitor in toggleFullscreen) defaults to
	// auto-iconifying on focus loss, which minimises the player the moment
	// another window is focused — unwanted on a multi-monitor setup where the
	// video plays fullscreen on one monitor while you work on another. Keep it
	// up instead. The "restore the previous video mode" half is moot here: we
	// fullscreen to the monitor's current mode, so nothing is restored.
	glfw.WindowHint(glfw.AutoIconify, glfw.False)
	// WM_CLASS must match StartupWMClass in zarf/govi.desktop, otherwise
	// the desktop shell cannot tie the window to the .desktop entry (no icon in
	// the task bar, no pinning). Without these hints GLFW derives the class from
	// the window title, which changes with the playing file. Hints are sticky,
	// so the preferences window inherits the same class — intended, it belongs
	// to the same application.
	glfw.WindowHintString(glfw.X11ClassName, appID)
	glfw.WindowHintString(glfw.X11InstanceName, appID)
	if desktopGL {
		glfw.WindowHint(glfw.ContextVersionMajor, 3)
		glfw.WindowHint(glfw.ContextVersionMinor, 3)
		glfw.WindowHint(glfw.OpenGLProfile, glfw.OpenGLCoreProfile)
		glfw.WindowHint(glfw.OpenGLForwardCompatible, glfw.True)
	} else {
		glfw.WindowHint(glfw.ContextCreationAPI, glfw.EGLContextAPI)
		glfw.WindowHint(glfw.ClientAPI, glfw.OpenGLESAPI)
		glfw.WindowHint(glfw.ContextVersionMajor, 3)
		glfw.WindowHint(glfw.ContextVersionMinor, 0)
	}

	window, err := glfw.CreateWindow(initialWidth, initialHeight, appID, nil, nil)
	if err != nil {
		glfw.Terminate()
		return fmt.Errorf("create window: %w", err)
	}
	p.window = window
	setMinSize(window, minWidth, minHeight)
	p.window.MakeContextCurrent()
	glfw.SwapInterval(1)

	if desktopGL {
		err = gl.Init()
	} else {
		err = gles2.Init()
	}
	if err != nil {
		return fmt.Errorf("init gl: %w", err)
	}
	if desktopGL {
		// Note: GL_FRAMEBUFFER_SRGB stays disabled here. Gio enables it for
		// its own pass and restores it; leaving it on globally would make
		// mpv's already-encoded output get gamma-encoded twice.
		// Set up default VAO, required for the forward-compatible core profile.
		// Kept on the Player because frame() has to re-bind it every iteration.
		gl.GenVertexArrays(1, &p.defVAO)
		gl.BindVertexArray(p.defVAO)
	}
	return nil
}

func (p *Player) initGio() error {
	p.theme = material.NewTheme()
	p.theme.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))

	gpuCtx, err := gpu.New(gpu.OpenGL{ES: !desktopGL, Shared: true})
	if err != nil {
		return fmt.Errorf("init gio gpu: %w", err)
	}
	p.gpuCtx = gpuCtx
	return nil
}

// mpvOptions is the option set libmpv is configured with before Initialize.
// startVol is the user's saved volume, or nil to leave mpv's own default alone.
// Split from initMpv so it is testable: initMpv itself builds a GL render context
// and so needs a GLFW context no display-free test has.
func mpvOptions(startVol *int) map[string]string {
	opts := map[string]string{
		"vo": "libmpv",
		// Kept equal to the control bar's slider range (see volumeMax).
		"volume-max": volumeMaxOption(),
		"idle":       "yes",
		// Priority list instead of plain "auto": every vendor is still probed
		// (see AGENTS.md), but cheap-to-fail probes come first so the CUDA
		// probe (~650ms of cuInit on machines without an NVIDIA GPU) only
		// runs when nothing faster matched. vdpau stays behind nvdec so
		// NVIDIA machines pick NVDEC over the legacy VDPAU path. The trailing
		// "auto" keeps coverage for anything not listed explicitly.
		"hwdec":        "videotoolbox,d3d11va,d3d11va-copy,vaapi,vaapi-copy,nvdec,nvdec-copy,vdpau,vdpau-copy,auto",
		"hwdec-codecs": "all",
	}
	// An option rather than a property set after Initialize, so playback starts at
	// the saved level instead of stepping up to it audibly on the first frames.
	if v, ok := startVolume(startVol); ok {
		opts["volume"] = v
	}
	return opts
}

// initMpv creates and configures the libmpv handle. startVol is the user's saved
// volume, or nil to leave mpv's own default in place.
func (p *Player) initMpv(startVol *int) error {
	p.mpv = mpv.New()

	for k, v := range mpvOptions(startVol) {
		if err := p.mpv.SetOptionString(k, v); err != nil {
			return fmt.Errorf("set mpv option %s=%s: %w", k, v, err)
		}
	}

	// Ask libmpv for log messages matching the logger's verbosity; the pump
	// goroutine forwards them (component=mpv). This is the primary tool for
	// diagnosing startup latency: hwdec probing, audio init and first-frame
	// timing all show up here at debug/verbose level.
	minLevel := slog.LevelError
	for _, l := range []slog.Level{logging.LevelTrace, slog.LevelDebug, slog.LevelInfo, slog.LevelWarn} {
		if p.log.Enabled(context.Background(), l) {
			minLevel = l
			break
		}
	}
	if err := p.mpv.RequestLogMessages(logging.MpvRequestLevel(minLevel)); err != nil {
		return fmt.Errorf("request mpv log messages: %w", err)
	}

	if err := p.mpv.Initialize(); err != nil {
		return fmt.Errorf("init mpv: %w", err)
	}

	// Observe idle-active so the overlay can draw the idle screen whenever no
	// file is loaded (launch, after stop, or end-of-file). userdata 1 tags
	// this observation in the event loop.
	if err := p.mpv.ObserveProperty(1, "idle-active", mpv.FormatFlag); err != nil {
		return fmt.Errorf("observe idle-active: %w", err)
	}

	// Observe position and duration so the render loop reads them from the
	// cache: a synchronous property read from this thread blocks on mpv's
	// 200 ms render timeout after a seek (see playback.go).
	if err := p.observePlayback(); err != nil {
		return err
	}

	// Same reasoning for the volume: the control bar's slider follows it on every
	// frame the bar is visible (see volume.go).
	if err := p.observeVolume(); err != nil {
		return err
	}

	render, err := p.mpv.NewRenderContextGL(func(name string) unsafe.Pointer {
		return glfw.GetProcAddress(name)
	})
	if err != nil {
		return fmt.Errorf("create mpv render context: %w", err)
	}
	p.render = render

	// The callback runs on an mpv thread: only flag the new frame and wake
	// the main loop, never touch mpv or GL from here.
	p.render.SetUpdateCallback(func() {
		p.needsRender.Store(true)
		glfw.PostEmptyEvent()
	})
	return nil
}

// timeTeardown runs one shutdown step and logs how long it took, so `--log
// debug` shows where exit time goes (mpv's audio-device drain and hwdec release
// dominate on most machines).
func (p *Player) timeTeardown(step string, fn func()) {
	start := time.Now()
	fn()
	p.log.Debug("teardown step done", "step", step, "took", time.Since(start))
}

// hideWindows unmaps the player windows without destroying them. Called first
// on the way out: the rest of the teardown (mpv terminate, GL context release)
// takes hundreds of milliseconds during which the window would otherwise sit
// on screen, making the exit feel slow. Unmapping is safe here — the GL context
// stays current and valid for render.Free/gpuCtx.Release, which run after.
func (p *Player) hideWindows() {
	if p.prefsWin != nil {
		p.prefsWin.win.Hide()
	}
	// Hide does nothing to a fullscreen window, so drop back to windowed first.
	// No frame is drawn in between, so the windowed size is never presented.
	if p.window.GetMonitor() != nil {
		p.window.SetMonitor(nil, p.winX, p.winY, p.winW, p.winH, 0)
	}
	p.window.Hide()
	// Push the unmap to the display server now instead of leaving it queued
	// until the next event-queue poll, which the shutdown path never does.
	glfw.PollEvents()
}

func (p *Player) loop(ctx context.Context) error {
	loopStart := time.Now()
	firstFrame := false
	defer p.destroyPrefsWindow()

	// Flush a still-pending volume on the way out, so a change made in the last
	// half second before quitting is not lost. saveVolumeIfDue only writes once
	// the change has settled, which a shutdown never waits for.
	defer p.flushVolumeSave()

	// Registered after the two above so it runs before them (defers are LIFO):
	// nothing on the way out — not the config write, not mpv's teardown — should
	// happen while the window is still on screen.
	defer p.timeTeardown("hideWindows", p.hideWindows)

	// A signal can arrive while the loop sleeps in WaitEventsTimeout, so wake
	// it as soon as ctx is cancelled instead of waiting out the timeout.
	// PostEmptyEvent is one of GLFW's thread-safe calls.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			glfw.PostEmptyEvent()
		case <-stopped:
		}
	}()

	for !p.window.ShouldClose() {
		glfw.WaitEventsTimeout(idleFrame)

		if err := ctx.Err(); err != nil {
			p.log.Info("shutting down", "reason", err)
			return nil
		}

		// A quit shortcut or the window's close button only sets the flag from
		// inside a callback, so check it here rather than waiting for the loop
		// condition: the rest of this iteration would render one more frame and
		// block on vsync in SwapBuffers, adding a frame's delay to every exit.
		if p.window.ShouldClose() {
			return nil
		}

		// Create or destroy the preferences window here: GLFW forbids it from
		// inside a callback. Its close button revokes the request first.
		if p.prefsWin != nil && p.prefsWin.win.ShouldClose() {
			p.prefsWanted = false
		}
		p.syncPrefsWindow()

		// Continue with the next video when the current one ended (played
		// through, or seeked past its end). Done here rather than in the event
		// pump so loadfile stays on the main loop.
		p.handleEndOfFile()

		// Open a file Finder handed over through the macOS open-document event
		// (double-click, or a drop on the Dock icon). Handled here for the same
		// reason as end-of-file: the event arrives on the Cocoa main thread, and
		// loadfile belongs on the loop, not in a callback.
		if path := takePendingOpen(); path != "" {
			p.log.Info("opening file from the desktop", "path", path)
			p.openFile(path)
		}

		// Write the volume out once it has stopped moving. Loop-driven for the
		// same reason as the info cache: no timer needed, the loop already ticks
		// every idleFrame.
		p.saveVolumeIfDue(time.Now())

		// Hide the mouse pointer once it has rested in fullscreen, and bring it
		// back on leaving fullscreen. Loop-driven for the same reason, and here
		// rather than in a callback because nothing moves when the mouse is the
		// thing that stopped (see cursor.go).
		p.syncCursor(time.Now(), p.fullscreen())

		// Acknowledge mpv's update flag when a new video frame is ready.
		if p.needsRender.CompareAndSwap(true, false) {
			p.render.Update()
			if !firstFrame {
				firstFrame = true
				p.log.Info("first video frame ready", "since-loadfile", time.Since(loopStart))
			}
		}

		if p.overlay == overlayInfo {
			if time.Since(p.infoRefreshedAt) >= time.Second || p.infoCache == nil {
				p.infoCache = p.infoSections()
				p.infoRefreshedAt = time.Now()
			}
		} else {
			p.infoCache = nil
		}

		// Render every iteration so the overlay stays responsive even while
		// playback is paused and no new video frames arrive.
		frameStart := time.Now()
		if err := p.frame(); err != nil {
			return err
		}
		logging.Trace(p.log, "frame rendered", "took", time.Since(frameStart))

		// The preferences window renders in the same iteration, on its own
		// context (unvsynced, so it never throttles the video window).
		if p.prefsWin != nil {
			if err := p.prefsFrame(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Player) frame() error {
	scale, _ := p.window.GetContentScale()
	width, height := p.window.GetFramebufferSize()
	sz := image.Point{X: width, Y: height}

	clear()

	// mpv paints the whole viewport (flipY: GL's origin is bottom-left).
	if err := p.render.RenderGL(0, width, height, true); err != nil {
		return fmt.Errorf("mpv render: %w", err)
	}

	// mpv ends every draw with glBindVertexArray(0) (its gl_vao_unbind), and
	// Gio shares this context: its per-frame state save/restore calls
	// glVertexAttribPointer, which is illegal with no VAO bound in a core
	// profile. The resulting GL_INVALID_OPERATION sits in the context until
	// mpv's next glGetError drain reports it as
	// "after creating texture: OpenGL error INVALID_OPERATION" — an error
	// blamed on mpv that mpv did not cause. Only desktopGL (macOS) is on a
	// core profile, hence only macOS logged it. Re-bind before Gio draws.
	if desktopGL {
		gl.BindVertexArray(p.defVAO)
	}

	p.ops.Reset()
	gtx := layout.Context{
		Ops:    &p.ops,
		Now:    time.Now(),
		Source: p.router.Source(),
		Metric: unit.Metric{
			PxPerDp: scale,
			PxPerSp: scale,
		},
		Constraints: layout.Exact(sz),
	}
	p.layoutUI(gtx)

	// Gio composites the overlay into the same (default) framebuffer.
	if err := p.gpuCtx.Frame(gtx.Ops, gpu.OpenGLRenderTarget{}, sz); err != nil {
		return fmt.Errorf("gio frame: %w", err)
	}
	p.router.Frame(&p.ops)

	p.window.SwapBuffers()
	p.render.ReportSwap()
	return nil
}

func clear() {
	if desktopGL {
		gl.ClearColor(0, 0, 0, 1)
		gl.Clear(gl.COLOR_BUFFER_BIT)
	} else {
		gles2.ClearColor(0, 0, 0, 1)
		gles2.Clear(gles2.COLOR_BUFFER_BIT)
	}
}

func (p *Player) togglePause() {
	p.paused = !p.paused
	p.log.Debug("toggle pause", "paused", p.paused)
	if err := p.mpv.SetProperty("pause", mpv.FormatFlag, p.paused); err != nil {
		p.log.Error("set pause property", "paused", p.paused, "err", err)
	}
}

// openFile plays path and rebuilds the playlist from its folder, so
// next/previous navigate the new file's siblings. Non-local sources (URLs)
// clear the playlist.
func (p *Player) openFile(path string) {
	p.loadFile(path)
	p.setPlaylist(path)
}

// setPlaylist rebuilds the playlist from path's folder and warms the page
// cache for its neighbours, so the first next/previous is already in memory.
func (p *Player) setPlaylist(path string) {
	p.pl = scanPlaylist(path)
	p.pf.start(p.pl.neighbors())
}

// loadFile asks mpv to play path without touching the playlist (used for
// playlist navigation itself). Errors are logged, not fatal: mpv emits its
// own error and returns to idle for unplayable files.
func (p *Player) loadFile(path string) {
	// Any pending end-of-file belongs to the file being replaced: the user (or
	// an auto-advance already handled) picked this one, so the loop must not
	// advance again on top of it and skip an entry. Done before the mpv guard
	// so the flag is dropped even on the test path.
	p.eofPending.Store(false)
	if p.mpv == nil { // unit tests build a Player without mpv
		return
	}
	if err := p.mpv.Command([]string{"loadfile", path}); err != nil {
		p.log.Error("loadfile", "path", path, "err", err)
		return
	}
	p.log.Info("playing", "file", path)
}

// stop returns to the idle screen. mpv's stop unloads the current file.
func (p *Player) stop() {
	if err := p.mpv.Command([]string{"stop"}); err != nil {
		p.log.Error("mpv stop", "err", err)
	}
}

// toggleProgress brings the control bar up on demand (its own action), so the
// position is reachable without moving the mouse or disturbing playback, and
// sends it away again when it is already showing. The bar *is* the progress
// readout, so the key that summons it has to be the one that dismisses it —
// otherwise a keyboard-only user can only get an unobstructed picture back by
// waiting out the auto-hide.
func (p *Player) toggleProgress() {
	now := time.Now()
	if controlsShowing(now, p.lastInput, p.barDragging()) {
		p.hideControls(now)
		return
	}
	p.revealControls(now)
}

// changeVolume nudges mpv's volume by delta (percent), clamped by mpv's own
// volume-max (set from volumeMax in initMpv), and brings the control bar up to
// report the resulting level (no text overlay; see noteVolumeChanged).
func (p *Player) changeVolume(delta int) {
	if err := p.mpv.Command([]string{"add", "volume", fmt.Sprintf("%d", delta)}); err != nil {
		p.log.Error("mpv add volume", "delta", delta, "err", err)
		return
	}
	p.noteVolumeChanged()
}

// toggleMute flips mpv's mute flag and brings the control bar up, whose crossed-out
// glyph is the readout for the new state.
func (p *Player) toggleMute() {
	p.muted = !p.muted
	if err := p.mpv.SetProperty("mute", mpv.FormatFlag, p.muted); err != nil {
		p.log.Error("set mute property", "muted", p.muted, "err", err)
		return
	}
	p.noteVolumeChanged()
}

// toggleOverlay opens ov, or closes it if it is already open. Rendering is
// added in a later piece; the state field is wired now so dispatch is stable.
func (p *Player) toggleOverlay(ov overlayKind) {
	if p.overlay == ov {
		p.overlay = overlayNone
	} else {
		p.overlay = ov
	}
}

// openMenu builds the context menu appropriate to the current state and shows
// it at pos (framebuffer pixels).
func (p *Player) openMenu(pos f32.Point) {
	p.menuItems = p.buildMenu(p.idle.Load())
	p.menuPos = pos
	p.overlay = overlayMenu
}

// closeMenu hides the context menu.
func (p *Player) closeMenu() {
	if p.overlay == overlayMenu {
		p.overlay = overlayNone
	}
	p.menuItems = nil
}

// trackList reads and parses mpv's current track-list.
func (p *Player) trackList() []trackInfo {
	v, err := p.mpv.GetProperty("track-list", mpv.FormatNode)
	if err != nil {
		p.log.Error("get track-list", "err", err)
		return nil
	}
	return parseTrackList(v)
}

// setTrack selects a track by kind ("audio"->aid, "video"->vid, "sub"->sid).
// An id <= 0 disables that track ("no").
func (p *Player) setTrack(kind string, id int64) {
	prop := map[string]string{"audio": "aid", "video": "vid", "sub": "sid"}[kind]
	if prop == "" {
		return
	}
	value := fmt.Sprintf("%d", id)
	if id <= 0 {
		value = "no"
	}
	if err := p.mpv.SetPropertyString(prop, value); err != nil {
		p.log.Error("set track", "prop", prop, "value", value, "err", err)
	}
}

// setAspect sets video-aspect-override.
func (p *Player) setAspect(value string) {
	if err := p.mpv.SetPropertyString("video-aspect-override", value); err != nil {
		p.log.Error("set aspect", "value", value, "err", err)
	}
}

// setZoom sets video-zoom from a user-facing factor.
func (p *Player) setZoom(factor float64) {
	if err := p.mpv.SetProperty("video-zoom", mpv.FormatDouble, zoomToVideoZoom(factor)); err != nil {
		p.log.Error("set zoom", "factor", factor, "err", err)
	}
}

// rectangle is a screen-space box used to pick the monitor a window occupies.
type rectangle struct{ x, y, w, h int }

// pickMonitor returns the index of the monitor the window overlaps most, or 0
// if there is no overlap (e.g. window off-screen).
func pickMonitor(win rectangle, monitors []rectangle) int {
	best, bestArea := 0, -1
	for i, m := range monitors {
		ox := overlap1D(win.x, win.x+win.w, m.x, m.x+m.w)
		oy := overlap1D(win.y, win.y+win.h, m.y, m.y+m.h)
		area := ox * oy
		if area > bestArea {
			best, bestArea = i, area
		}
	}
	return best
}

func overlap1D(a0, a1, b0, b1 int) int {
	lo := a0
	if b0 > lo {
		lo = b0
	}
	hi := a1
	if b1 < hi {
		hi = b1
	}
	if hi <= lo {
		return 0
	}
	return hi - lo
}

// toggleFullscreen switches the window between windowed and borderless
// fullscreen on the monitor it mostly occupies, remembering the windowed
// geometry so it can be restored.
func (p *Player) toggleFullscreen() {
	if p.window.GetMonitor() != nil {
		p.window.SetMonitor(nil, p.winX, p.winY, p.winW, p.winH, 0)
		return
	}
	p.winX, p.winY = p.window.GetPos()
	p.winW, p.winH = p.window.GetSize()

	monitors := glfw.GetMonitors()
	if len(monitors) == 0 {
		return
	}
	rects := make([]rectangle, len(monitors))
	for i, m := range monitors {
		mx, my := m.GetPos()
		mode := m.GetVideoMode()
		rects[i] = rectangle{mx, my, mode.Width, mode.Height}
	}
	idx := pickMonitor(rectangle{p.winX, p.winY, p.winW, p.winH}, rects)
	m := monitors[idx]
	mode := m.GetVideoMode()
	mx, my := m.GetPos()
	p.window.SetMonitor(m, mx, my, mode.Width, mode.Height, mode.RefreshRate)
}
