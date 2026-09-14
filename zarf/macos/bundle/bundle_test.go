package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeBinary writes a non-empty executable file and returns its path.
func fakeBinary(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "govi")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return p
}

func TestBuildLayout(t *testing.T) {
	tmp := t.TempDir()
	bin := fakeBinary(t, tmp)
	app := filepath.Join(tmp, "govi.app")

	got, err := Build(Options{
		AppDir: app,
		Binary: bin,
		Icns:   []byte("icns-payload"),
		Info: Info{
			Name:            "govi",
			Identifier:      "com.candy-tools.govi",
			Version:         "0.1.5",
			Executable:      "govi",
			Icon:            "govi.icns",
			VideoExtensions: []string{"mkv"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got != app {
		t.Errorf("Build returned %q, want %q", got, app)
	}

	for _, rel := range []string{
		"Contents/Info.plist",
		"Contents/MacOS/govi",
		"Contents/Resources/govi.icns",
	} {
		if _, err := os.Stat(filepath.Join(app, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}

	// A bundle whose executable lacks the x bit installs fine and then refuses
	// to launch, so the mode is asserted rather than assumed.
	st, err := os.Stat(filepath.Join(app, "Contents/MacOS/govi"))
	if err != nil {
		t.Fatalf("stat executable: %v", err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("executable mode = %v, want the x bit set", st.Mode().Perm())
	}

	if err := Verify(app, "govi"); err != nil {
		t.Errorf("Verify on a freshly built bundle: %v", err)
	}
}

// The icon file must land under the name Info.plist advertises, otherwise the
// app shows the generic blank-page icon.
func TestBuildIconNaming(t *testing.T) {
	tests := []struct {
		name     string
		icon     string
		wantFile string
	}{
		{name: "with extension", icon: "govi.icns", wantFile: "govi.icns"},
		{name: "without extension", icon: "govi", wantFile: "govi.icns"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			app := filepath.Join(tmp, "govi.app")
			if _, err := Build(Options{
				AppDir: app,
				Binary: fakeBinary(t, tmp),
				Icns:   []byte("icns"),
				Info: Info{
					Name:       "govi",
					Identifier: "com.candy-tools.govi",
					Executable: "govi",
					Icon:       tc.icon,
				},
			}); err != nil {
				t.Fatalf("Build: %v", err)
			}
			if _, err := os.Stat(filepath.Join(app, "Contents/Resources", tc.wantFile)); err != nil {
				t.Errorf("icon not at Contents/Resources/%s: %v", tc.wantFile, err)
			}
		})
	}
}

// Re-running over an existing bundle must replace the binary, not append to it:
// a concatenated Mach-O will not run.
func TestBuildOverwritesExistingBinary(t *testing.T) {
	tmp := t.TempDir()
	app := filepath.Join(tmp, "govi.app")
	info := Info{Name: "govi", Identifier: "com.candy-tools.govi", Executable: "govi"}

	big := filepath.Join(tmp, "big")
	if err := os.WriteFile(big, make([]byte, 4096), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Build(Options{AppDir: app, Binary: big, Info: info}); err != nil {
		t.Fatalf("first Build: %v", err)
	}

	small := fakeBinary(t, tmp)
	if _, err := Build(Options{AppDir: app, Binary: small, Info: info}); err != nil {
		t.Fatalf("second Build: %v", err)
	}

	want, err := os.Stat(small)
	if err != nil {
		t.Fatalf("stat source: %v", err)
	}
	got, err := os.Stat(filepath.Join(app, "Contents/MacOS/govi"))
	if err != nil {
		t.Fatalf("stat bundled: %v", err)
	}
	if got.Size() != want.Size() {
		t.Errorf("bundled binary is %d bytes, want %d — it was not replaced", got.Size(), want.Size())
	}
}

// Build with no Binary is how the caller wraps a binary goreleaser already put
// in place: the tree and manifest are written, the existing payload untouched.
func TestBuildWithoutBinaryKeepsExistingPayload(t *testing.T) {
	tmp := t.TempDir()
	app := filepath.Join(tmp, "govi.app")
	if err := os.MkdirAll(filepath.Join(app, macOSDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := filepath.Join(app, macOSDir, "govi")
	if err := os.WriteFile(payload, []byte("prebuilt"), 0o755); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	if _, err := Build(Options{
		AppDir: app,
		Info:   Info{Name: "govi", Identifier: "com.candy-tools.govi", Executable: "govi"},
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}

	got, err := os.ReadFile(payload)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(got) != "prebuilt" {
		t.Errorf("payload = %q, want it left alone", got)
	}
	if err := Verify(app, "govi"); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestBuildRejects(t *testing.T) {
	tmp := t.TempDir()
	bin := fakeBinary(t, tmp)
	valid := Info{Name: "govi", Identifier: "com.candy-tools.govi", Executable: "govi"}

	tests := []struct {
		name string
		opts Options
	}{
		{
			name: "no app dir",
			opts: Options{Binary: bin, Info: valid},
		},
		{
			name: "app dir without the .app extension",
			opts: Options{AppDir: filepath.Join(tmp, "govi"), Binary: bin, Info: valid},
		},
		{
			name: "no executable name",
			opts: Options{AppDir: filepath.Join(tmp, "a.app"), Binary: bin, Info: Info{Name: "govi", Identifier: "x"}},
		},
		{
			name: "missing binary",
			opts: Options{AppDir: filepath.Join(tmp, "b.app"), Binary: filepath.Join(tmp, "nope"), Info: valid},
		},
		{
			name: "icon content with no icon name",
			opts: Options{AppDir: filepath.Join(tmp, "c.app"), Binary: bin, Icns: []byte("x"), Info: valid},
		},
		{
			name: "invalid plist metadata",
			opts: Options{AppDir: filepath.Join(tmp, "d.app"), Binary: bin, Info: Info{Executable: "govi"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(tc.opts); err == nil {
				t.Fatal("Build succeeded, want an error")
			}
		})
	}
}

// Verify is the guard against shipping a bundle that installs cleanly and then
// fails to open, so each way that can happen gets a case.
func TestVerifyRejects(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, app string)
	}{
		{
			name:  "nothing there",
			setup: func(*testing.T, string) {},
		},
		{
			name: "no Info.plist",
			setup: func(t *testing.T, app string) {
				if err := os.MkdirAll(filepath.Join(app, macOSDir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(app, macOSDir, "govi"), []byte("x"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "Info.plist names a binary that is not there",
			setup: func(t *testing.T, app string) {
				if err := os.MkdirAll(filepath.Join(app, contentsDir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(app, infoPlistName), []byte("<plist/>"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "binary is not executable",
			setup: func(t *testing.T, app string) {
				if err := os.MkdirAll(filepath.Join(app, macOSDir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(app, infoPlistName), []byte("<plist/>"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(app, macOSDir, "govi"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "binary is empty",
			setup: func(t *testing.T, app string) {
				if err := os.MkdirAll(filepath.Join(app, macOSDir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(app, infoPlistName), []byte("<plist/>"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(app, macOSDir, "govi"), nil, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := filepath.Join(t.TempDir(), "govi.app")
			tc.setup(t, app)
			if err := Verify(app, "govi"); err == nil {
				t.Fatal("Verify succeeded, want an error")
			}
		})
	}
}
