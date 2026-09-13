// Command macpack builds govi's macOS .app bundle and, on macOS, a .dmg.
//
// It exists because goreleaser's app_bundles and dmg sections are Pro-only. It
// is a build-time tool, never shipped: goreleaser calls it from a post-build
// hook in .goreleaser.darwin.yaml, which is also where its flags are set.
//
//	go run ./zarf/macos --binary dist/.../govi --version 0.1.5 --out dist
//
// Everything except the hdiutil call works on any OS, so `--skip-dmg` gives a
// full bundle build (and therefore a real test of this tool) on Linux.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/candy-tools/govi/zarf/macos/bundle"
	"github.com/candy-tools/govi/zarf/macos/dmg"
	"github.com/candy-tools/govi/zarf/macos/icns"
)

const (
	// appName is the user-visible name and the bundle's basename: govi.app.
	appName = "govi"
	// bundleID keys every piece of per-app state macOS keeps (Launch Services
	// registration, window frames, granted permissions). Changing it makes
	// macOS treat an upgrade as a different application.
	bundleID = "com.andresbott.govi"
	// minSystem is the oldest macOS the arm64 build targets. 12.0 because that is
	// Go's floor (go1.26 is the last release to run on macOS 12 at all), and
	// Apple Silicon shipped with 11.0, so nothing this excludes could have run
	// the binary regardless.
	//
	// It must equal MACOSX_DEPLOYMENT_TARGET in .goreleaser.darwin.yaml: this
	// value goes into Info.plist, the env var goes into the binary's
	// LC_BUILD_VERSION, and Launch Services enforces the latter. Verify below
	// fails the build when they drift.
	minSystem = "12.0"
	// bundleStageDir is where, relative to --out, the generated Info.plist and
	// .icns are mirrored for the archive step to pick up. See exportBundleFiles.
	bundleStageDir = "macos-bundle"
)

// iconPNGs are the sizes to put in the .icns, resolved against --assets.
//
// 1024 and 512 are rendered at build time from zarf/govi.svg because the
// committed PNGs stop at 256, which the Dock and Finder's icon view both
// upscale visibly. The rest reuse the committed files.
var iconPNGs = []string{
	"govi-32.png",
	"govi-64.png",
	"govi-128.png",
	"govi-256.png",
	"govi-512.png",
	"govi-1024.png",
}

// videoExtensions is what makes govi appear in Finder's "Open with" menu.
//
// Kept in step with videoExts in app/player/playlist.go and the MimeType list
// in zarf/govi.desktop. The three serve different jobs — sibling scanning,
// Linux mime association, macOS document types — so they are deliberately
// separate lists; add a new format to all three.
var videoExtensions = []string{
	"mp4", "mkv", "webm", "avi", "mov", "wmv", "flv", "m4v",
	"mpg", "mpeg", "ts", "m2ts", "ogv", "3gp", "vob",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "macpack:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		binaryPath = flag.String("binary", "", "path of the compiled darwin binary to bundle (required)")
		version    = flag.String("version", "", "release version, e.g. 0.1.5 (required)")
		outDir     = flag.String("out", "dist", "directory for the .dmg, and for govi.app when --binary is not already inside one")
		assetsDir  = flag.String("assets", "zarf", "directory holding the govi-<size>.png icon sources")
		dmgName    = flag.String("dmg-name", "", "basename of the .dmg (default: govi_<version>)")
		skipDMG    = flag.Bool("skip-dmg", false, "build only the .app; set automatically off macOS")
	)
	flag.Parse()

	if *binaryPath == "" {
		return errors.New("--binary is required")
	}
	if *version == "" {
		return errors.New("--version is required")
	}
	if _, err := os.Stat(*binaryPath); err != nil {
		return fmt.Errorf("--binary: %w", err)
	}

	iconData, err := buildIcon(*assetsDir)
	if err != nil {
		return err
	}

	// When goreleaser's `binary:` is a path ending in
	// <name>.app/Contents/MacOS/<exe>, the binary is already in its final place
	// inside the bundle: complete that bundle rather than building a second one
	// elsewhere and leaving the archive to pick up the wrong tree. An empty
	// Binary is what tells bundle.Build to leave the existing payload alone —
	// copying the file onto itself would truncate it to nothing.
	appDir, srcBinary := enclosingBundle(*binaryPath), ""
	if appDir == "" {
		appDir, srcBinary = filepath.Join(*outDir, appName+".app"), *binaryPath
	}

	if _, err := bundle.Build(bundle.Options{
		AppDir: appDir,
		Binary: srcBinary,
		Icns:   iconData,
		Info: bundle.Info{
			Name:            appName,
			Identifier:      bundleID,
			Version:         *version,
			Executable:      appName,
			Icon:            appName + ".icns",
			MinSystem:       minSystem,
			VideoExtensions: videoExtensions,
		},
	}); err != nil {
		return err
	}
	// Verify rather than trust: every failure this catches produces a bundle
	// that installs cleanly and then refuses to open with a message naming
	// nothing useful.
	if err := bundle.Verify(appDir, appName); err != nil {
		return err
	}
	// The deployment-target check is separate because it is about the binary
	// goreleaser produced, not about the tree this tool wrote: on a runner whose
	// macOS is newer than minSystem, an unset MACOSX_DEPLOYMENT_TARGET stamps the
	// runner's version into the Mach-O and Finder then refuses the bundle on
	// every older Mac. Off macOS the payload is not a Mach-O and this is a no-op.
	if err := bundle.VerifyMinOS(appDir, appName, minSystem); err != nil {
		return err
	}
	fmt.Println("macpack: built", appDir)

	// Mirror the two generated files to a fixed path, because the archive step
	// has to name them as globs in the config and goreleaser's real output
	// directory carries a microarchitecture suffix (…_darwin_arm64_v8.0) that a
	// static glob cannot predict.
	if err := exportBundleFiles(appDir, filepath.Join(*outDir, bundleStageDir)); err != nil {
		return err
	}

	if *skipDMG {
		fmt.Println("macpack: skipping the disk image (--skip-dmg)")
		return nil
	}

	name := *dmgName
	if name == "" {
		name = fmt.Sprintf("%s_%s", appName, strings.TrimPrefix(*version, "v"))
	}
	out := filepath.Join(*outDir, name+".dmg")
	stage := filepath.Join(*outDir, ".dmg-stage")
	// A stale stage dir from a previous run would make Stage refuse to proceed.
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("clear stage dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	path, err := dmg.Create(dmg.Options{
		AppDir:     appDir,
		Out:        out,
		VolumeName: appName,
		StageDir:   stage,
	})
	if err != nil {
		// Off macOS this is expected, not a failure: the bundle is built and
		// that is all a Linux run can do. The release job runs on macOS.
		if errors.Is(err, dmg.ErrNotDarwin) {
			fmt.Println("macpack: not on macOS, skipping the disk image")
			return nil
		}
		return err
	}
	fmt.Println("macpack: built", path)
	return nil
}

