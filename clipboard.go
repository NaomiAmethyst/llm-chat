// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

// Reading an image from the system clipboard. A terminal can only paste
// text, so image paste works the way it does in other terminal tools: a key
// (Ctrl+V or Alt+V) or /paste makes llm-chat ask the platform's clipboard
// tool for the image itself. Over SSH this reads the remote clipboard, so
// attach files by path there instead.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const clipboardTimeout = 10 * time.Second

var errNoClipboardImage = errors.New("no image on the clipboard")

func readClipboardImage() ([]byte, error) {
	switch runtime.GOOS {
	case "darwin":
		return clipboardDarwin()
	case "windows":
		return nil, errors.New("clipboard images are not supported on Windows yet; attach a file with <attach:PATH>")
	default:
		return clipboardUnix()
	}
}

func runClipboardTool(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr // never let the tool write over the editor
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %s", name, sanitize(msg))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// clipboardUnix uses wl-paste on Wayland and xclip on X11.
func clipboardUnix() ([]byte, error) {
	have := func(name string) bool { _, err := exec.LookPath(name); return err == nil }
	var list func() ([]byte, error)
	var get func(mime string) ([]byte, error)
	switch {
	case os.Getenv("WAYLAND_DISPLAY") != "" && have("wl-paste"):
		list = func() ([]byte, error) { return runClipboardTool("wl-paste", "--list-types") }
		get = func(mime string) ([]byte, error) { return runClipboardTool("wl-paste", "--no-newline", "--type", mime) }
	case os.Getenv("DISPLAY") != "" && have("xclip"):
		list = func() ([]byte, error) {
			return runClipboardTool("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
		}
		get = func(mime string) ([]byte, error) {
			return runClipboardTool("xclip", "-selection", "clipboard", "-t", mime, "-o")
		}
	case os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") == "":
		return nil, errors.New("no graphical session to read a clipboard from (over SSH, attach a file with <attach:PATH>)")
	default:
		return nil, errors.New("reading the clipboard needs wl-clipboard (Wayland) or xclip (X11)")
	}
	types, err := list()
	if err != nil {
		return nil, errNoClipboardImage // both tools fail when the clipboard is empty
	}
	mime := pickImageType(strings.Fields(string(types)))
	if mime == "" {
		return nil, errNoClipboardImage
	}
	return get(mime)
}

// pickImageType chooses the best image format the clipboard offers.
func pickImageType(offered []string) string {
	for _, want := range []string{"image/png", "image/jpeg", "image/webp", "image/gif"} {
		for _, t := range offered {
			if t == want {
				return want
			}
		}
	}
	return ""
}

// clipboardDarwin asks AppleScript to write the clipboard's PNG data to a
// temporary file; macOS has no stock command that prints it.
func clipboardDarwin() ([]byte, error) {
	f, err := os.CreateTemp("", "llm-chat-paste-*.png")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	var args []string
	for _, line := range []string{
		"on run argv",
		"try",
		"set img to the clipboard as «class PNGf»",
		"on error",
		`error "no image on the clipboard"`,
		"end try",
		"set fh to open for access (POSIX file (item 1 of argv)) with write permission",
		"set eof fh to 0",
		"write img to fh",
		"close access fh",
		"end run",
	} {
		args = append(args, "-e", line)
	}
	if _, err := runClipboardTool("osascript", append(args, path)...); err != nil {
		if strings.Contains(err.Error(), "no image on the clipboard") {
			return nil, errNoClipboardImage
		}
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errNoClipboardImage
	}
	return data, nil
}
