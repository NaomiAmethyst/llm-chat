// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

// Tests for <attach:PATH> attachments: token syntax, snapshots and the
// cache, the wire format, save/load round trips, and the rules that keep a
// model or a loaded file from making llm-chat upload files on its own.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func withCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LLM_CHAT_CACHE_DIR", dir)
	return dir
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// wireBody sends the session's history to a server and returns the decoded
// request the server received.
func wireBody(t *testing.T, s *Session) map[string]any {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	s.client, s.baseURL, s.model = srv.Client(), srv.URL, "m"
	captureStdout(t, func() {
		if _, _, err := s.send(context.Background()); err != nil {
			t.Errorf("send: %v", err)
		}
	})
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v\n%s", err, body)
	}
	return req
}

func lastContent(t *testing.T, req map[string]any) any {
	t.Helper()
	msgs := req["messages"].([]any)
	return msgs[len(msgs)-1].(map[string]any)["content"]
}

// ---------------------------------------------------------------------------
// token syntax

func TestParseAttachTokens(t *testing.T) {
	cases := map[string][]string{
		"see <attach:/a/b.png> here":           {"/a/b.png"},
		`two <attach:"/a b>c.png"> <attach:x>`: {"/a b>c.png", "x"},
		"escaped <<attach:/a.png> stays":       nil,
		"unterminated <attach:/a.png":          nil,
		"broken <attach:/a\n.png>":             nil,
		"empty <attach:> token":                nil,
		`bad quote <attach:"/a.png> end`:       nil,
	}
	for text, want := range cases {
		var got []string
		for _, tok := range parseAttachTokens(text) {
			got = append(got, tok.path)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("parseAttachTokens(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestFormatAttachTokenRoundTrips(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"plain.png", "with space.png", "odd>name.png"} {
		p := filepath.Join(dir, name)
		toks := parseAttachTokens("x " + formatAttachToken(p) + " y")
		if len(toks) != 1 {
			t.Fatalf("%q: got %d tokens", name, len(toks))
		}
		got, err := expandPath(toks[0].path)
		if err != nil || got != p {
			t.Errorf("%q round-tripped to %q (%v)", p, got, err)
		}
	}
}

// ---------------------------------------------------------------------------
// snapshots and the cache

func TestSnapshotFileClassifies(t *testing.T) {
	withCache(t)
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"shot.png":  {pngBytes(t), "image/png"},
		"doc.pdf":   {[]byte("%PDF-1.4\n%fake\n"), "application/pdf"},
		"notes.md":  {[]byte("# notes\n"), "text/plain"},
		"empty.txt": {nil, "text/plain"},
	} {
		a, err := snapshotFile(writeTemp(t, name, tc.data))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if a.Type != tc.want {
			t.Errorf("%s: type %q, want %q", name, a.Type, tc.want)
		}
	}
	if _, err := snapshotFile(writeTemp(t, "blob.bin", []byte{0, 1, 2, 0xff, 0})); err == nil {
		t.Error("binary file should be refused")
	}
	if _, err := snapshotFile(t.TempDir()); err == nil {
		t.Error("directory should be refused")
	}
}

func TestSnapshotCacheIsPrivate(t *testing.T) {
	cache := withCache(t)
	a, err := snapshotFile(writeTemp(t, "n.txt", []byte("hi")))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(cache, "files", a.SHA256+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("snapshot readable by others: %v", fi.Mode().Perm())
	}
}

func TestPushUserMissingFileNamesIt(t *testing.T) {
	withCache(t)
	s := &Session{}
	missing := filepath.Join(t.TempDir(), "gone.png")
	err := s.pushUser("look <attach:" + missing + ">")
	if err == nil || !strings.Contains(err.Error(), "gone.png") {
		t.Errorf("err = %v", err)
	}
	if len(s.history) != 0 {
		t.Error("a message with a bad attachment must not enter history")
	}
}

// ---------------------------------------------------------------------------
// wire format

func TestWireImageIsContentPart(t *testing.T) {
	withCache(t)
	p := writeTemp(t, "shot.png", pngBytes(t))
	s := &Session{}
	if err := s.pushUser("what is this? " + formatAttachToken(p) + " thanks"); err != nil {
		t.Fatal(err)
	}
	parts, ok := lastContent(t, wireBody(t, s)).([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("content = %#v, want text/image/text parts", lastContent(t, wireBody(t, s)))
	}
	first := parts[0].(map[string]any)
	if !strings.Contains(first["text"].(string), "what is this? [image: shot.png]") {
		t.Errorf("text part = %q", first["text"])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" || !strings.HasPrefix(img["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,") {
		t.Errorf("image part = %#v", img)
	}
	if parts[2].(map[string]any)["text"] != " thanks" {
		t.Errorf("trailing text = %#v", parts[2])
	}
}

func TestWirePDFIsFilePart(t *testing.T) {
	withCache(t)
	p := writeTemp(t, "doc.pdf", []byte("%PDF-1.4\n%fake\n"))
	s := &Session{}
	if err := s.pushUser("summarize " + formatAttachToken(p)); err != nil {
		t.Fatal(err)
	}
	parts := lastContent(t, wireBody(t, s)).([]any)
	file := parts[1].(map[string]any)["file"].(map[string]any)
	if file["filename"] != "doc.pdf" || !strings.HasPrefix(file["file_data"].(string), "data:application/pdf;base64,") {
		t.Errorf("file part = %#v", file)
	}
}

// Text-only attachments keep the content a plain string, which every
// endpoint accepts.
func TestWireTextIsInlined(t *testing.T) {
	withCache(t)
	p := writeTemp(t, "main.go", []byte("package main\n"))
	s := &Session{}
	if err := s.pushUser("review " + formatAttachToken(p)); err != nil {
		t.Fatal(err)
	}
	content, ok := lastContent(t, wireBody(t, s)).(string)
	if !ok {
		t.Fatalf("content is not a string: %#v", lastContent(t, wireBody(t, s)))
	}
	if !strings.Contains(content, "[file: main.go]\n```go\npackage main\n```") {
		t.Errorf("inlined content = %q", content)
	}
}

func TestInlineTextFenceOutlastsContent(t *testing.T) {
	got := inlineText("x.md", []byte("````\nnested\n````"))
	if !strings.Contains(got, "`````md\n") {
		t.Errorf("fence not longer than content's backtick run:\n%s", got)
	}
}

func TestWireEscapeIsLiteral(t *testing.T) {
	withCache(t)
	s := &Session{}
	if err := s.pushUser("write <<attach:/x.png> literally"); err != nil {
		t.Fatal(err)
	}
	if len(s.history[0].Attachments) != 0 {
		t.Fatal("an escaped token must not attach anything")
	}
	if content := lastContent(t, wireBody(t, s)).(string); !strings.HasSuffix(content, "write <attach:/x.png> literally") {
		t.Errorf("content = %q", content)
	}
}

// Editing the file after sending must not change what the message said.
func TestSnapshotIsTakenAtSend(t *testing.T) {
	withCache(t)
	p := writeTemp(t, "n.txt", []byte("version one"))
	s := &Session{}
	if err := s.pushUser(formatAttachToken(p)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("version two"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := lastContent(t, wireBody(t, s)).(string)
	if !strings.Contains(content, "version one") || strings.Contains(content, "version two") {
		t.Errorf("history changed with the file: %q", content)
	}
}

func TestSnapshotSurvivesFileRemoval(t *testing.T) {
	withCache(t)
	p := writeTemp(t, "n.txt", []byte("kept"))
	s := &Session{}
	if err := s.pushUser(formatAttachToken(p)); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	if content := lastContent(t, wireBody(t, s)).(string); !strings.Contains(content, "kept") {
		t.Errorf("content = %q", content)
	}
}

func TestSnapshotUnavailableErrors(t *testing.T) {
	cache := withCache(t)
	p := writeTemp(t, "n.txt", []byte("one"))
	s := &Session{}
	if err := s.pushUser(formatAttachToken(p)); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(cache)
	os.WriteFile(p, []byte("two"), 0o600)
	s.client = http.DefaultClient
	s.baseURL = "http://127.0.0.1:1" // never reached: the attachment fails first
	_, _, err := s.send(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Errorf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// what may never be uploaded

// A model mentioning a token must not make llm-chat read the file.
func TestAssistantTokensAreNeverExpanded(t *testing.T) {
	withCache(t)
	secret := writeTemp(t, "id_ed25519", []byte("PRIVATE KEY MATERIAL"))
	s := &Session{}
	s.push("assistant", "send me "+formatAttachToken(secret))
	if err := s.pushUser("ok"); err != nil {
		t.Fatal(err)
	}
	req := wireBody(t, s)
	raw, _ := json.Marshal(req)
	if strings.Contains(string(raw), "PRIVATE KEY MATERIAL") {
		t.Fatal("file named by the assistant was uploaded")
	}
}

// Text with tokens but no recorded snapshots is always literal.
func TestUnsnapshottedUserTokensAreLiteral(t *testing.T) {
	withCache(t)
	secret := writeTemp(t, "secret.txt", []byte("PRIVATE"))
	s := &Session{}
	s.push("user", "look "+formatAttachToken(secret))
	raw, _ := json.Marshal(wireBody(t, s))
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("token without a snapshot was resolved at send time")
	}
}

// ---------------------------------------------------------------------------
// save/load round trips

func TestJSONRoundTripKeepsAttachments(t *testing.T) {
	withCache(t)
	p := writeTemp(t, "shot.png", pngBytes(t))
	a := &Session{}
	if err := a.pushUser("see " + formatAttachToken(p)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chat.json")
	captureStdout(t, func() {
		if err := a.saveJSON(path); err != nil {
			t.Error(err)
		}
	})
	os.Remove(p) // the snapshot in the cache is what matters

	b := &Session{confirmAttach: func([]string) bool { t.Error("pinned snapshot needed confirmation"); return false }}
	summary, err := b.loadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.history) != 1 || len(b.history[0].Attachments) != 1 {
		t.Fatalf("history = %+v", b.history)
	}
	if !strings.Contains(summary, "1 attachments") {
		t.Errorf("summary = %q", summary)
	}
	if parts, ok := lastContent(t, wireBody(t, b)).([]any); !ok || len(parts) != 2 {
		t.Errorf("reloaded message did not carry its image")
	}
}

func transcriptWith(content string) string {
	return "you ❯\n" + content + "\n[sent: 2026-10-31 12:00:00; 10 bytes]\n\n" +
		"◆ m\nanswer\n[recv: 2026-10-31 12:00:01; 1s; 1 in / 1 out; session 1 in / 1 out]\n"
}

// A transcript may come from anyone, so paths outside the cache need consent.
func TestTranscriptAttachmentNeedsConfirmation(t *testing.T) {
	withCache(t)
	secret := writeTemp(t, "secret.txt", []byte("PRIVATE"))
	transcript := transcriptWith("look " + formatAttachToken(secret))

	declined := &Session{} // nil confirmAttach: non-interactive, always declines
	captureStdout(t, func() {
		if _, err := declined.loadFromText(transcript); err != nil {
			t.Error(err)
		}
	})
	if len(declined.history[0].Attachments) != 0 {
		t.Fatal("attachment restored without confirmation")
	}
	if !strings.Contains(declined.history[0].Content, attachEscape) {
		t.Errorf("declined token not escaped: %q", declined.history[0].Content)
	}
	raw, _ := json.Marshal(wireBody(t, declined))
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("declined file was uploaded")
	}

	var asked []string
	accepted := &Session{confirmAttach: func(paths []string) bool { asked = paths; return true }}
	if _, err := accepted.loadFromText(transcript); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != secret {
		t.Errorf("asked about %q, want [%q]", asked, secret)
	}
	if len(accepted.history[0].Attachments) != 1 {
		t.Error("confirmed attachment not restored")
	}
}

// Files llm-chat stored itself (pasted images) restore without asking.
func TestTranscriptCachedAttachmentRestores(t *testing.T) {
	withCache(t)
	path, _, err := storeBytes(pngBytes(t), ".png")
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{confirmAttach: func([]string) bool { t.Error("cached file needed confirmation"); return false }}
	if _, err := s.loadFromText(transcriptWith("pasted " + formatAttachToken(path))); err != nil {
		t.Fatal(err)
	}
	if len(s.history[0].Attachments) != 1 {
		t.Error("cached attachment not restored")
	}
}

// A JSON file whose pinned hash does not match is treated like a bare path.
func TestJSONForgedSnapshotNeedsConfirmation(t *testing.T) {
	withCache(t)
	secret := writeTemp(t, "secret.txt", []byte("PRIVATE"))
	forged := fmt.Sprintf(`{"messages":[{"role":"user","content":%q,"attachments":[{"path":%q,"sha256":"%s","type":"text/plain","size":7}]}]}`,
		"look "+formatAttachToken(secret), secret, strings.Repeat("0", 64))
	s := &Session{}
	captureStdout(t, func() {
		if _, err := s.loadFromText(forged); err != nil {
			t.Error(err)
		}
	})
	if len(s.history[0].Attachments) != 0 {
		t.Fatal("forged snapshot accepted")
	}
}

// ---------------------------------------------------------------------------
// input: dragged files and commands

func TestDraggedImagePath(t *testing.T) {
	p := writeTemp(t, "My Shot.png", pngBytes(t))
	txt := writeTemp(t, "notes.txt", []byte("x"))
	cases := map[string]string{
		"'" + p + "'":  p,
		`"` + p + `"`:  p,
		"  " + p + " ": p,  // some terminals drop paths unescaped
		txt:            "", // only images convert automatically
		"shot.png":     "", // relative paths are typed, not dragged
	}
	if runtime.GOOS != "windows" {
		cases[strings.ReplaceAll(p, " ", `\ `)] = p
		cases["file://"+strings.ReplaceAll(p, " ", "%20")] = p
	}
	for in, want := range cases {
		if got := draggedImagePath(in); got != want {
			t.Errorf("draggedImagePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEditorConvertsDroppedImage(t *testing.T) {
	p := writeTemp(t, "shot.png", pngBytes(t))
	e := &editor{buf: []rune("look " + p), batchStart: 5}
	e.cur = len(e.buf)
	e.convertDrop()
	if want := "look " + formatAttachToken(p) + " "; string(e.buf) != want {
		t.Errorf("buf = %q, want %q", string(e.buf), want)
	}

	// A batch that included edits is not a drop.
	e = &editor{buf: []rune(p), batchStart: 0, batchDirty: true}
	e.cur = len(e.buf)
	e.convertDrop()
	if string(e.buf) != p {
		t.Errorf("dirty batch converted: %q", string(e.buf))
	}
}

func TestAttachCommandPrefills(t *testing.T) {
	withCache(t)
	p := writeTemp(t, "with space.md", []byte("# hi"))
	s := &Session{}
	res := handleCommand(s, `/attach "`+p+`"`)
	if res.prefill != formatAttachToken(p)+" " {
		t.Errorf("prefill = %q", res.prefill)
	}
	res = handleCommand(s, "/attach "+filepath.Join(t.TempDir(), "missing.png"))
	if res.prefill != "" {
		t.Errorf("missing file produced prefill %q", res.prefill)
	}
}

func TestPickImageType(t *testing.T) {
	if got := pickImageType([]string{"TARGETS", "image/jpeg", "image/png"}); got != "image/png" {
		t.Errorf("got %q", got)
	}
	if got := pickImageType([]string{"text/plain", "UTF8_STRING"}); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestImageEstimateCounted(t *testing.T) {
	withCache(t)
	s := &Session{}
	before := approxMessages(s.messages())
	if err := s.pushUser(formatAttachToken(writeTemp(t, "s.png", pngBytes(t)))); err != nil {
		t.Fatal(err)
	}
	if got := approxMessages(s.messages()) - before; got < imageTokenEstimate {
		t.Errorf("image added only ~%d tokens to the estimate", got)
	}
}
