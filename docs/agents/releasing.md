# Releasing — goreleaser via tag push

See [testing.md](testing.md) for the verification gates; run `make verify`
before tagging.

## Flow

```sh
make tag version="v0.1.0"   # requires: main branch, clean tree
```

`make tag` deletes any existing local/remote tag of that name, creates an
annotated tag, and pushes it. The push triggers
`.github/workflows/release.yml` (`v*.*.*` and `v*.*.*-*` prerelease tags),
which runs goreleaser.

## Publishing targets

Two artifacts are published outside this repo's own GitHub release:

- **Homebrew cask → `candy-tools/homebrew-tap`.** The darwin run's
  `homebrew_casks` block commits `Casks/govi.rb` into the shared org tap (not
  this repo). Pushing to another repo needs a cross-repo PAT exposed as the
  `HOMEBREW_TAP_GITHUB_TOKEN` secret, set on the `release-darwin` job; the
  default `GITHUB_TOKEN` is scoped to this repo only. Users install with
  `brew tap candy-tools/tap && brew trust candy-tools/tap && brew install --cask candy-tools/tap/govi`.
- **`.deb` → `candy-tools/debian-repo`.** After the linux run builds and uploads
  the amd64 `.deb`, the `Register the .deb with the Debian repo` step calls
  `candy-tools/debian-repo/.github/actions/register@main`, which writes
  `packages/govi.json` (URL + sha256) into the APT repo and triggers its
  publish. Needs a cross-repo PAT as the `DEBIAN_REPO_TOKEN` secret. amd64 only
  for now — govi's cgo build has no arm64 target. Users install per the
  debian-repo README (add the keyring + `candy-tools.sources`, then
  `apt install govi`).

Both secrets are required for a release to succeed; without them the respective
job fails at tag-push time.

## Build constraints

- **Two independent goreleaser runs, one release.** govi links libmpv via cgo, so
  a darwin binary must be compiled on macOS, and goreleaser OSS has no
  `--split` / `continue --merge` (Pro-only). So `.goreleaser.yaml` builds
  **linux amd64 only, CGO_ENABLED=1**, and `.goreleaser.darwin.yaml` builds
  **darwin arm64** on a `macos-latest` runner. Adding a platform means adding a
  native build runner, not a goarch line. Validate both with
  `make release-check`.
- Version info is linker-stamped into `app/metainfo` (`Version`, `BuildTime`,
  `ShaVer`); dev builds show `dev-build` and stamp `BuildTime` at init.
- Artifacts: tar.gz archive (uname-compatible naming) plus nfpm packages.

## The darwin run (`.goreleaser.darwin.yaml`)

`.github/workflows/release.yml` has two jobs. `release` (ubuntu) creates the
GitHub release and its changelog; `release-darwin` (`macos-latest`, Apple
Silicon, free for public repos) then appends the darwin artifacts. The
`needs: release` edge is load-bearing — the release and its notes must exist
before the second run touches it, which is also why the darwin config sets
`release.mode: keep-existing` and `changelog.disable: true`.

Five things the darwin config must keep:

