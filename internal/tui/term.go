// Package tui implements `cm tui`, a hand-rolled terminal dashboard for magisterd.
package tui

import "golang.org/x/term"

// size returns the terminal width and height in cells for the given fd.
func size(fd int) (w, h int, err error) {
	return term.GetSize(fd)
}

// enterRaw puts the terminal in raw mode and returns a restore func that puts it
// back. The caller MUST defer restore (including on panic) so the terminal is
// never left wedged.
func enterRaw(fd int) (restore func() error, err error) {
	st, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() error { return term.Restore(fd, st) }, nil
}
