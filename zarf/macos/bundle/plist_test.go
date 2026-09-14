package bundle

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

// baseInfo is a valid Info; tests vary one field at a time from it.
func baseInfo() Info {
	return Info{
		Name:       "govi",
		Identifier: "com.candy-tools.govi",
		Version:    "0.1.5",
		Executable: "govi",
		Icon:       "govi.icns",
		MinSystem:  "11.0",
	}
}

// decodePlist walks the document with the XML parser, which is the check that
// matters: Launch Services rejects a malformed plist by ignoring the bundle,
// with no error the user ever sees.
func decodePlist(t *testing.T, doc []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(doc))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, doc)
		}
	}
}

// value returns the text of the element following <key>name</key>, or "" when
// the key is absent. Enough to assert on scalar keys without a plist library.
func value(t *testing.T, doc []byte, name string) string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(doc))
	inKey := false
	found := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch {
			case el.Name.Local == "key":
				inKey = true
			case found && el.Name.Local == "true":
				return "true"
			case found && el.Name.Local == "false":
				return "false"
			case found && el.Name.Local == "string":
				var s string
				if err := dec.DecodeElement(&s, &el); err != nil {
					t.Fatalf("decode value of %s: %v", name, err)
				}
				return s
			}
		case xml.CharData:
			if inKey && strings.TrimSpace(string(el)) == name {
				found = true
			}
		case xml.EndElement:
			if el.Name.Local == "key" {
				inKey = false
			}
		}
	}
}

func TestInfoPlistKeys(t *testing.T) {
	doc, err := InfoPlist(baseInfo())
	if err != nil {
		t.Fatalf("InfoPlist: %v", err)
	}
	decodePlist(t, doc)

	tests := []struct{ key, want string }{
		{"CFBundleName", "govi"},
		{"CFBundleDisplayName", "govi"},
		{"CFBundleIdentifier", "com.candy-tools.govi"},
		{"CFBundleExecutable", "govi"},
		{"CFBundleShortVersionString", "0.1.5"},
		{"CFBundleVersion", "0.1.5"},
		{"CFBundlePackageType", "APPL"},
		// Without this the process gets no menu bar and no Dock icon, which is
		// the whole point of bundling.
		{"NSPrincipalClass", "NSApplication"},
		{"NSHighResolutionCapable", "true"},
		{"LSUIElement", "false"},
		{"LSMinimumSystemVersion", "11.0"},
		// The icon key names the file without its extension.
		{"CFBundleIconFile", "govi"},
	}
	for _, tc := range tests {
		if got := value(t, doc, tc.key); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// CFBundleVersion must be dotted-numeric: Homebrew reads it to decide whether a
// cask upgrade is a no-op, and macOS parses it. The display string keeps the
// full version instead.
func TestVersionKeys(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		wantShort string
		wantFull  string
	}{
		{name: "release", version: "0.1.5", wantShort: "0.1.5", wantFull: "0.1.5"},
		{name: "leading v is dropped", version: "v0.1.5", wantShort: "0.1.5", wantFull: "0.1.5"},
		{name: "prerelease suffix is kept only in the display string", version: "0.1.5-rc1", wantShort: "0.1.5-rc1", wantFull: "0.1.5"},
		{name: "snapshot suffix", version: "0.1.6-snapshot", wantShort: "0.1.6-snapshot", wantFull: "0.1.6"},
		{name: "build metadata", version: "1.2.3+abc123", wantShort: "1.2.3+abc123", wantFull: "1.2.3"},
		{name: "two components", version: "1.2", wantShort: "1.2", wantFull: "1.2"},
		{name: "non-numeric falls back", version: "dev-build", wantShort: "dev-build", wantFull: "0"},
		{name: "empty falls back", version: "", wantShort: "0", wantFull: "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := baseInfo()
			info.Version = tc.version
			doc, err := InfoPlist(info)
			if err != nil {
				t.Fatalf("InfoPlist: %v", err)
			}
			if got := value(t, doc, "CFBundleShortVersionString"); got != tc.wantShort {
				t.Errorf("CFBundleShortVersionString = %q, want %q", got, tc.wantShort)
			}
			if got := value(t, doc, "CFBundleVersion"); got != tc.wantFull {
				t.Errorf("CFBundleVersion = %q, want %q", got, tc.wantFull)
			}
		})
	}
}

// The document types are what put govi in Finder's "Open with" menu. Both
// matching mechanisms have to be present: formats like .mkv conform to no
// system UTI, so public.movie alone would never match them.
func TestDocumentTypes(t *testing.T) {
	info := baseInfo()
	info.VideoExtensions = []string{"mp4", ".MKV", "webm"}
	doc, err := InfoPlist(info)
	if err != nil {
		t.Fatalf("InfoPlist: %v", err)
	}
	decodePlist(t, doc)

	s := string(doc)
	for _, want := range []string{
		"CFBundleDocumentTypes",
		"LSItemContentTypes",
		"public.movie",
		"CFBundleTypeExtensions",
		"<string>mkv</string>", // lowercased and the dot stripped
		"<string>mp4</string>",
		"<string>webm</string>",
		"Alternate", // govi offers itself, it does not claim to own every video
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist is missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "<string>.MKV</string>") {
		t.Error("extension was not normalised to lowercase without the dot")
	}
}

func TestNoDocumentTypesWhenNoExtensions(t *testing.T) {
	doc, err := InfoPlist(baseInfo())
	if err != nil {
		t.Fatalf("InfoPlist: %v", err)
	}
	if strings.Contains(string(doc), "CFBundleDocumentTypes") {
		t.Error("declared document types with no extensions configured")
	}
}

// A value carrying XML metacharacters must not be able to break the document —
// an unescaped & yields a plist macOS silently ignores.
func TestValuesAreEscaped(t *testing.T) {
	info := baseInfo()
	info.Name = `go & vi <"player">`
	doc, err := InfoPlist(info)
	if err != nil {
		t.Fatalf("InfoPlist: %v", err)
	}
	decodePlist(t, doc)
	if got := value(t, doc, "CFBundleName"); got != info.Name {
		t.Errorf("CFBundleName = %q, want %q", got, info.Name)
	}
}

func TestInfoPlistRequiresFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Info)
	}{
		{name: "no name", mutate: func(i *Info) { i.Name = "" }},
		{name: "no identifier", mutate: func(i *Info) { i.Identifier = "" }},
		{name: "no executable", mutate: func(i *Info) { i.Executable = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			info := baseInfo()
			tc.mutate(&info)
			if _, err := InfoPlist(info); err == nil {
				t.Fatal("InfoPlist succeeded, want an error")
			}
		})
	}
}

// Release artifacts are compared across builds, so key order must be stable —
// a Go map would reshuffle it every run.
func TestInfoPlistIsDeterministic(t *testing.T) {
	info := baseInfo()
	info.VideoExtensions = []string{"mkv", "mp4", "webm"}
	first, err := InfoPlist(info)
	if err != nil {
		t.Fatalf("InfoPlist: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := InfoPlist(info)
		if err != nil {
			t.Fatalf("InfoPlist: %v", err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("InfoPlist output varies between identical calls")
		}
	}
}
