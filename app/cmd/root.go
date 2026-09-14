package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	"github.com/candy-tools/govi/app/metainfo"
	"github.com/candy-tools/govi/app/player"
	"github.com/candy-tools/govi/internal/logging"
	"github.com/spf13/cobra"
)

// Execute is the entry point for the command line.
func Execute() {
	// A GUI launcher (e.g. KDE's "Open with") can hand us a stderr pipe whose
	// reader then goes away; without this, the first log line written to that
	// broken pipe would make the Go runtime kill govi with SIGPIPE (the window
	// flashes and vanishes). Ignoring it turns that write into a discarded
	// EPIPE instead. Must run before anything writes to stderr.
	signal.Ignore(syscall.SIGPIPE)

	// The ring buffer collects the tail of the log in memory; newRootCommand
	// tees the logger into it, and a crash report flushes it to disk.
	rb := logging.NewRingBuffer(0)
	report := func(reason, detail, stack string) {
		reportCrash(rb, reason, detail, stack)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Uninstall the handler once the first signal has been seen, so a second
	// one gets the default behaviour (terminate) even if the graceful shutdown
	// of mpv or the GL window wedges.
	go func() {
		<-ctx.Done()
		stop()
	}()

	var runErr error
	// A panic writes a crash report and then keeps crashing (reportPanics
	// re-panics), so the failure is still loud and the exit code non-zero.
	reportPanics(report, func() {
		runErr = newRootCommand(rb).ExecuteContext(ctx)
	})
	if runErr != nil {
		report("error", runErr.Error(), "")
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

// reportPanics runs fn; if fn panics it calls report with the panic value and a
// stack trace, then re-panics so the process still crashes. report is a
// parameter so the behaviour can be tested without touching the filesystem.
func reportPanics(report func(reason, detail, stack string), fn func()) {
	defer func() {
		if r := recover(); r != nil {
			report("panic", fmt.Sprint(r), string(debug.Stack()))
			panic(r)
		}
	}()
	fn()
}

// runPlayer is the player entry point, indirected so tests can substitute it
// (the real one opens a window). It must receive the command's context: that
// is what makes Ctrl-C quit, since Execute replaces the default SIGINT
// behaviour with cancellation.
var runPlayer = player.Run

func newRootCommand(rb *logging.RingBuffer) *cobra.Command {
	var logLevel string
	var configPath string

	cmd := &cobra.Command{
		Use:           "govi [file|dir]",
		Short:         "govi: minimalistic video player based on mpv",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.MaximumNArgs(1),
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			level, err := logging.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			w := cmd.ErrOrStderr()
			if rb != nil {
				// Tee the log into the ring buffer so a crash report can
				// include the lines leading up to the failure.
				w = io.MultiWriter(w, rb)
			}
			slog.SetDefault(logging.New(w, level))
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				p, err := defaultConfigPath()
				if err != nil {
					return err
				}
				configPath = p
			}
			cfg, err := loadConfig(configPath)
			if err != nil {
				return err
			}
			path := ""
			if len(args) > 0 {
				path = args[0]
			}
			pc := cfg.toPlayerConfig()
			cfgPath := configPath
			pc.SaveShortcuts = func(sc map[string][]string) error {
				return saveShortcuts(cfgPath, sc)
			}
			pc.SaveAudioState = func(volume int, muted bool) error {
				return saveAudioState(cfgPath, volume, muted)
			}
			return runPlayer(cmd.Context(), path, pc)
		},
	}

	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		_ = cmd.Help()
		return nil
	})

	cmd.PersistentFlags().StringVar(&logLevel, "log", "warn",
		"log level: trace, debug, info, warn or error")
	cmd.PersistentFlags().StringVar(&configPath, "config", "",
		"path to config file (default: <user config dir>/govi/config.yaml)")

	cmd.AddCommand(
		versionCmd(),
	)

	return cmd
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print version information",
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Version:    %s\n", metainfo.Version)
			_, _ = fmt.Fprintf(out, "Build date: %s\n", metainfo.BuildTime)
			_, _ = fmt.Fprintf(out, "Commit sha: %s\n", metainfo.ShaVer)
			_, _ = fmt.Fprintf(out, "Compiler:   %s\n", runtime.Version())
		},
	}
}
