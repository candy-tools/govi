package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/candy-tools/govi/app/player"
	"github.com/go-bumbu/config"
	"gopkg.in/yaml.v3"
)

// ShortcutsCfg maps each action id (via config tags) to its key list. bumbu
// cannot unmarshal into a Go map, so shortcuts are modelled as a struct with
// one field per action; the config tag reproduces the same YAML the design
// specifies. A nil/empty slice means "not configured" and the player applies
// its built-in default.
type ShortcutsCfg struct {
	PlayPause      []string `config:"play-pause"`
	Stop           []string `config:"stop"`
	VolumeUp       []string `config:"volume-up"`
	VolumeDown     []string `config:"volume-down"`
	Mute           []string `config:"mute"`
	Progress       []string `config:"progress"`
	Info           []string `config:"info"`
	Help           []string `config:"help"`
	Fullscreen     []string `config:"fullscreen"`
	Quit           []string `config:"quit"`
	SeekForward    []string `config:"seek-forward"`
	SeekBack       []string `config:"seek-back"`
	SeekForwardPct []string `config:"seek-forward-percent"`
	SeekBackPct    []string `config:"seek-back-percent"`
	NextVideo      []string `config:"next-video"`
	PrevVideo      []string `config:"previous-video"`
	Trash          []string `config:"move-to-trash"`
	Delete         []string `config:"delete-file"`
	Preferences    []string `config:"preferences"`
}

// AppCfg is the on-disk configuration.
type AppCfg struct {
	Shortcuts        ShortcutsCfg `config:"shortcuts"`
	PlaceholderImage string       `config:"placeholderImage"`
	Volume           int          `config:"volume"`
	Muted            bool         `config:"muted"`
}

// volumeUnset marks "no saved volume", so an absent key stays distinguishable
// from a configured 0 — otherwise a config that never mentions the volume would
// look like saved silence and every fresh install would start muted. loadConfig
// seeds Volume with it and bumbu only overwrites fields it actually found (in the
// file or in a GOVI_VOLUME env var), so the sentinel survives exactly when the
// user set nothing. A pointer field cannot do this job: bumbu allocates nil
// pointers before unmarshalling, so an absent key would come back as a set 0.
//
// The cost is that a literal `volume: -1` reads as unset rather than as 0. It is
// out of range either way (the player clamps into 0..volumeMax), so mpv's own
// default is a fair reading of it.
const volumeUnset = -1

// knownActions lists every valid shortcut key name, used to reject typos in
// the config file (bumbu silently drops unknown struct fields, so this check
// is done separately against the raw YAML). Must cover every registry action
// id, or a config saved from the preferences screen would fail to load.
var knownActions = map[string]bool{
	"play-pause": true, "stop": true, "volume-up": true, "volume-down": true,
	"mute": true, "progress": true, "info": true, "help": true,
	"fullscreen": true, "quit": true,
	"seek-forward": true, "seek-back": true,
	"seek-forward-percent": true, "seek-back-percent": true,
	"next-video":     true,
	"previous-video": true, "move-to-trash": true, "delete-file": true,
	"preferences": true,
}

// defaultConfigPath returns <os.UserConfigDir>/govi/config.yaml.
func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "govi", "config.yaml"), nil
}

