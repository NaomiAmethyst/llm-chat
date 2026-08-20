// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func render(m *mdWriter, line string) string {
	em := &ansiEmitter{width: 1 << 30}
	m.renderLine(line, em)
	return em.b.String()
}

// ---------------------------------------------------------------------------
// small helpers

func TestWithCommas(t *testing.T) {
	for in, want := range map[int]string{0: "0", 5: "5", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := withCommas(in); got != want {
			t.Errorf("withCommas(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestApproxTokens(t *testing.T) {
	for in, want := range map[string]int{"": 0, "abc": 1, "abcd": 1, "abcde": 2} {
		if got := approxTokens(in); got != want {
			t.Errorf("approxTokens(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestDurStr(t *testing.T) {
	if got := durStr(500 * time.Millisecond); got != "500ms" {
		t.Errorf("durStr = %q", got)
	}
	if got := durStr(6840 * time.Millisecond); got != "6.8s" {
		t.Errorf("durStr = %q", got)
	}
}

func TestHeaderLevel(t *testing.T) {
	for in, want := range map[string]int{
		"# Title": 1, "## T": 2, "   ### T": 6, "#": 1,
		"#x": 0, "####### x": 0, "plain": 0, "": 0,
	} {
		if got := headerLevel(in); got != want {
			t.Errorf("headerLevel(%q) = %d, want %d", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// session: placeholders, static system prompt, wire timestamps

func TestExpandAndStaticSystem(t *testing.T) {
	s := &Session{model: "m1", baseURL: "http://x/v1", system: "model={model} url={endpoint}"}
	s.refreshSystem()
	if s.sysExpanded != "model=m1 url=http://x/v1" {
		t.Fatalf("sysExpanded = %q", s.sysExpanded)
	}
	// Changing the model must NOT change the sent prompt until refreshSystem.
	s.model = "m2"
	if got := s.messages(); got[0].Content != "model=m1 url=http://x/v1" {
		t.Errorf("system re-expanded without refresh: %q", got[0].Content)
	}
	s.refreshSystem()
	if got := s.messages(); got[0].Content != "model=m2 url=http://x/v1" {
		t.Errorf("refreshSystem did not update: %q", got[0].Content)
	}
}

func TestMessagesTimestampPrefix(t *testing.T) {
	s := &Session{}
	s.push("user", "hi there")
	msgs := s.messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if ok, _ := regexp.MatchString(`^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\] hi there$`, msgs[0].Content); !ok {
		t.Errorf("missing timestamp prefix: %q", msgs[0].Content)
	}
}

// ---------------------------------------------------------------------------
// non-interactive input

func TestReadDotMessage(t *testing.T) {
	rd := bufio.NewReader(strings.NewReader("first\nsecond line\n.\ntrailing"))
	msg, eof := readDotMessage(rd)
	if msg != "first\nsecond line\n" || eof {
		t.Errorf("got %q eof=%v", msg, eof)
	}
	msg, eof = readDotMessage(rd)
	if msg != "trailing" || !eof {
		t.Errorf("got %q eof=%v", msg, eof)
	}
}

// ---------------------------------------------------------------------------
// transcript parsing and JSON round-trip

const sampleTranscript = `llm-chat · http://127.0.0.1:8931/v1 · mock/alpha
/help for commands · Enter for newline

system ❯
Be terse.
Very terse.

you ❯
what is the answer
[sent: 2026-08-13 22:37:05; 18 bytes]

◆ mock/alpha
It is **42**.
[recv: 2026-08-13 22:37:12; 6.8s; 10 in / 20 out; session 10 in / 20 out]

────────────────────────────────
you ❯
thanks
[sent: 2026-08-13 22:38:00; 6 bytes]
`

func TestParseTranscript(t *testing.T) {
	c, err := parseTranscript(sampleTranscript)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "mock/alpha" {
		t.Errorf("model = %q", c.Model)
	}
	if c.System == nil || *c.System != "Be terse.\nVery terse." {
		t.Errorf("system = %v", c.System)
	}
	if len(c.Messages) != 3 {
		t.Fatalf("got %d messages: %+v", len(c.Messages), c.Messages)
	}
	want := []savedMsg{
		{Role: "user", Content: "what is the answer"},
		{Role: "assistant", Content: "It is **42**."},
		{Role: "user", Content: "thanks"},
	}
	for i, w := range want {
		if c.Messages[i].Role != w.Role || c.Messages[i].Content != w.Content {
			t.Errorf("message %d = %+v, want %+v", i, c.Messages[i], w)
		}
	}
	ts, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-08-13 22:37:05", time.Local)
	if !c.Messages[0].At.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", c.Messages[0].At, ts)
	}
}

func TestParseTranscriptRejectsGarbage(t *testing.T) {
	if _, err := parseTranscript("no markers here\njust text\n"); err == nil {
		t.Error("expected an error for a marker-less transcript")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	a := &Session{model: "m", baseURL: "http://x/v1", system: "sys"}
	a.refreshSystem()
	a.push("user", "q1")
	a.push("assistant", "a1\nwith **markdown**")
	path := t.TempDir() + "/conv.json"
	if err := a.saveJSON(path); err != nil {
		t.Fatal(err)
	}

	b := &Session{model: "other"}
	if _, err := b.loadFromFile(path); err != nil {
		t.Fatal(err)
	}
	if b.model != "m" || b.system != "sys" {
		t.Errorf("model=%q system=%q", b.model, b.system)
	}
	if len(b.history) != 2 {
		t.Fatalf("got %d messages", len(b.history))
	}
	for i := range a.history {
		if b.history[i].Role != a.history[i].Role ||
			b.history[i].Content != a.history[i].Content ||
			!b.history[i].At.Equal(a.history[i].At) {
			t.Errorf("message %d: %+v != %+v", i, b.history[i], a.history[i])
		}
	}
	if b.sysExpanded != "sys" {
		t.Errorf("system not re-expanded after load: %q", b.sysExpanded)
	}
}

func TestLoadFromTextSniffsJSON(t *testing.T) {
	s := &Session{}
	if _, err := s.loadFromText(`{"messages":[{"role":"user","content":"hi"}]}`); err != nil {
		t.Fatal(err)
	}
	if len(s.history) != 1 || s.history[0].Content != "hi" {
		t.Errorf("history = %+v", s.history)
	}
	if s.history[0].At.IsZero() {
		t.Error("zero timestamp should be replaced")
	}
	if _, err := (&Session{}).loadFromText("   "); err == nil {
		t.Error("expected error for empty input")
	}
}

// ---------------------------------------------------------------------------
// markdown rendering

// Highlighting must never change the visible characters.
func TestMarkdownPreservesCharacters(t *testing.T) {
	lines := []string{
		"# A header",
		"Some **bold** and *italic* and `code` here.",
		"- a list item",
		"> a quote",
		"3. numbered",
		"snake_case_stays and __under__",
		"```go",
		"func main() { s := \"hi\" } // comment",
		"x = 42",
		"```",
		"unbalanced **bold and `code",
		"",
	}
	m := &mdWriter{}
	for _, line := range lines {
		if got := stripANSI(render(m, line)); got != line {
			t.Errorf("characters changed:\n in: %q\nout: %q", line, got)
		}
	}
}

func TestMarkdownInlineStyles(t *testing.T) {
	m := &mdWriter{}
	for line, want := range map[string]string{
		"x **b** y":  mGray + "**" + cReset + mBold + "b",
		"x *i* y":    mGray + "*" + cReset + mItal + "i",
		"x __u__ y":  mGray + "__" + cReset + mUndr + "u",
		"x `c` y":    mGray + "`" + cReset + mCode + "c",
		"- item":     mGray + "- " + cReset,
		"# T":        mGray + "#" + cReset + mHead + " T",
		"> quoted":   mGray + "> " + cReset,
		"12. listed": mGray + "12. " + cReset,
	} {
		if got := render(m, line); !strings.Contains(got, want) {
			t.Errorf("render(%q) = %q, missing %q", line, got, want)
		}
	}
}

func TestMarkdownCodeFence(t *testing.T) {
	m := &mdWriter{}
	if got := render(m, "```go"); !strings.HasPrefix(got, mGray) {
		t.Errorf("fence line not gray: %q", got)
	}
	if !m.inFence {
		t.Fatal("inFence not toggled")
	}
	code := render(m, `if x := "s" { // c`)
	for _, want := range []string{mKw + "if", mStr + `"s"`, mGray + "// c"} {
		if !strings.Contains(code, want) {
			t.Errorf("code render %q missing %q", code, want)
		}
	}
	num := render(m, "x = 42")
	if !strings.Contains(num, mNum+"42") {
		t.Errorf("number not highlighted: %q", num)
	}
	render(m, "```")
	if m.inFence {
		t.Error("inFence not closed")
	}
}

func TestSnakeCaseNotItalicized(t *testing.T) {
	m := &mdWriter{}
	got := render(m, "use snake_case_name here")
	if strings.Contains(got, mItal) {
		t.Errorf("mid-word underscores italicized: %q", got)
	}
}

// ---------------------------------------------------------------------------
// editor buffer operations (no terminal required)

func TestEditorInsertDelete(t *testing.T) {
	e := &editor{buf: []rune("hello"), cur: 4}
	e.deleteRange(1, 3) // "hlo", cursor was after the range
	if string(e.buf) != "hlo" || e.cur != 2 {
		t.Errorf("buf=%q cur=%d", string(e.buf), e.cur)
	}
	e.cur = 1
	e.insert('X')
	if string(e.buf) != "hXlo" || e.cur != 2 {
		t.Errorf("buf=%q cur=%d", string(e.buf), e.cur)
	}
}

func TestEditorLineNavAndKill(t *testing.T) {
	e := &editor{buf: []rune("one\ntwo three"), cur: 13}
	if e.lineStart() != 4 || e.lineEnd() != 13 {
		t.Errorf("lineStart=%d lineEnd=%d", e.lineStart(), e.lineEnd())
	}
	e.deleteWordBack()
	if string(e.buf) != "one\ntwo " || e.cur != 8 {
		t.Errorf("buf=%q cur=%d", string(e.buf), e.cur)
	}
}

func TestEditorLayoutWrapping(t *testing.T) {
	e := &editor{buf: []rune("abcdef\ngh")}
	p := e.layout(4)
	for i, want := range []vpos{{0, 0}, {0, 1}, {0, 2}, {0, 3}, {1, 0}, {1, 1}} {
		if p[i] != want {
			t.Errorf("p[%d] = %v, want %v", i, p[i], want)
		}
	}
	if p[7] != (vpos{2, 0}) { // 'g' starts the line after the newline
		t.Errorf("p[7] = %v", p[7])
	}
	if p[len(e.buf)] != (vpos{2, 2}) {
		t.Errorf("end = %v", p[len(e.buf)])
	}
}

func TestEditorMoveVertical(t *testing.T) {
	e := &editor{buf: []rune("abc\ndef"), cur: 7} // end of "def"
	e.moveVertical(-1)
	if e.cur != 3 { // closest column on the first row is after "abc"
		t.Errorf("cur = %d, want 3", e.cur)
	}
	e.moveVertical(1)
	if e.cur != 7 {
		t.Errorf("cur = %d, want 7", e.cur)
	}
}

// ---------------------------------------------------------------------------
// API: SSE streaming, usage, errors

func sseHandler(lines []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprintf(w, "%s\n\n", l)
		}
	}
}

func TestSendStreaming(t *testing.T) {
	srv := httptest.NewServer(sseHandler([]string{
		": KEEPALIVE",
		`data: {"choices":[{"delta":{"content":"Hello "}}]}`,
		`data: {"choices":[{"delta":{"content":"world"}}]}`,
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`,
		"data: [DONE]",
	}))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true}
	s.push("user", "hi")
	content, usage, err := s.send(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if content != "Hello world" {
		t.Errorf("content = %q", content)
	}
	if usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestSendStreamError(t *testing.T) {
	srv := httptest.NewServer(sseHandler([]string{
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		`data: {"error":{"message":"boom"}}`,
	}))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true}
	s.push("user", "hi")
	content, _, err := s.send(context.Background())
	if err == nil || err.Error() != "boom" {
		t.Errorf("err = %v", err)
	}
	if content != "partial" {
		t.Errorf("partial content = %q", content)
	}
}

func TestSendHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
	}))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true}
	s.push("user", "hi")
	_, _, err := s.send(context.Background())
	if err == nil || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("err = %v", err)
	}
}

func TestSendNonStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"plain answer"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m"}
	s.push("user", "hi")
	content, usage, err := s.send(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if content != "plain answer" || usage == nil || usage.CompletionTokens != 2 {
		t.Errorf("content=%q usage=%+v", content, usage)
	}
}

func TestFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"z-first"},{"id":"a-second"}]}`)
	}))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL}
	ids, err := s.fetchModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// API order must be preserved (the first entry becomes the default model).
	if len(ids) != 2 || ids[0] != "z-first" || ids[1] != "a-second" {
		t.Errorf("ids = %v", ids)
	}
}