// enclosingBundle reports the .app directory that binaryPath already sits
// inside, when it is at the canonical <name>.app/Contents/MacOS/<exe> position,
// and "" otherwise.
//
// This is what keeps the tool independent of goreleaser's dist layout, which
// includes a microarchitecture suffix (dist/<id>_darwin_arm64_v8.0/) that is not
// worth predicting from a hook: the bundle is found relative to the binary
// goreleaser reports, whatever directory that turns out to be in.
func enclosingBundle(binaryPath string) string {
	macos := filepath.Dir(binaryPath)   // .../govi.app/Contents/MacOS
	contents := filepath.Dir(macos)     // .../govi.app/Contents
	candidate := filepath.Dir(contents) // .../govi.app
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" {
		return ""
	}
	if filepath.Ext(candidate) != ".app" {
		return ""
	}
	return candidate
}

// exportBundleFiles copies the generated Info.plist and .icns from appDir into
// stage, preserving the Contents/… layout.
//
// The archive step needs a stable path to reference; see bundleStageDir. Only
// the generated files are copied, never the binary — goreleaser already places
// that one itself, and a second copy in the archive would double its size.
func exportBundleFiles(appDir, stage string) error {
	files := []string{
		filepath.Join("Contents", "Info.plist"),
		filepath.Join("Contents", "Resources", appName+".icns"),
	}
	for _, rel := range files {
		src := filepath.Join(appDir, rel)
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("export bundle file: %w", err)
		}
		dst := filepath.Join(stage, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("export bundle file: %w", err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("export bundle file: %w", err)
		}
	}
	return nil
}

// buildIcon assembles the .icns from the PNGs in dir.
//
// Missing sizes are skipped with a warning rather than failing the build: the
// icon degrades (macOS scales the nearest size it has) where a hard error would
// break the release over a cosmetic asset. An empty set is still an error —
// that means the assets directory is wrong, not that one file is absent.
func buildIcon(dir string) ([]byte, error) {
	var pngs [][]byte
	for _, name := range iconPNGs {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "macpack: skipping icon size %s: %v\n", name, err)
			continue
		}
		pngs = append(pngs, data)
	}
	if len(pngs) == 0 {
		return nil, fmt.Errorf("no icon PNGs found in %s (looked for %v)", dir, iconPNGs)
	}
	return icns.Build(pngs)
}