// loadConfig loads configuration from path (missing file = defaults) with env
// overrides under the GOVI_ prefix, after validating shortcut action names.
func loadConfig(path string) (AppCfg, error) {
	// Seeded rather than zero: see volumeUnset.
	cfg := AppCfg{Volume: volumeUnset}
	if err := validateShortcutNames(path); err != nil {
		return cfg, err
	}
	_, err := config.Load(
		config.CfgFile{Path: path, Mandatory: false},
		config.EnvVar{Prefix: "GOVI"},
		config.Unmarshal{Item: &cfg},
	)
	if err != nil {
		return cfg, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}

// validateShortcutNames reads only the shortcut keys from the file (if it
// exists) and rejects any action name not in knownActions, naming the entry.
func validateShortcutNames(path string) error {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // missing file = defaults
		}
		return fmt.Errorf("read config %q: %w", path, err)
	}
	var raw struct {
		Shortcuts map[string]yaml.Node `yaml:"shortcuts"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}
	for name := range raw.Shortcuts {
		if !knownActions[name] {
			return fmt.Errorf("config %q: unknown shortcut action %q", path, name)
		}
	}
	return nil
}

// toPlayerConfig translates the on-disk config into the player's bumbu-free
// Config, including only shortcuts the user actually set.
func (c AppCfg) toPlayerConfig() player.Config {
	sc := map[string][]string{}
	add := func(name string, keys []string) {
		if len(keys) > 0 {
			sc[name] = keys
		}
	}
	add("play-pause", c.Shortcuts.PlayPause)
	add("stop", c.Shortcuts.Stop)
	add("volume-up", c.Shortcuts.VolumeUp)
	add("volume-down", c.Shortcuts.VolumeDown)
	add("mute", c.Shortcuts.Mute)
	add("progress", c.Shortcuts.Progress)
	add("info", c.Shortcuts.Info)
	add("help", c.Shortcuts.Help)
	add("fullscreen", c.Shortcuts.Fullscreen)
	add("quit", c.Shortcuts.Quit)
	add("seek-forward", c.Shortcuts.SeekForward)
	add("seek-back", c.Shortcuts.SeekBack)
	add("seek-forward-percent", c.Shortcuts.SeekForwardPct)
	add("seek-back-percent", c.Shortcuts.SeekBackPct)
	add("next-video", c.Shortcuts.NextVideo)
	add("previous-video", c.Shortcuts.PrevVideo)
	add("move-to-trash", c.Shortcuts.Trash)
	add("delete-file", c.Shortcuts.Delete)
	add("preferences", c.Shortcuts.Preferences)
	pc := player.Config{
		Shortcuts:        sc,
		PlaceholderImage: c.PlaceholderImage,
		Muted:            c.Muted,
	}
	// nil is how the player hears "no saved level, keep mpv's default".
	if c.Volume != volumeUnset {
		vol := c.Volume
		pc.Volume = &vol
	}
	return pc
}

// saveAudioState rewrites the `volume:` and `muted:` keys of the config file at
// path, preserving every other top-level key (including `shortcuts:`, which
// saveShortcuts owns — the two writers share one file and neither may drop what
// the other stored). Same mechanics as saveShortcuts: comments are not preserved,
// a missing file or directory is created, and the write is atomic.
func saveAudioState(path string, volume int, muted bool) error {
	return updateConfig(path, func(doc map[string]any) {
		doc["volume"] = volume
		doc["muted"] = muted
	})
}

// saveShortcuts rewrites the `shortcuts:` section of the config file at path,
// preserving every other top-level key. Comments are not preserved (the file
// is re-marshalled). A missing file or directory is created; the write is
// atomic (temp file + rename).
func saveShortcuts(path string, shortcuts map[string][]string) error {
	return updateConfig(path, func(doc map[string]any) {
		if len(shortcuts) == 0 {
			delete(doc, "shortcuts")
		} else {
			doc["shortcuts"] = shortcuts
		}
	})
}

// updateConfig applies edit to the config file at path as a whole YAML document,
// so a writer touching one section leaves every other top-level key intact — the
// property both savers depend on, since they write the same file at different
// times. Comments are not preserved (the file is re-marshalled). A missing file or
// directory is created; the write is atomic (temp file + rename).
func updateConfig(path string, edit func(doc map[string]any)) error {
	doc := map[string]any{}
	b, err := os.ReadFile(filepath.Clean(path))
	if err == nil {
		if uerr := yaml.Unmarshal(b, &doc); uerr != nil {
			return fmt.Errorf("parse config %q: %w", path, uerr)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	edit(doc)
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	// 0o700: the config dir holds only this user's settings, and the file
	// itself is written 0o600.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace config %q: %w", path, err)
	}
	return nil
}
