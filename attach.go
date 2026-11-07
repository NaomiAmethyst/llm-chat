// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

// File attachments. A user message names files with <attach:PATH> tokens
// (or <attach:"PATH"> when the path holds spaces or '>'); "<<attach:" is a
// literal "<attach:".
//
//   - Tokens are plain text, so they can be edited in the prompt and survive
//     JSON saves and transcripts copied from the terminal.
//   - Only text the user submits is scanned. Assistant output never is, so a
//     model cannot make llm-chat upload a file by mentioning one, and text
//     without recorded attachments is always sent literally.
//   - Files are snapshotted into a content-addressed cache when the message
//     is sent and referenced by SHA-256 from then on. Editing or moving the
//     file later cannot change what an earlier message said, and the request
//     prefix stays stable for prompt caching.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	attachOpen    = "<attach:"
	attachEscape  = "<<attach:"
	maxAttachSize = 20 << 20 // providers reject much larger payloads anyway
	maxInlineText = 1 << 20  // text files are inlined into the prompt

	imageTokenEstimate = 1000 // rough; providers count image tokens differently
)

// attachment is a snapshot of one attached file.
type attachment struct {
	Path   string `json:"path"`   // absolute path the file was read from
	SHA256 string `json:"sha256"` // names the snapshot in the cache
	Type   string `json:"type"`   // MIME type
	Size   int64  `json:"size"`
}

func (a attachment) kind() string {
	switch {
	case strings.HasPrefix(a.Type, "image/"):
		return "image"
	case a.Type == "application/pdf":
		return "pdf"
	default:
		return "text"
	}
}

func (a attachment) estimate() int {
	switch a.kind() {
	case "image":
		return imageTokenEstimate
	case "pdf":
		return int(a.Size / 40)
	default:
		return int(a.Size / 4)
	}
}

// ---------------------------------------------------------------------------
// tokens

type attachToken struct {
	start, end int // byte offsets of the whole token
	path       string
}

// parseAttachTokens finds the attachment tokens in text, skipping escapes.
// Anything that does not form a complete token is left as literal text.
func parseAttachTokens(text string) []attachToken {
	var toks []attachToken
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], attachEscape) {
			i += len(attachEscape)
			continue
		}
		if !strings.HasPrefix(text[i:], attachOpen) {
			i++
			continue
		}
		rest := text[i+len(attachOpen):]
		var path string
		var n int // bytes consumed after attachOpen
		if strings.HasPrefix(rest, `"`) {
			j := strings.IndexAny(rest[1:], "\"\n")
			if j < 0 || rest[1+j] != '"' || !strings.HasPrefix(rest[2+j:], ">") {
				i++
				continue
			}
			path, n = rest[1:1+j], j+3
		} else {
			j := strings.IndexAny(rest, ">\n")
			if j < 0 || rest[j] != '>' {
				i++
				continue
			}
			path, n = rest[:j], j+1
		}
		if strings.TrimSpace(path) == "" {
			i++
			continue
		}
		end := i + len(attachOpen) + n
		toks = append(toks, attachToken{start: i, end: end, path: path})
		i = end
	}
	return toks
}

// formatAttachToken renders the token for path, abbreviating the home
// directory and quoting when the path would otherwise end the token early.
func formatAttachToken(path string) string {
	p := displayPath(path)
	if strings.ContainsRune(p, '>') || strings.TrimSpace(p) != p {
		return attachOpen + `"` + p + `">`
	}
	return attachOpen + p + ">"
}

// escapeTokens makes the given tokens literal, so they stay visible in the
// message but can never be resolved.
func escapeTokens(text string, toks []attachToken) string {
	for i := len(toks) - 1; i >= 0; i-- {
		text = text[:toks[i].start] + "<" + text[toks[i].start:]
	}
	return text
}

func unescapeAttach(s string) string { return strings.ReplaceAll(s, attachEscape, attachOpen) }

func expandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}

func displayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
		return "~" + string(filepath.Separator) + rel
	}
	return p
}

// ---------------------------------------------------------------------------
// cache

// attachCacheDir is where snapshots live: $LLM_CHAT_CACHE_DIR/files, or the
// platform cache directory (~/.cache/llm-chat/files on Linux).
func attachCacheDir() (string, error) {
	if d := os.Getenv("LLM_CHAT_CACHE_DIR"); d != "" {
		return filepath.Join(d, "files"), nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "llm-chat", "files"), nil
}

func inCache(path string) bool {
	dir, err := attachCacheDir()
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// storeBytes writes data into the cache under its hash and returns the path.
// Snapshots may be sensitive, so the cache is private to the user.
func storeBytes(data []byte, ext string) (path, sum string, err error) {
	dir, err := attachCacheDir()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	sum = sha256Hex(data)
	path = filepath.Join(dir, sum+strings.ToLower(ext))
	if _, err := os.Stat(path); err == nil {
		return path, sum, nil
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", "", err
	}
	return path, sum, nil
}

var imageTypes = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp",
}

