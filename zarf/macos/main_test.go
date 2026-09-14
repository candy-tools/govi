package main

import (
	"os"
	"path/filepath"
	"testing"
)

// enclosingBundle decides whether the binary goreleaser reported is already
// inside a bundle. Getting it wrong is destructive rather than cosmetic: a false
// positive would leave bundle.Build with no binary to place, and a false
// negative would make it copy the file over itself and truncate it.
func TestEnclosingBundle(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "goreleaser's real output path, microarch suffix and all",
			path: "dist/govi-darwin_darwin_arm64_v8.0/govi.app/Contents/MacOS/govi",
			want: "dist/govi-darwin_darwin_arm64_v8.0/govi.app",
		},
		{
			name: "absolute path",
			path: "/build/dist/x/govi.app/Contents/MacOS/govi",
			want: "/build/dist/x/govi.app",
		},
		{
			name: "bare binary is not in a bundle",
			path: "dist/govi-darwin_darwin_arm64/govi",
			want: "",
		},
		{
			name: "right depth but no .app",
			path: "dist/govi/Contents/MacOS/govi",
			want: "",
		},
		{
			name: "Contents level misspelled",
			path: "dist/govi.app/contents/MacOS/govi",
			want: "",
		},
		{
			name: "MacOS level misspelled",
			path: "dist/govi.app/Contents/macos/govi",
			want: "",
		},
		{
			name: "resources rather than the executable",
			path: "dist/govi.app/Contents/Resources/govi.icns",
			want: "",
		},
		{
			name: "empty",
			path: "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := enclosingBundle(tc.path); got != tc.want {
				t.Errorf("enclosingBundle(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestExportBundleFiles(t *testing.T) {
	tmp := t.TempDir()
	app := filepath.Join(tmp, "govi.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents/Resources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents/Info.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Contents/Resources/govi.icns"), []byte("icns"), 0o644); err != nil {
		t.Fatal(err)
	}

	stage := filepath.Join(tmp, "stage")
	if err := exportBundleFiles(app, stage); err != nil {
		t.Fatalf("exportBundleFiles: %v", err)
	}

	// The layout has to match the archive config's src paths exactly, or the
	// archive step silently warns "no files matched" and ships a bundle with no
	// manifest.
	for _, rel := range []string{"Contents/Info.plist", "Contents/Resources/govi.icns"} {
		if _, err := os.Stat(filepath.Join(stage, rel)); err != nil {
			t.Errorf("missing %s in the stage dir: %v", rel, err)
		}
	}

	// The binary must not be mirrored: it is already in the archive via
	// goreleaser's own `binary:` path, and a copy would double the archive size.
	if _, err := os.Stat(filepath.Join(stage, "Contents/MacOS/govi")); err == nil {
		t.Error("the binary was mirrored into the stage dir; only generated files should be")
	}
}

func TestExportBundleFilesReportsMissingSource(t *testing.T) {
	tmp := t.TempDir()
	if err := exportBundleFiles(filepath.Join(tmp, "absent.app"), filepath.Join(tmp, "stage")); err == nil {
		t.Fatal("exportBundleFiles succeeded with no bundle, want an error")
	}
}

// buildIcon degrades over a missing size but must fail when the assets
// directory is wrong entirely — otherwise a typo'd --assets ships an app with a
// blank icon and a green build.
func TestBuildIconRequiresAtLeastOnePNG(t *testing.T) {
	if _, err := buildIcon(t.TempDir()); err == nil {
		t.Fatal("buildIcon succeeded with no PNGs, want an error")
	}
}

func TestBuildIconFromRepoAssets(t *testing.T) {
	// The committed assets are the real input the release uses; if a size is
	// renamed or dropped this catches it here rather than on a macOS runner.
	// ".." is zarf/, the default --assets value, relative to this package.
	data, err := buildIcon("..")
	if err != nil {
		t.Fatalf("buildIcon on zarf/: %v", err)
	}
	if len(data) < 1024 {
		t.Errorf("icon is %d bytes, suspiciously small", len(data))
	}
	if string(data[:4]) != "icns" {
		t.Errorf("icon magic = %q, want icns", data[:4])
	}
}

func TestBundleIDStable(t *testing.T) {
	// The bundle ID keys macOS Launch Services identity; changing it makes every
	// upgrade look like a different app. It was rebranded once during the
	// candy-tools org migration and must not drift again — see
	// docs/agents/releasing.md.
	const want = "com.candy-tools.govi"
	if bundleID != want {
		t.Fatalf("bundleID = %q, want %q", bundleID, want)
	}
}
