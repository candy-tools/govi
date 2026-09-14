package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/candy-tools/govi/app/metainfo"
	"github.com/candy-tools/govi/internal/logging"
)

// crashReportFile is the crash report's name inside the govi config directory.
const crashReportFile = "crash_report.txt"

// crashData is everything a crash report records. It is assembled by reportCrash
// and rendered by crashReportContents; keeping it a plain struct makes the
// formatter a pure, testable function.
type crashData struct {
	Time      time.Time
	Reason    string // "error" or "panic"
	Detail    string // the error message or the panic value
	Stack     string // goroutine stack, for panics only
	Version   string
	BuildTime string
	Commit    string
	GoVersion string
	OS        string
	Arch      string
	Env       []string // already curated by curatedEnv
	Logs      string   // tail of the log, from the RingBuffer
}

// crashEnvKeys is the allowlist of environment variables captured in a crash
// report. Security: this is an allowlist, not a denylist of secret-looking
// names — a variable is captured only if it is named here (or matches a prefix
// below), so credentials (tokens, API keys, passwords) are dropped by default.
// The set is scoped to what actually helps diagnose govi's launch and rendering:
// display/session, library resolution, and locale.
var crashEnvKeys = map[string]bool{
	"DISPLAY": true, "WAYLAND_DISPLAY": true,
	"XDG_SESSION_TYPE": true, "XDG_RUNTIME_DIR": true,
	"XDG_CURRENT_DESKTOP": true, "XDG_SESSION_DESKTOP": true, "DESKTOP_SESSION": true,
	"PATH": true, "LD_LIBRARY_PATH": true, "LD_PRELOAD": true,
	"HOME": true, "USER": true, "SHELL": true, "LANG": true, "TERM": true,
	"GDK_BACKEND": true, "QT_QPA_PLATFORM": true, "SDL_VIDEODRIVER": true,
}

// crashEnvPrefixes captures whole families of graphics/driver/locale variables
// without naming each one. Kept deliberately narrow so no secret family (AWS_,
// GITHUB_, …) is swept in.
var crashEnvPrefixes = []string{
	"LC_", "MESA_", "LIBGL_", "GALLIUM_", "EGL_", "__GL", "__NV",
	"VDPAU_", "LIBVA_", "DRI_", "GOVI_",
}

// curatedEnv filters raw "KEY=VALUE" environment entries down to the allowlist,
// returning them sorted. Malformed entries (no key before '=') are skipped.
func curatedEnv(environ []string) []string {
	out := make([]string, 0, len(crashEnvKeys))
	for _, e := range environ {
		i := strings.IndexByte(e, '=')
		if i <= 0 {
			continue
		}
		key := e[:i]
		if crashEnvKeys[key] || hasAnyPrefix(key, crashEnvPrefixes) {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// crashReportContents renders a crash report as human-pasteable text.
func crashReportContents(d crashData) string {
	var b strings.Builder
	b.WriteString("govi crash report\n=================\n\n")
	fmt.Fprintf(&b, "time:       %s\n", d.Time.Format(time.RFC3339))
	fmt.Fprintf(&b, "reason:     %s\n", d.Reason)
	fmt.Fprintf(&b, "detail:     %s\n", d.Detail)

	b.WriteString("\n[version]\n")
	fmt.Fprintf(&b, "version:    %s\n", d.Version)
	fmt.Fprintf(&b, "build time: %s\n", d.BuildTime)
	fmt.Fprintf(&b, "commit:     %s\n", d.Commit)
	fmt.Fprintf(&b, "go:         %s\n", d.GoVersion)
	fmt.Fprintf(&b, "os/arch:    %s/%s\n", d.OS, d.Arch)

	b.WriteString("\n[environment] (curated allowlist)\n")
	if len(d.Env) == 0 {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(strings.Join(d.Env, "\n"))
		b.WriteString("\n")
	}

	b.WriteString("\n[recent logs]\n")
	if d.Logs == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(d.Logs)
	}

	if d.Stack != "" {
		b.WriteString("\n[stack]\n")
		b.WriteString(d.Stack)
		b.WriteString("\n")
	}
	return b.String()
}

// writeCrashReport writes the report to path, creating parent directories. The
// file is user-only (0600): it can contain local paths and environment values,
// so it should not be world-readable. It overwrites any previous report.
func writeCrashReport(path string, d crashData) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create crash report dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(crashReportContents(d)), 0o600); err != nil {
		return fmt.Errorf("write crash report: %w", err)
	}
	return nil
}

// crashReportPath is <os.UserConfigDir>/govi/crash_report.txt, alongside the
// config file (see defaultConfigPath).
func crashReportPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "govi", crashReportFile), nil
}

// reportCrash assembles and writes a best-effort crash report. It never returns
// an error and never panics: failing to write a report must not turn a handled
// failure into a worse one, and on the panic path the original panic must still
// propagate. rb may be nil (no buffered logs available).
func reportCrash(rb *logging.RingBuffer, reason, detail, stack string) {
	defer func() { _ = recover() }()

	path, err := crashReportPath()
	if err != nil {
		return
	}
	logs := ""
	if rb != nil {
		logs = rb.Snapshot()
	}
	d := crashData{
		Time:      time.Now(),
		Reason:    reason,
		Detail:    detail,
		Stack:     stack,
		Version:   metainfo.Version,
		BuildTime: metainfo.BuildTime,
		Commit:    metainfo.ShaVer,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Env:       curatedEnv(os.Environ()),
		Logs:      logs,
	}
	if err := writeCrashReport(path, d); err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "govi: wrote crash report to %s\n", path)
}
