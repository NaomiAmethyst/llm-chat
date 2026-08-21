// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

// Regression tests for reported issues: endpoint restoration on load,
// terminal-escape sanitizing, SSE event framing, and reasoning isolation.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-out
}

func rawSSE(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}
}

func clearKeyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LLM_CHAT_API_KEY", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "OPENCLAW_GATEWAY_TOKEN"} {
		t.Setenv(k, "")
	}
}

// ---------------------------------------------------------------------------
// endpoint restoration

// A conversation saved against one endpoint must not be replayed to another.
func TestLoadRestoresEndpointAndCredential(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	t.Setenv("OPENCLAW_GATEWAY_TOKEN", "gw-token")

	s := &Session{baseURL: "https://openrouter.ai/api/v1"}
	s.resolveKey()
	if s.apiKey != "or-key" {
		t.Fatalf("initial key = %q", s.apiKey)
	}
	summary, err := s.loadFromText(
		`{"endpoint":"http://127.0.0.1:18789/v1","model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if s.baseURL != "http://127.0.0.1:18789/v1" {
		t.Errorf("endpoint not restored: %q", s.baseURL)
	}
	if s.apiKey != "gw-token" {
		t.Errorf("credential not re-resolved for the restored endpoint: %q", s.apiKey)
	}
	if !strings.Contains(summary, "endpoint http://127.0.0.1:18789/v1") {
		t.Errorf("endpoint switch not reported: %q", summary)
	}
}

func TestLoadRespectsExplicitURLAndKey(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	s := &Session{baseURL: "https://mine/v1", apiKey: "mine", urlLocked: true, keyLocked: true}
	if _, err := s.loadFromText(
		`{"endpoint":"http://elsewhere/v1","messages":[{"role":"user","content":"hi"}]}`); err != nil {
		t.Fatal(err)
	}
	if s.baseURL != "https://mine/v1" || s.apiKey != "mine" {
		t.Errorf("explicit -url/-key overridden: url=%q key=%q", s.baseURL, s.apiKey)
	}
}

// The gateway token must never follow a load to some other endpoint.
func TestGatewayTokenNotSentElsewhere(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("OPENCLAW_GATEWAY_TOKEN", "gw-token")
	s := &Session{baseURL: "http://127.0.0.1:18789/v1"}
	s.resolveKey()
	if s.apiKey != "gw-token" {
		t.Fatalf("gateway key = %q", s.apiKey)
	}
	if _, err := s.loadFromText(
		`{"endpoint":"https://elsewhere.example/v1","messages":[{"role":"user","content":"hi"}]}`); err != nil {
		t.Fatal(err)
	}
	if s.apiKey != "" {
		t.Errorf("gateway token leaked to %s: %q", s.baseURL, s.apiKey)
	}
}

// A pasted transcript records no endpoint, so it can never redirect one.
func TestTranscriptLoadKeepsEndpoint(t *testing.T) {
	clearKeyEnv(t)
	s := &Session{baseURL: "https://mine/v1"}
	if _, err := s.loadFromText(sampleTranscript); err != nil {
		t.Fatal(err)
	}
	if s.baseURL != "https://mine/v1" {
		t.Errorf("endpoint changed by a transcript load: %q", s.baseURL)
	}
}

// ---------------------------------------------------------------------------
// escape sanitizing

func TestSanitize(t *testing.T) {
	in := "safe\x1b[31m red \x1b]0;title\x07 \x00\x7f\ttab\nline\r\n"
	got := sanitize(in)
	if strings.ContainsAny(got, "\x1b\x07\x00\x7f\r") {
		t.Errorf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "\ttab\nline") {
		t.Errorf("tabs/newlines not preserved: %q", got)
	}
	if sanitize("plain text") != "plain text" {
		t.Error("clean text should pass through unchanged")
	}
}

// -color=false must not turn model output into terminal control.
func TestPlainOutputStripsEscapes(t *testing.T) {
	m := &mdWriter{plain: true}
	out := captureStdout(t, func() { m.WriteString("hi \x1b]0;pwned\x07\x1b[2J there\n") })
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("escapes reached stdout in plain mode: %q", out)
	}
	if !strings.Contains(out, "hi ") || !strings.Contains(out, " there") {
		t.Errorf("text lost: %q", out)
	}
}

func TestHTTPErrorSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, "{\"error\":{\"message\":\"bad \x1b]0;pwned\x07 request\"}}")
	}))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m"}
	s.push("user", "hi")
	_, _, err := s.send(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.ContainsAny(err.Error(), "\x1b\x07") {
		t.Errorf("escapes in error text: %q", err.Error())
	}
}

func TestFetchModelsSanitizesIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// \u001b is a JSON-escaped ESC: valid JSON carrying a control character
		fmt.Fprint(w, `{"data":[{"id":"ev\u001b[2Jil/model"}]}`)
	}))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL}
	ids, err := s.fetchModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || strings.ContainsRune(ids[0], 0x1b) {
		t.Errorf("model id not sanitized: %q", ids)
	}
}

// ---------------------------------------------------------------------------
// SSE framing

// Per the SSE spec a single event's data may span multiple data: lines.
func TestSendStreamMultiLineData(t *testing.T) {
	srv := httptest.NewServer(rawSSE(
		"data: {\"choices\":[{\"delta\":\n" +
			"data: {\"content\":\"joined\"}}]}\n" +
			"\n" +
			"data: [DONE]\n\n"))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true}
	s.push("user", "hi")
	content, _, err := s.send(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if content != "joined" {
		t.Errorf("content = %q, want %q", content, "joined")
	}
}

// A stream that ends without a blank line must still deliver its last event.
func TestSendStreamNoTrailingBlankLine(t *testing.T) {
	srv := httptest.NewServer(rawSSE(`data: {"choices":[{"delta":{"content":"last"}}]}`))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true}
	s.push("user", "hi")
	content, _, err := s.send(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if content != "last" {
		t.Errorf("content = %q", content)
	}
}

// Dropping an unparsable event would silently lose answer text.
func TestSendStreamMalformedEventErrors(t *testing.T) {
	srv := httptest.NewServer(rawSSE(
		"data: {\"choices\":[{\"delta\":{\"content\":\"kept\"}}]}\n\n" +
			"data: {not json\n\n" +
			"data: [DONE]\n\n"))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true}
	s.push("user", "hi")
	content, _, err := s.send(context.Background())
	if err == nil {
		t.Fatal("expected an error for a malformed event")
	}
	if !strings.Contains(err.Error(), "malformed streaming event") {
		t.Errorf("err = %v", err)
	}
	if content != "kept" {
		t.Errorf("partial content lost: %q", content)
	}
}

// Comments and unknown SSE fields carry no payload and must be ignored.
func TestSendStreamIgnoresCommentsAndFields(t *testing.T) {
	srv := httptest.NewServer(rawSSE(
		": OPENROUTER PROCESSING\n\n" +
			"event: message\nid: 7\nretry: 100\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true}
	s.push("user", "hi")
	content, _, err := s.send(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if content != "ok" {
		t.Errorf("content = %q", content)
	}
}

// ---------------------------------------------------------------------------
// reasoning isolation

// Reasoning is display-only: marked on screen, absent from history.
func TestReasoningMarkedAndExcluded(t *testing.T) {
	srv := httptest.NewServer(rawSSE(
		"data: {\"choices\":[{\"delta\":{\"reasoning\":\"first thought\\nsecond\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"the answer\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true, interactive: true}
	s.push("user", "hi")
	var content string
	out := captureStdout(t, func() {
		var err error
		content, _, err = s.send(context.Background())
		if err != nil {
			t.Error(err)
		}
	})
	if content != "the answer" {
		t.Errorf("reasoning leaked into history: %q", content)
	}
	for _, line := range []string{reasoningMark + " first thought", reasoningMark + " second"} {
		if !strings.Contains(stripANSI(out), line) {
			t.Errorf("reasoning line %q not marked in %q", line, stripANSI(out))
		}
	}
}

// Reasoning containing terminal escapes must not reach the terminal raw.
func TestReasoningSanitized(t *testing.T) {
	rw := newReasoningWriter()
	out := captureStdout(t, func() {
		rw.write("thinking \x1b]0;pwned\x07 on")
		rw.end()
	})
	if strings.Contains(out, "\x1b]0;") || strings.ContainsRune(out, 0x07) {
		t.Errorf("escapes in reasoning output: %q", out)
	}
}

// A copied transcript must not turn printed reasoning into assistant text.
func TestParseTranscriptSkipsReasoning(t *testing.T) {
	transcript := "you ❯\nask\n[sent: 2026-08-13 22:37:05; 3 bytes]\n\n" +
		"◆ mock/alpha\n" +
		reasoningMark + " let me think about this\n" +
		reasoningMark + " still thinking\n\n" +
		"the actual answer\n" +
		"[recv: 2026-08-13 22:37:12; 1s; 1 in / 2 out; session 1 in / 2 out]\n"
	c, err := parseTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 2 {
		t.Fatalf("got %d messages: %+v", len(c.Messages), c.Messages)
	}
	if got := c.Messages[1].Content; got != "the actual answer" {
		t.Errorf("assistant content = %q, want %q", got, "the actual answer")
	}
}
