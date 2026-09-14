package player

import (
	"image/color"
	"os"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"github.com/candy-tools/govi/libs/trash"
)

// currentPath returns the loaded file's path, or "" when idle / unavailable.
func (p *Player) currentPath() string {
	if p.idle.Load() {
		return ""
	}
	return p.propStr("path")
}

// removalSucceeded reports whether path is really gone, so the playlist only
// advances past a file that was actually removed. The filesystem is checked in
// addition to err because a backend can report success while leaving the file in
// place (e.g. a copy succeeded but the unlink did not); advancing then hides a
// file that is still on disk.
func removalSucceeded(path string, err error) bool {
	if err != nil {
		return false
	}
	_, statErr := os.Lstat(path)
	return os.IsNotExist(statErr)
}

// moveToTrash stops playback and moves the current file to the OS trash.
// No confirmation: the file is recoverable from the trash.
//
// This runs synchronously on the main loop, which is safe because trashing is
// always a rename within one filesystem — libs/trash refuses to copy across
// devices rather than blocking on a multi-gigabyte transfer.
func (p *Player) moveToTrash() {
	path := p.currentPath()
	if path == "" {
		return
	}
	p.stop()
	err := trash.Move(path)
	if err != nil {
		p.log.Error("move to trash", "path", path, "err", err)
	}
	if !removalSucceeded(path, err) {
		return
	}
	p.log.Info("moved to trash", "path", path)
	p.advanceAfterRemoval(path)
}

// askDeleteFile opens the confirmation overlay for permanent deletion.
func (p *Player) askDeleteFile() {
	path := p.currentPath()
	if path == "" {
		return
	}
	p.confirmPath = path
	p.overlay = overlayConfirm
}

// closeConfirm dismisses the confirmation overlay without deleting.
func (p *Player) closeConfirm() {
	p.confirmPath = ""
	if p.overlay == overlayConfirm {
		p.overlay = overlayNone
	}
}

// deleteConfirmed stops playback and permanently removes the file the
// confirmation overlay was opened for.
func (p *Player) deleteConfirmed() {
	path := p.confirmPath
	p.closeConfirm()
	if path == "" {
		return
	}
	p.stop()
	err := os.Remove(path)
	if err != nil {
		p.log.Error("delete file", "path", path, "err", err)
	}
	if !removalSucceeded(path, err) {
		return
	}
	p.log.Info("deleted file", "path", path)
	p.advanceAfterRemoval(path)
}

// layoutConfirm draws the centered delete-confirmation panel.
func (p *Player) layoutConfirm(gtx layout.Context) layout.Dimensions {
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return p.drawPanel(gtx, func(gtx layout.Context) layout.Dimensions {
			line := func(txt string, c color.NRGBA) layout.FlexChild {
				return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Bottom: unit.Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(p.theme, txt)
						lbl.Color = c
						return lbl.Layout(gtx)
					})
				})
			}
			white := color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
			grey := color.NRGBA{R: 0xaa, G: 0xaa, B: 0xaa, A: 0xff}
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				line("Permanently delete this file?", white),
				line(p.confirmPath, grey),
				line("Enter deletes — Esc cancels", grey),
			)
		})
	})
}