// classify returns the MIME type for an attachable file, judged by content.
func classify(data []byte) (string, error) {
	sniffed := http.DetectContentType(data)
	if _, ok := imageTypes[sniffed]; ok {
		return sniffed, nil
	}
	if sniffed == "application/pdf" {
		return sniffed, nil
	}
	if utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		if len(data) > maxInlineText {
			return "", fmt.Errorf("text file is %s; at most %s can be inlined", humanSize(int64(len(data))), humanSize(maxInlineText))
		}
		return "text/plain", nil
	}
	return "", fmt.Errorf("unsupported file type %s (images, PDFs, and text files can be attached)", sniffed)
}

// snapshotFile reads path, checks that it can be attached, and stores it in
// the cache.
func snapshotFile(path string) (attachment, error) {
	abs, err := expandPath(path)
	if err != nil {
		return attachment{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return attachment{}, err
	}
	if !fi.Mode().IsRegular() {
		return attachment{}, errors.New("not a regular file")
	}
	if fi.Size() > maxAttachSize {
		return attachment{}, fmt.Errorf("file is %s; the limit is %s", humanSize(fi.Size()), humanSize(maxAttachSize))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return attachment{}, err
	}
	mime, err := classify(data)
	if err != nil {
		return attachment{}, err
	}
	_, sum, err := storeBytes(data, filepath.Ext(abs))
	if err != nil {
		return attachment{}, fmt.Errorf("caching snapshot: %w", err)
	}
	return attachment{Path: abs, SHA256: sum, Type: mime, Size: int64(len(data))}, nil
}

// attachmentData returns a snapshot's bytes from the cache or, failing that,
// from the original file if it still has the same content.
func attachmentData(a attachment) ([]byte, error) {
	if dir, err := attachCacheDir(); err == nil {
		p := filepath.Join(dir, a.SHA256+strings.ToLower(filepath.Ext(a.Path)))
		if data, err := os.ReadFile(p); err == nil && sha256Hex(data) == a.SHA256 {
			return data, nil
		}
	}
	if data, err := os.ReadFile(a.Path); err == nil && sha256Hex(data) == a.SHA256 {
		storeBytes(data, filepath.Ext(a.Path)) // repopulate; the data is good either way
		return data, nil
	}
	return nil, fmt.Errorf("%s is no longer available: the cached snapshot is gone and the file has changed or moved", displayPath(a.Path))
}

// resolveAttachments snapshots every file the user's text names.
func resolveAttachments(text string) ([]attachment, error) {
	var atts []attachment
	for _, t := range parseAttachTokens(text) {
		a, err := snapshotFile(t.path)
		if err != nil {
			return nil, fmt.Errorf("attach %s: %w", t.path, err)
		}
		atts = append(atts, a)
	}
	return atts, nil
}

// ---------------------------------------------------------------------------
// wire format

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
	File     *filePart `json:"file,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type filePart struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// inlineText renders a text file as a fenced block, with a fence longer than
// any backtick run inside so the content cannot close it early.
func inlineText(name string, data []byte) string {
	longest, run := 0, 0
	for _, b := range data {
		if b == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	lang := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	return fmt.Sprintf("\n[file: %s]\n%s%s\n%s\n%s\n", name, fence, lang, strings.TrimRight(string(data), "\n"), fence)
}

// expandUserContent turns a user message and its snapshots into what goes on
// the wire. Text files are inlined, so a message with only text attachments
// stays a plain string that any endpoint accepts; images and PDFs make it a
// list of content parts, interleaved where their tokens were.
func expandUserContent(text string, atts []attachment) (string, []contentPart, error) {
	if len(atts) == 0 { // no snapshots: tokens are literal text, never files
		return unescapeAttach(text), nil, nil
	}
	toks := parseAttachTokens(text)
	if len(toks) != len(atts) {
		return "", nil, fmt.Errorf("message names %d attachments but has %d snapshots", len(toks), len(atts))
	}
	var parts []contentPart
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, contentPart{Type: "text", Text: cur.String()})
			cur.Reset()
		}
	}
	last := 0
	for i, t := range toks {
		cur.WriteString(unescapeAttach(text[last:t.start]))
		last = t.end
		a := atts[i]
		data, err := attachmentData(a)
		if err != nil {
			return "", nil, err
		}
		name := filepath.Base(a.Path)
		if inCache(a.Path) { // pasted: the file name is only a hash
			name = "pasted image" + filepath.Ext(a.Path)
		}
		switch a.kind() {
		case "image":
			fmt.Fprintf(&cur, "[image: %s]", name)
			flush()
			parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: dataURL(a.Type, data)}})
		case "pdf":
			fmt.Fprintf(&cur, "[file: %s]", name)
			flush()
			parts = append(parts, contentPart{Type: "file", File: &filePart{Filename: name, FileData: dataURL(a.Type, data)}})
		default:
			cur.WriteString(inlineText(name, data))
		}
	}
	cur.WriteString(unescapeAttach(text[last:]))
	if len(parts) == 0 {
		return cur.String(), nil, nil
	}
	flush()
	return "", parts, nil
}

// expandAttachments resolves the snapshots of every user message in place.
func expandAttachments(msgs []Message) error {
	for i := range msgs {
		if msgs[i].Role != "user" {
			continue
		}
		content, parts, err := expandUserContent(msgs[i].Content, msgs[i].attach)
		if err != nil {
			return fmt.Errorf("attachment: %w", err)
		}
		msgs[i].Content, msgs[i].parts = content, parts
	}
	return nil
}

// ---------------------------------------------------------------------------
// restoring attachments of a loaded conversation

// restoreAttachments re-establishes the attachments of loaded user messages.
// A token is restored without asking when the conversation pins it to a
// snapshot whose bytes are still available, or when it names a file inside
// llm-chat's own cache. Any other path was chosen by whoever wrote the file,
// possibly a stranger, so its contents are only read with the user's
// confirmation. Declined or unavailable tokens are escaped: still visible,
// never resolved. It returns a summary fragment, or "" when nothing changed.
func (s *Session) restoreAttachments(recorded [][]attachment) string {
	type ref struct{ msg, tok int }
	var ask []ref
	var askPaths []string
	seen := map[string]bool{}
	resolved := make([][]*attachment, len(s.history))
	toks := make([][]attachToken, len(s.history))
	restored := 0
	for i, h := range s.history {
		if h.Role != "user" {
			continue
		}
		toks[i] = parseAttachTokens(h.Content)
		resolved[i] = make([]*attachment, len(toks[i]))
		var rec []attachment
		if i < len(recorded) && len(recorded[i]) == len(toks[i]) {
			rec = recorded[i]
		}
		for j, t := range toks[i] {
			if rec != nil {
				if _, err := attachmentData(rec[j]); err == nil {
					a := rec[j]
					resolved[i][j] = &a
					restored++
					continue
				}
			}
			if p, err := expandPath(t.path); err == nil && inCache(p) {
				if a, err := snapshotFile(p); err == nil {
					resolved[i][j] = &a
					restored++
					continue
				}
			}
			ask = append(ask, ref{i, j})
			if p, err := expandPath(t.path); err == nil && !seen[p] {
				seen[p] = true
				askPaths = append(askPaths, p)
			}
		}
	}
	if len(ask) > 0 && s.confirmAttach != nil && s.confirmAttach(askPaths) {
		for _, r := range ask {
			if a, err := snapshotFile(toks[r.msg][r.tok].path); err == nil {
				resolved[r.msg][r.tok] = &a
				restored++
			}
		}
	}
	dropped := 0
	for i := range s.history {
		var atts []attachment
		var unresolved []attachToken
		for j, a := range resolved[i] {
			if a == nil {
				unresolved = append(unresolved, toks[i][j])
				dropped++
			} else {
				atts = append(atts, *a)
			}
		}
		if len(unresolved) > 0 {
			s.history[i].Content = escapeTokens(s.history[i].Content, unresolved)
		}
		s.history[i].Attachments = atts
	}
	var parts []string
	if restored > 0 {
		parts = append(parts, fmt.Sprintf("%d attachments", restored))
	}
	if dropped > 0 {
		fmt.Fprintf(os.Stderr, "%swarning:%s %d attachment(s) not restored; their tokens were escaped and will be sent as text\n", cRed, cReset, dropped)
		parts = append(parts, fmt.Sprintf("%d attachments not restored", dropped))
	}
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// pasted and dropped files

// draggedImagePath recognizes text a terminal inserts when an image file is
// dragged onto it or a copied file is pasted: an absolute path, possibly
// quoted, shell-escaped, or a file:// URL. It returns the path, or "".
func draggedImagePath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	} else if strings.HasPrefix(s, "file://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		s = u.Path
	} else if filepath.Separator == '/' { // backslash is a separator on Windows
		var b strings.Builder // undo shell escaping such as "My\ Shot.png"
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		s = b.String()
	}
	if strings.ContainsRune(s, '\n') || !(filepath.IsAbs(s) || strings.HasPrefix(s, "~/")) {
		return ""
	}
	p, err := expandPath(s)
	if err != nil {
		return ""
	}
	if _, ok := imageExts[strings.ToLower(filepath.Ext(p))]; !ok {
		return ""
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// pasteClipboardImage saves the clipboard's image into the cache and returns
// a token for it.
func pasteClipboardImage() (string, error) {
	data, err := readClipboardImage()
	if err != nil {
		return "", err
	}
	ext, ok := imageTypes[http.DetectContentType(data)]
	if !ok {
		return "", errors.New("the clipboard does not hold a PNG, JPEG, GIF, or WebP image")
	}
	path, _, err := storeBytes(data, ext)
	if err != nil {
		return "", err
	}
	return formatAttachToken(path), nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
