// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

import "time"

// Windows uses the line-based input mode until the raw terminal editor has
// a native console implementation. main rejects an explicit -interactive.
const supportsTerminalEditor = false

func stdinHasData(time.Duration) bool { return false }
