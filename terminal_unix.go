//go:build !windows

// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

const supportsTerminalEditor = true

// stdinHasData reports whether stdin has bytes ready within the timeout.
// The editor uses it to batch redraws during pastes, to tell a typed Enter
// from a pasted newline, and to tell a bare Esc from an escape sequence.
func stdinHasData(d time.Duration) bool {
	fds := []unix.PollFd{{Fd: 0, Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(d.Milliseconds()))
	return err == nil && n > 0
}