- **`MACOSX_DEPLOYMENT_TARGET=12.0`** — see
  [The deployment target](#the-deployment-target) below. Without it the release
  is unopenable from Finder on any Mac older than the build runner.
- **`tags: [pkgconfig]`** — go-mpv's default cgo path is a bare `-lmpv`, and
  clang doesn't search `/opt/homebrew/{include,lib}`. The tag switches it to
  `pkg-config: mpv`; Homebrew's mpv formula ships `lib/pkgconfig/mpv.pc`
  (built with `-Dlibmpv=true`). The workflow installs `pkgconf` explicitly
  because of this.
- **`checksums_darwin.txt`** — two runs uploading to one release must not
  produce identically named assets, and the archive template already
  disambiguates by OS but the checksum file does not.
- **arm64 only** — Intel needs a `macos-*-intel` runner, billed at 12x the
  arm64 rate per release. The generated cask has an `on_arm` block only, so
  `brew install --cask` on an Intel Mac fails with an unsupported-architecture
  error rather than installing an unrunnable binary.
- **The quarantine hook** — the binary is unsigned and un-notarized, so
  without `xattr -dr com.apple.quarantine` Gatekeeper reports "govi is
  damaged and can't be opened". Removing this hack needs a paid Apple
  Developer account and a notarization step.

**Never add `--single-target` to the darwin build.** It matches the *host*
target, so on a mismatched runner goreleaser exits 0 having produced no binary
at all. Both workflows omit it and assert a binary exists afterwards.

**goreleaser is not preinstalled on the macOS runner images** (unlike ubuntu's),
so both darwin jobs invoke it through `goreleaser/goreleaser-action@v6` rather
than as a bare `run:` command — a plain `run: goreleaser ...` fails with
`command not found` (exit 127).

## The deployment target

`.goreleaser.darwin.yaml` sets **`MACOSX_DEPLOYMENT_TARGET=12.0`** in the build
env, and it must stay equal to `minSystem` in `zarf/macos/main.go`. This is not
cosmetic: **v0.1.5 shipped without it and could not be opened from Finder at
all**, on any Mac older than the runner's.

Two separate places state a minimum OS, and only one of them is enforced:

- `LSMinimumSystemVersion` in `Info.plist` — advisory metadata.
- `LC_BUILD_VERSION.minos` in the Mach-O — what **Launch Services** checks
  before it will open the bundle.

cgo forces external linking, so *clang* writes that load command, not Go. With
no deployment target set clang stamps in **the build machine's own OS version**.
`macos-latest` is macOS 26, so v0.1.5's binary declared `minos 26.0` while its
plist said `11.0`; Finder reported "you need to upgrade to macOS 26.0" on a
15.6 machine. Running the same binary straight from a shell worked, because dyld
is laxer about the field than Launch Services — which is exactly why this looked
like a bundle problem rather than a compiler-flag one.

`bundle.VerifyMinOS` (`zarf/macos/bundle/minos.go`), called from the post-build
hook, reads the load command back and fails the build when it disagrees with
`LSMinimumSystemVersion` in either direction. Too high means the env var went
missing again; too low means the plist was bumped alone. Off macOS the payload is
not a Mach-O and the check is a no-op, so the Linux rehearsal still works. The
PR job also prints `vtool -show-build-version`, so the real number is in every
log.

**12.0, not 11.0**, because that is the Go toolchain's own floor — go1.26 is the
last release that runs on macOS 12 at all, and go1.27 will require 13. Apple
Silicon shipped with 11.0, so nothing this excludes could have run the arm64
binary anyway. When Go raises its floor again, both values move together.

## libmpv linkage

A cgo binary hardcodes the path of every dylib it loads — macOS dyld does no
library search — so the concern is govi being pinned to a versioned Cellar
path like `/opt/homebrew/Cellar/mpv/0.41.0_6/lib/libmpv.2.dylib`, which would
stop resolving on the user's next `brew upgrade mpv`.

**Homebrew already prevents this**, so nothing in the build patches anything:
`fix_dynamic_linkage` sets install names from `opt_record`, i.e.
`/opt/homebrew/opt/mpv/lib/libmpv.2.dylib`, which is the stable symlink
Homebrew repoints on upgrade. Both darwin jobs print `otool -L` on the built
binary as their last step, so the real dylib paths are visible in the log of
every PR and every release — check there if a Mac user reports govi failing to
start.

Do **not** "fix" this with `install_name_tool`: it invalidates the ad-hoc code
signature Apple Silicon requires, forcing a re-sign step, and there is nothing
to fix in the first place.

Three failure modes are accepted because no build-time change addresses them:
a libmpv soname bump (needs a rebuild and a new tag), a non-standard
`HOMEBREW_PREFIX` (`/opt/homebrew` is the only supported prefix on Apple
Silicon), and Homebrew changing the above behaviour (would surface as a user
bug report, not a red build).

**What is and isn't verified.** CI proves govi *compiles and cgo-links* on
macOS arm64 (`build-darwin` in `test.yml` runs on every PR) and that the
linkage is opt-path based. It does **not** prove govi runs: the macOS test
suite is not green (headless mpv and Gio measurement tests need a
display/font environment), and nobody has launched the binary on a Mac. Treat
the first tagged release as the real test.

## The macOS `.app` bundle (`zarf/macos`)

goreleaser's `app_bundles` and `dmg` sections are **Pro-only**, so
`zarf/macos` (a build-time Go command, never shipped) does that work from a
`builds.hooks.post` in `.goreleaser.darwin.yaml`. It is Go rather than shell
because then the fiddly parts — the `.icns` encoder, the plist writer, the DMG
staging — are unit-tested on Linux instead of only exercised on a macOS runner.

An `.app` is just a directory plus an XML manifest, which is why no Apple
tooling is involved:

```
govi.app/Contents/Info.plist
govi.app/Contents/MacOS/govi
govi.app/Contents/Resources/govi.icns
```

How the pieces fit, and why each is the way it is:

- **`binary: govi.app/Contents/MacOS/govi`** — goreleaser's `binary` accepts a
  path, so the compiled binary lands at its final position inside the bundle,
  and `archive.Pipe` reuses that same relative path as the archive
  destination. This is what lets an OSS build ship a bundle at all. A plain
  `binary: govi` would give a bare CLI: no Dock icon, no Finder association.
- **`dist/macos-bundle/`** — the archive's `files:` entries need a static path
  for the two generated files, but goreleaser's real output directory carries a
  microarchitecture suffix (`dist/govi-darwin_darwin_arm64_v8.0/`) that a
  config-time glob can't predict. The tool mirrors `Info.plist` and the
  `.icns` there for the archive to pick up. Only those two — the binary is
  already in the tree, and a second copy would double the archive.
- **`custom_block: app "govi.app"`** — no `homebrew_casks` field emits an `app`
  stanza in the OSS build (it exists only for Pro's DMG path), so it goes in as
  raw Ruby. Homebrew's `app` stanza works from a `.tar.gz` exactly as from a
  `.dmg`.
- **`binaries: ['#{appdir}/govi.app/…']`** — single-quoted so the Ruby
  interpolation survives goreleaser's template pass. Homebrew installs `app`
  before `binary` (`ORDINARY_ARTIFACT_CLASSES` in `cask/dsl.rb`), so the symlink
  target exists when it is made. One copy on disk, reachable as both an app and
  a command.
- **`CFBundleVersion` is dotted-numeric only** — `0.1.5-rc1` is not a legal
  value; the suffix lives in `CFBundleShortVersionString` instead. Homebrew
  compares bundle versions to decide whether an upgrade is a no-op.
- **`com.candy-tools.govi` must never change.** macOS keys Launch Services
  registration, saved window frames and granted permissions on the bundle id;
  changing it makes every upgrade look like a different application. It was
  rebranded once, during the move to the candy-tools org (safe only because no
  macOS release had shipped yet); it is frozen from here on.
- **The `.icns` is generated, not committed.** Sizes 512 and 1024 were rendered
  from `zarf/govi.svg` and committed as PNGs alongside the existing ones,
  because the Dock and Finder's icon view visibly upscale a 256px icon. Re-render
  with the same inkscape loop as the other sizes.
- **The `.dmg` cannot be the cask's download.** goreleaser computes checksums
  only for artifacts it produced, and this one is external, so it is uploaded
  through `release.extra_files` as an opaque asset — the install path for people
  who don't use Homebrew. The cask keeps pointing at the tar.gz.
- **The quarantine hook targets `govi.app`, not `govi`.** `-dr` recurses, so the
  executable inside is covered. It runs while the payload is still in
  `staged_path`, before the `app` stanza moves it.

`hdiutil` is the only macOS-only step; off macOS the tool builds the bundle and
skips the image, which is what makes `go run ./zarf/macos --binary … --version …`
a useful local check.

### Finder "Open with" needs more than the bundle

`CFBundleDocumentTypes` makes govi *appear* in Finder's "Open with" menu, but
that alone would open the idle screen with the file discarded: when Finder opens
a document with a bundled app it does **not** pass the path in `argv` — it sends
an `odoc` Apple Event, which AppKit routes to `-application:openFiles:` on the
NSApplication delegate, and an unhandled open event is dropped silently.

`app/player/openfiles_darwin.{go,m}` supplies that handler. Three details are
load-bearing:

- It **grafts the method onto the existing delegate's class** with
  `class_addMethod` rather than installing its own delegate: GLFW creates one in
  `_glfwPlatformInit` and depends on it for `applicationShouldTerminate` and the
  launch hooks, so replacing it would break window closing.
- **It is installed between `glfw.Init` and `glfw.CreateWindow`**, and both
  bounds matter. After Init, because that is what creates the delegate to graft
  onto. Before CreateWindow, because the same call finishes AppKit's launch
  sequence (next section), and that is what delivers a **cold start's**
  open-document event — double-clicking a video when govi isn't already running.
  Grafting after the launch sequence would work for a running instance and
  silently drop the file in the case users hit first. `Run` then takes the queued
  path before the first frame, so the idle screen doesn't flash.
- The path crosses into the loop through a **buffered channel**, not a direct
  `loadfile`. The event arrives on the Cocoa main thread inside GLFW's event
  pump, and mpv commands belong on the loop (same reasoning as
  `handleEndOfFile`; see player.md invariant 6). On a cold start the channel is
  also what carries the path across the gap between window creation and mpv
  being initialised at all. The wake-up posted alongside it is
  `goviWakeEventLoop`, **not** `glfw.PostEmptyEvent`: GLFW's version starts with
  `if (!finishedLaunching) [NSApp run];`, so on a cold start it would open a
  nested run loop from inside a delegate callback.

### govi must finish AppKit's launch sequence itself

GLFW expects `[NSApp run]` to pump the launch: `_glfwPlatformCreateWindow` calls
it and relies on `-applicationDidFinishLaunching:` to call `[NSApp stop:]` and
unwind it (`cocoa_window.m`, `cocoa_init.m`). That works for a bare binary. It
does **not** work once the executable lives inside `govi.app`, which is what
v0.1.5 changed — the cask symlinks `Contents/MacOS/govi` onto `PATH`, so even a
shell invocation is now a bundled app. The launch notification then only arrives
when the app is *activated*, which produced two bugs:

- `govi <video>` from a shell hung forever with no window and no log output —
  `CreateWindow` never returned. There wasn't even a Dock icon to click, because
  `NSApplicationActivationPolicyRegular` is set inside that same callback.
- Launched from Finder or `open`, the window appeared only after the user
  clicked the Dock icon.

`goviInstallOpenFilesHandler` therefore calls `[NSApp finishLaunching]` (once,
guarded) plus `activateIgnoringOtherApps:` before any window exists. That posts
the launch notifications synchronously, so GLFW's delegate sets its
`finishedLaunching` flag and `CreateWindow` skips `[NSApp run]` entirely. This is
why a failure from `installOpenFilesHandler` is **fatal on macOS** rather than a
warning: without a finished launch sequence `CreateWindow` hangs instead of
returning an error.

One trap comes with it: **`-finishLaunching` turns a positional command-line
argument into an open-document event.** So `govi <video>` fires the open-files
handler with a path `app/cmd` already passed to `Run` — and with AppKit's raw
copy, still relative to the working directory. Left alone, the loop's
`takePendingOpen` replays the file over the absolute path `Run` resolved, and mpv
reports "No such file or directory" for a file it had just opened. `goviLaunching`
plus `goviHasFileArgument` suppress exactly those echo events, scoped to the
`-finishLaunching` call: an open event after it is a real user action (double-click
on a running govi, drop on the Dock icon) and must still play. A cold-start
double-click also arrives inside that window, but a Finder launch has no
positional argument — the path comes as an Apple Event — so nothing is dropped.

### The bundle's working directory

GLFW's `GLFW_COCOA_CHDIR_RESOURCES` init hint defaults to **on**, and
`_glfwPlatformInit` acts on it by `chdir`ing into `Contents/Resources`. Harmless
for a bare binary; for the bundled build it would move the process out of the
directory the user typed the command in and break every relative path handed to
mpv or `scanPlaylist`. `initWindow` turns it off (`glfw.InitHint`, a no-op off
macOS), and `Run` additionally resolves the media path through `absMediaPath`
before `initWindow` runs, so the argument is anchored regardless of what else
touches the cwd. `absMediaPath` only substitutes the absolute form when it
**exists**: absolutising a name that is not in the working directory would make
mpv's error name a path the user never typed, and would break the prefixes mpv
resolves but `filepath` does not (`dvd://1` and friends).

Two document-type entries are declared, not one: `public.movie` covers the
formats with a system UTI, and the extension list covers those without one
(`.mkv`, `.webm` and `.ogv` conform to no UTI and would otherwise never match).
Both are `LSHandlerRank: Alternate` — govi offers itself without claiming to own
every video on the disk. That extension list is a **third** copy of the format
set, alongside `videoExts` in `app/player/playlist.go` and `MimeType=` in
`zarf/govi.desktop`; the three serve different jobs, so add a new format to all
three.

**What is verified.** `plutil -lint` (the parser Launch Services itself uses)
and `hdiutil imageinfo` run on every PR and every release, so a malformed
manifest or unreadable image is a red build rather than a bundle macOS silently
ignores. The `.icns` layout, plist contents and DMG staging are unit-tested on
Linux. **Not** verified: that the Apple Event actually arrives — that needs
someone to double-click a video on a Mac.

## Desktop integration (`.deb` only)

The nfpm package installs `zarf/govi.desktop` to
`/usr/share/applications/` and the icons from `zarf/govi.svg` /
`govi-<size>.png` into `/usr/share/icons/hicolor/`, which is what makes govi
appear in the launcher and in the file manager's "Open with" list.

- The `MimeType=` list in `govi.desktop` is the only place that decides which
  formats offer govi as a handler. It is intentionally close to `videoExts` in
  `app/player/playlist.go`, but they serve different jobs (mime types vs.
  sibling-scanning extensions) — update both when adding a format.
- `Exec=govi %f` (single local path, not `%F`/`%u`): govi takes at most one
  file argument, and a `file://` URI would break the folder playlist scan.
- `StartupWMClass=govi` must match the `appID` constant in
  `app/player/player.go`, which is fed to the GLFW `X11ClassName` /
  `X11InstanceName` hints. If they drift, the shell shows a generic icon for
  the running window. Verify with
  `xprop -id "$(xdotool search --classname govi | head -1)" WM_CLASS`.
- `StartupNotify=false` because GLFW does not consume `DESKTOP_STARTUP_ID`;
  with it enabled the shell shows a spinner until it times out.
- `zarf/postinstall.sh` / `postremove.sh` refresh the desktop, mime and
  icon caches (all guarded on the tool existing) so no re-login is needed.
- The tar.gz archive deliberately ships the binary only — desktop files
  belong to a system-wide install path the tarball does not own.
- The PNGs are rendered from the SVG:
  `for s in 32 48 64 128 256 512 1024; do inkscape -w $s -h $s zarf/govi.svg -o zarf/govi-$s.png; done`
  (`rsvg-convert -w $s -h $s` works too). They are committed, so no build-time
  dependency on either tool. 512 and 1024 exist for the macOS `.icns` (see
  above); the `.deb` installs the hicolor sizes only.
- `zarf/govi.svg` is the flattened, Inkscape-metadata-free form of the artwork
  in `zarf/design/icon.svg` — the same play glyph as the idle-screen logo
  (`app/player/assets/logo.png`, exported from the same design file). Re-export
  both when the artwork changes so the launcher icon and the idle screen agree.

## License gate

`make license-check` (part of `verify`, and a standalone CI workflow) runs
go-licence-detector against `allowedLicenses.json` with
`overrideLicenses.json` for exceptions. A new dependency with an unlisted
license fails CI — update the rules deliberately, don't bypass. The project
itself is GPL-3.0.

## Local snapshot

`make build` = `goreleaser build --snapshot --clean --single-target`; output
under `dist/` (git-ignored, `make clean` removes it).
