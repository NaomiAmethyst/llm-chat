// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

// llm-chat is a minimal terminal chat client for OpenAI-compatible endpoints
// (OpenRouter, OpenClaw, Ollama, vLLM, llama.cpp, ...). No tools, no TUI,
// and only golang.org/x/{term,sys} as dependencies: a raw-mode multi-line
// editor, streamed responses with ANSI markdown highlighting, token
// accounting, conversation save/load, and full control of the system prompt.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

// ---------------------------------------------------------------------------
// terminal helpers

var (
	cReset = "\x1b[0m"
	cBold  = "\x1b[1m"
	cDim   = "\x1b[2m"
	cCyan  = "\x1b[36m"
	cMag   = "\x1b[35m"
	cRed   = "\x1b[31m"
)

func disableColors() {
	cReset, cBold, cDim, cCyan, cMag, cRed = "", "", "", "", "", ""
}

func isTerminal(fd uintptr) bool {
	return term.IsTerminal(int(fd))
}

// ---------------------------------------------------------------------------
// input

// The interactive prompt is a small raw-mode multi-line editor: Enter inserts
// a newline (but runs a single-line /command directly) and Ctrl+D sends, or
// exits when the buffer is empty; arrow keys, Home/End, Backspace, and Delete
// edit in place; Ctrl+C clears the input; pasted text (newlines included) is
// inserted literally.

const batchWait = 2 * time.Millisecond

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

type vpos struct{ row, col int }

type editor struct {
	in      *bufio.Reader
	prompt  string // marker line drawn above the input; may contain color codes
	buf     []rune
	cur     int // cursor index into buf
	lastRow int // visual row of the cursor, counted from the prompt line
}

// layout maps every cursor position (0..len(buf)) to its visual row/column
// within the input area, mirroring exactly how render draws the buffer (all
// runes treated as width 1). The input starts on the line below the prompt at
// column 0, with no continuation markers, so it can be copied cleanly.
func (e *editor) layout(width int) []vpos {
	p := make([]vpos, len(e.buf)+1)
	row, col := 0, 0
	for i, r := range e.buf {
		p[i] = vpos{row, col}
		if r == '\n' {
			row, col = row+1, 0
			continue
		}
		col++
		if col >= width {
			row, col = row+1, 0
		}
	}
	p[len(e.buf)] = vpos{row, col}
	return p
}

// render redraws the whole edit region and parks the cursor at e.cur.
// Wrapping is done manually so cursor arithmetic stays deterministic.
func (e *editor) render() {
	width := termWidth()
	var b strings.Builder
	b.WriteString("\r")
	if e.lastRow > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", e.lastRow)
	}
	b.WriteString("\x1b[J")
	b.WriteString(e.prompt)
	b.WriteString("\r\n")
	row, col := 0, 0
	for _, r := range e.buf {
		if r == '\n' {
			b.WriteString("\r\n")
			row, col = row+1, 0
			continue
		}
		b.WriteRune(r)
		col++
		if col >= width {
			b.WriteString("\r\n")
			row, col = row+1, 0
		}
	}
	p := e.layout(width)[e.cur]
	if row > p.row {
		fmt.Fprintf(&b, "\x1b[%dA", row-p.row)
	}
	b.WriteString("\r")
	if p.col > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", p.col)
	}
	e.lastRow = p.row + 1 // +1 for the prompt line
	os.Stdout.WriteString(b.String())
}

func (e *editor) insert(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cur+1:], e.buf[e.cur:])
	e.buf[e.cur] = r
	e.cur++
}

func (e *editor) deleteRange(from, to int) { // deletes buf[from:to]
	if from < 0 || to > len(e.buf) || from >= to {
		return
	}
	e.buf = append(e.buf[:from], e.buf[to:]...)
	switch {
	case e.cur >= to:
		e.cur -= to - from
	case e.cur > from:
		e.cur = from
	}
}

func (e *editor) lineStart() int {
	i := e.cur
	for i > 0 && e.buf[i-1] != '\n' {
		i--
	}
	return i
}

func (e *editor) lineEnd() int {
	i := e.cur
	for i < len(e.buf) && e.buf[i] != '\n' {
		i++
	}
	return i
}

func (e *editor) deleteWordBack() {
	i := e.cur
	for i > 0 && (e.buf[i-1] == ' ' || e.buf[i-1] == '\t') {
		i--
	}
	for i > 0 && e.buf[i-1] != ' ' && e.buf[i-1] != '\n' {
		i--
	}
	e.deleteRange(i, e.cur)
}

// finish parks the cursor after the last character, moves to a fresh line,
// and returns the buffer contents.
func (e *editor) finish() string {
	e.cur = len(e.buf)
	e.render()
	os.Stdout.WriteString("\r\n")
	return string(e.buf)
}

// moveVertical moves the cursor one visual row up (-1) or down (+1), to the
// position whose column is closest to the current one.
func (e *editor) moveVertical(dir int) {
	p := e.layout(termWidth())
	cur := p[e.cur]
	target := cur.row + dir
	if target < 0 || target > p[len(e.buf)].row {
		return
	}
	bestIdx, bestDist := -1, int(^uint(0)>>1)
	for i, q := range p {
		if q.row != target {
			continue
		}
		d := q.col - cur.col
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			bestDist, bestIdx = d, i
		}
	}
	if bestIdx >= 0 {
		e.cur = bestIdx
	}
}

func (e *editor) handleEscape() {
	// A bare Esc press sends 0x1b alone; only parse a sequence if more
	// bytes are already on their way.
	if e.in.Buffered() == 0 && !stdinHasData(5*time.Millisecond) {
		return
	}
	b1, err := e.in.ReadByte()
	if err != nil || (b1 != '[' && b1 != 'O') {
		return
	}
	var params []byte
	var final byte
	for {
		c, err := e.in.ReadByte()
		if err != nil {
			return
		}
		if (c >= '0' && c <= '9') || c == ';' {
			params = append(params, c)
			continue
		}
		final = c
		break
	}
	switch final {
	case 'A':
		e.moveVertical(-1)
	case 'B':
		e.moveVertical(1)
	case 'C':
		if e.cur < len(e.buf) {
			e.cur++
		}
	case 'D':
		if e.cur > 0 {
			e.cur--
		}
	case 'H':
		e.cur = e.lineStart()
	case 'F':
		e.cur = e.lineEnd()
	case '~':
		switch string(params) {
		case "3":
			e.deleteRange(e.cur, e.cur+1)
		case "1", "7":
			e.cur = e.lineStart()
		case "4", "8":
			e.cur = e.lineEnd()
		}
	}
}

// edit reads one message; eof is true when the user exits (Ctrl+D on an empty
// buffer, or stdin closing).
func (e *editor) edit() (msg string, eof bool) {
	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot enter raw mode: %v\n", err)
		return "", true
	}
	defer term.Restore(int(os.Stdin.Fd()), old)
	e.buf, e.cur, e.lastRow = e.buf[:0], 0, 0
	e.render()
	for {
		r, _, err := e.in.ReadRune()
		if err != nil {
			os.Stdout.WriteString("\r\n")
			return "", true
		}
		switch {
		case r == 0x04: // Ctrl+D
			if len(e.buf) == 0 {
				os.Stdout.WriteString("\r\n")
				return "", true
			}
			return e.finish(), false
		case r == '\r' || r == '\n':
			// Enter runs a single-line slash command immediately — unless
			// this newline is part of a paste still streaming in.
			if text := string(e.buf); strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "//") &&
				!strings.ContainsRune(text, '\n') &&
				e.in.Buffered() == 0 && !stdinHasData(batchWait) {
				return e.finish(), false
			}
			e.insert('\n')
		case r == 0x03: // Ctrl+C clears the input
			e.buf, e.cur = e.buf[:0], 0
		case r == 0x7f || r == 0x08: // Backspace
			e.deleteRange(e.cur-1, e.cur)
		case r == 0x01: // Ctrl+A
			e.cur = e.lineStart()
		case r == 0x05: // Ctrl+E
			e.cur = e.lineEnd()
		case r == 0x0b: // Ctrl+K: kill to line end; at line end, join lines
			if end := e.lineEnd(); end == e.cur && e.cur < len(e.buf) {
				e.deleteRange(e.cur, e.cur+1)
			} else {
				e.deleteRange(e.cur, end)
			}
		case r == 0x15: // Ctrl+U
			e.deleteRange(e.lineStart(), e.cur)
		case r == 0x17: // Ctrl+W
			e.deleteWordBack()
		case r == 0x0c: // Ctrl+L
			os.Stdout.WriteString("\x1b[H\x1b[2J")
			e.lastRow = 0
		case r == 0x1b:
			e.handleEscape()
		case r == '\t':
			for i := 0; i < 4; i++ {
				e.insert(' ')
			}
		case r < 0x20: // other control characters
		default:
			e.insert(r)
		}
		// Skip redraws while more input (typically a paste) is pending.
		if e.in.Buffered() == 0 && !stdinHasData(batchWait) {
			e.render()
		}
	}
}

// ---------------------------------------------------------------------------
// API types

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type apiError struct {
	Message string          `json:"message"`
	Code    json.RawMessage `json:"code"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage    `json:"usage"`
	Error *apiError `json:"error"`
}

type completion struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *Usage    `json:"usage"`
	Error *apiError `json:"error"`
}

// ---------------------------------------------------------------------------
// session

// defaultSystem is used when no -system flag is given. Placeholders are
// expanded once at startup and again only when core state changes (/system,
// /model, /load), keeping the request prefix stable and provider-cacheable;
// per-message timestamp prefixes carry the current time instead.
const defaultSystem = "You are a helpful AI chat assistant running in llm-chat, a simple terminal harness. " +
	"The model powering you is {model}. This session began on {date} at {time}. " +
	"Every conversation message you receive is prefixed by the harness with its send time in " +
	"[YYYY-MM-DD HH:MM:SS] form; use these timestamps for temporal context, but never begin your " +
	"own replies with such a timestamp — the harness adds them automatically."

// buildDefaultSystem tailors the default prompt to the session's rendering
// capabilities.
func buildDefaultSystem(color, interactive bool) string {
	s := defaultSystem
	if color {
		s += " Your replies are rendered in the terminal with ANSI-colored markdown; supported: " +
			"#-style headings, **bold**, *italic* or _italic_, __underline__, `inline code`, " +
			"``` fenced code blocks (with basic syntax highlighting), and list/quote markers; " +
			"other markdown appears verbatim."
	} else {
		s += " Your replies are shown as plain text in a terminal, so prefer prose over heavy markdown formatting."
	}
	if interactive {
		s += " This is a live interactive session with a human typing at a terminal."
	}
	return s
}

type histMsg struct {
	Role    string
	Content string
	At      time.Time
}

type Session struct {
	client      *http.Client
	baseURL     string
	apiKey      string
	model       string
	system      string
	temperature float64 // < 0 means unset
	maxTokens   int     // 0 means unset
	stream      bool
	interactive bool
	urlLocked   bool          // -url was given explicitly: never overridden by a load
	keyLocked   bool          // -key/-key-cmd was given: env never overrides it
	keyCmd      string        // -key-cmd: shell command printing the API key
	keyTTL      time.Duration // -key-ttl: refresh the key before it expires
	keyFetched  time.Time     // when -key-cmd last produced a key
	defaultSys  string        // the built default system prompt, for "/system default"
	sysExpanded string        // system prompt with placeholders expanded, refreshed
	// only when the system prompt or model changes so the request prefix
	// stays stable (and cacheable by the provider) across turns
	history []histMsg

	// token accounting
	totalIn     int
	totalOut    int
	totalApprox bool // totals include estimated values
	lastPrompt  int  // prompt_tokens reported for the last request
}

// approxTokens estimates token count at ~4 characters per token.
func approxTokens(text string) int {
	return (len(text) + 3) / 4
}

func approxMessages(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += approxTokens(m.Content) + 4 // ~4 tokens of per-message framing
	}
	return n
}

func durStr(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func printSent(msg string) {
	fmt.Printf("%s[sent: %s; %d bytes]%s\n", cDim, time.Now().Format("2006-01-02 15:04:05"), len(msg), cReset)
}

func withCommas(n int) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// gatewayURL is the local OpenClaw gateway, whose token is only ever sent
// there — never to whatever endpoint a loaded conversation happens to name.
const gatewayURL = "http://127.0.0.1:18789"

// keyCmdTimeout is generous: a credential helper may prompt for a passphrase
// or a hardware-key touch before it prints anything.
const keyCmdTimeout = 2 * time.Minute

// runKeyCmd executes -key-cmd and returns the key it prints. The command runs
// through the shell so pipelines and helpers ("pass show …", "op read …")
// work as written; $LLM_CHAT_ENDPOINT tells it which endpoint the key is for.
// Only the first line is used: helpers commonly print the secret first and
// metadata after.
func (s *Session) runKeyCmd() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keyCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", s.keyCmd)
	cmd.Env = append(os.Environ(), "LLM_CHAT_ENDPOINT="+s.baseURL)
	cmd.Stdin = os.Stdin // let helpers prompt on the terminal
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, sanitize(msg))
		}
		return "", err
	}
	key, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if key = strings.TrimSpace(key); key == "" {
		return "", errors.New("command produced no key")
	}
	return key, nil
}

// initKey fetches the starting key from -key-cmd.
func (s *Session) initKey() error {
	key, err := s.runKeyCmd()
	if err != nil {
		return err
	}
	s.apiKey, s.keyFetched = key, time.Now()
	return nil
}

// refreshKey re-runs -key-cmd and adopts the key if it differs from the one
// in use. It reports whether the key changed, which is what makes retrying a
// failed request worthwhile.
func (s *Session) refreshKey() bool {
	if s.keyCmd == "" {
		return false
	}
	key, err := s.runKeyCmd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%swarning:%s -key-cmd failed: %v\n", cRed, cReset, err)
		return false
	}
	s.keyFetched = time.Now()
	if key == s.apiKey {
		return false
	}
	s.apiKey = key
	return true
}

// refreshKeyIfStale renews a -key-cmd key that has outlived -key-ttl, before
// it is used. This is the proactive half of key handling: set the TTL below
// the credential's lifetime and requests never fail on an expired key; leave
// it unset and the failure-triggered refresh in send is the only backstop.
func (s *Session) refreshKeyIfStale() {
	if s.keyCmd == "" || s.keyTTL <= 0 {
		return
	}
	if !s.keyFetched.IsZero() && time.Since(s.keyFetched) < s.keyTTL {
		return
	}
	s.refreshKey()
}

// resolveKey picks the API key for the current endpoint from the environment.
// An explicit -key always wins; otherwise the choice is endpoint-specific, so
// it must be redone whenever the endpoint changes.
func (s *Session) resolveKey() {
	if s.keyLocked { // includes -key-cmd, whose key is refreshed on demand
		return
	}
	keys := []string{"LLM_CHAT_API_KEY"}
	if strings.HasPrefix(s.baseURL, gatewayURL) {
		keys = append(keys, "OPENCLAW_GATEWAY_TOKEN")
	}
	keys = append(keys, "OPENROUTER_API_KEY", "OPENAI_API_KEY")
	s.apiKey = envOr(keys, "")
}

// setEndpoint switches endpoints and re-resolves the credential for the new
// one. It reports whether the endpoint actually changed.
func (s *Session) setEndpoint(u string) bool {
	u = strings.TrimRight(u, "/")
	if u == "" || u == s.baseURL {
		return false
	}
	s.baseURL = u
	// A -key-cmd key is kept across the switch; if it turns out to be wrong
	// for the new endpoint, the first failed request refreshes it with
	// $LLM_CHAT_ENDPOINT set to the new value.
	s.resolveKey()
	return true
}

// expand substitutes {model}, {endpoint}, {date}, and {time} placeholders.
func (s *Session) expand(text string) string {
	now := time.Now()
	return strings.NewReplacer(
		"{model}", s.model,
		"{endpoint}", s.baseURL,
		"{date}", now.Format("Monday, January 2, 2006"),
		"{time}", now.Format("15:04 MST"),
	).Replace(text)
}

// refreshSystem re-expands the system prompt template. Called only when core
// state changes (startup, /system, /model, /load) — never per message.
func (s *Session) refreshSystem() {
	s.sysExpanded = s.expand(s.system)
}

func (s *Session) push(role, content string) {
	s.history = append(s.history, histMsg{Role: role, Content: content, At: time.Now()})
}

func (s *Session) messages() []Message {
	msgs := make([]Message, 0, len(s.history)+1)
	if s.sysExpanded != "" {
		msgs = append(msgs, Message{Role: "system", Content: s.sysExpanded})
	}
	for _, h := range s.history {
		msgs = append(msgs, Message{Role: h.Role, Content: h.At.Format("[2006-01-02 15:04:05] ") + h.Content})
	}
	return msgs
}

func (s *Session) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", "llm-chat")
	return req, nil
}

func httpError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var e struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != nil && e.Error.Message != "" {
		return fmt.Errorf("%s: %s", resp.Status, sanitize(e.Error.Message))
	}
	return fmt.Errorf("%s: %s", resp.Status, sanitize(strings.TrimSpace(string(body))))
}

// reasoningMark prefixes every streamed reasoning line. It keeps reasoning
// visually distinct from the answer and, because parseTranscript skips lines
// carrying it, stops copied reasoning from being reloaded as assistant text.
const reasoningMark = "\u250a"

// reasoningWriter prints reasoning deltas dimmed and line-prefixed. Reasoning
// is display-only: it never enters the conversation history.
type reasoningWriter struct {
	active      bool
	atLineStart bool
}

func newReasoningWriter() *reasoningWriter {
	return &reasoningWriter{atLineStart: true}
}

func (w *reasoningWriter) write(s string) {
	var b strings.Builder
	for _, r := range sanitize(s) {
		if w.atLineStart {
			b.WriteString(cDim + reasoningMark + " ")
			w.atLineStart, w.active = false, true
		}
		if r == '\n' {
			b.WriteString(cReset + "\n")
			w.atLineStart = true
			continue
		}
		b.WriteRune(r)
	}
	os.Stdout.WriteString(b.String())
}

// end closes an open reasoning block, leaving a blank line before the answer.
func (w *reasoningWriter) end() {
	if !w.active {
		return
	}
	if !w.atLineStart {
		os.Stdout.WriteString(cReset + "\n")
	}
	os.Stdout.WriteString("\n")
	w.active, w.atLineStart = false, true
}

// send performs one chat completion, printing the response as it arrives.
// Returns the full assistant text (possibly partial on error/interrupt).
//
// On failure it gives -key-cmd a chance to supply a rotated key and retries
// once. The retry is skipped when the request was cancelled or had already
// printed part of an answer, since re-sending would duplicate it.
func (s *Session) send(ctx context.Context) (string, *Usage, error) {
	s.refreshKeyIfStale()
	content, usage, err := s.sendOnce(ctx)
	if err == nil || content != "" || ctx.Err() != nil || !s.refreshKey() {
		return content, usage, err
	}
	fmt.Fprintf(os.Stderr, "%s[key refreshed; retrying]%s\n", cDim, cReset)
	return s.sendOnce(ctx)
}

func (s *Session) sendOnce(ctx context.Context) (string, *Usage, error) {
	creq := chatRequest{Model: s.model, Messages: s.messages(), Stream: s.stream}
	if s.stream {
		creq.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if s.temperature >= 0 {
		creq.Temperature = &s.temperature
	}
	if s.maxTokens > 0 {
		creq.MaxTokens = &s.maxTokens
	}
	body, err := json.Marshal(creq)
	if err != nil {
		return "", nil, err
	}
	req, err := s.newRequest(ctx, "POST", "/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, httpError(resp)
	}

	if !s.stream {
		var c completion
		if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
			return "", nil, err
		}
		if c.Error != nil {
			return "", nil, errors.New(c.Error.Message)
		}
		if len(c.Choices) == 0 {
			return "", c.Usage, errors.New("empty response (no choices)")
		}
		content := c.Choices[0].Message.Content
		mw := newMdWriter(s.interactive)
		mw.WriteString(content)
		mw.Close()
		return content, c.Usage, nil
	}

	mw := newMdWriter(s.interactive)
	defer mw.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var full strings.Builder
	var usage *Usage
	var event strings.Builder // data: payload of the SSE event being read
	rw := newReasoningWriter()
	done := false

	// dispatch handles one complete SSE event. Malformed JSON is reported
	// rather than skipped: dropping a chunk would silently lose answer text.
	dispatch := func() error {
		data := strings.TrimSuffix(event.String(), "\n")
		event.Reset()
		if data == "" {
			return nil
		}
		if strings.TrimSpace(data) == "[DONE]" {
			done = true
			return nil
		}
		var ck streamChunk
		if err := json.Unmarshal([]byte(data), &ck); err != nil {
			return fmt.Errorf("malformed streaming event: %w", err)
		}
		if ck.Error != nil {
			return errors.New(sanitize(ck.Error.Message))
		}
		if ck.Usage != nil {
			usage = ck.Usage
		}
		if len(ck.Choices) == 0 {
			return nil
		}
		d := ck.Choices[0].Delta
		if d.Reasoning != "" && s.interactive {
			rw.write(d.Reasoning)
		}
		if d.Content != "" {
			rw.end()
			mw.WriteString(d.Content)
			full.WriteString(d.Content)
		}
		return nil
	}

	// An SEE event ends at a blank line; its data may span several data:
	// lines, which the spec joins with newlines.
	for !done && sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				rw.end()
				return full.String(), usage, err
			}
		case strings.HasPrefix(line, ":"): // comment / keepalive
		default:
			if v, ok := strings.CutPrefix(line, "data:"); ok {
				event.WriteString(strings.TrimPrefix(v, " "))
				event.WriteByte('\n')
			}
			// other SSE fields (event:, id:, retry:) carry no payload
		}
	}
	if !done { // a stream may end without a final blank line
		if err := dispatch(); err != nil {
			rw.end()
			return full.String(), usage, err
		}
	}
	rw.end()
	if err := sc.Err(); err != nil {
		return full.String(), usage, err
	}
	return full.String(), usage, nil
}

// runTurn sends the current history and handles printing, interrupts, and
// appending the assistant reply. Ctrl+C cancels the request but not the chat.
func (s *Session) runTurn(sigc chan os.Signal) {
	select { // drain any Ctrl+C pressed at the prompt
	case <-sigc:
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		select {
		case <-sigc:
			cancel()
		case <-done:
		}
	}()

	if s.interactive {
		fmt.Printf("\n%s◆ %s%s\n", cMag, s.model, cReset)
	}
	reqEstimate := approxMessages(s.messages())
	start := time.Now()
	content, usage, err := s.send(ctx)
	elapsed := time.Since(start)
	close(done)
	cancel()

	if content != "" {
		s.push("assistant", content)
		if !strings.HasSuffix(content, "\n") {
			fmt.Println()
		}
	}
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		fmt.Printf("%s[interrupted]%s\n", cDim, cReset)
	default:
		fmt.Fprintf(os.Stderr, "%serror:%s %v\n", cRed, cReset, err)
		if s.interactive {
			fmt.Printf("%s(/retry to resend)%s\n", cDim, cReset)
		}
	}
	if usage != nil || content != "" {
		in, out, mark := 0, 0, ""
		if usage != nil {
			in, out = usage.PromptTokens, usage.CompletionTokens
			s.lastPrompt = usage.PromptTokens
		} else { // endpoint reported no usage; estimate at ~4 chars/token
			in, out, mark = reqEstimate, approxTokens(content), "~"
			s.totalApprox = true
		}
		s.totalIn += in
		s.totalOut += out
		if s.interactive {
			sessMark := ""
			if s.totalApprox {
				sessMark = "~"
			}
			fmt.Printf("%s[recv: %s; %s; %s%s in / %s%s out; session %s%s in / %s%s out]%s\n", cDim,
				time.Now().Format("2006-01-02 15:04:05"), durStr(elapsed),
				mark, withCommas(in), mark, withCommas(out),
				sessMark, withCommas(s.totalIn), sessMark, withCommas(s.totalOut), cReset)
		}
	}
	if s.interactive { // rule separating this AI turn from the next prompt
		fmt.Printf("\n%s%s%s\n\n", cDim, strings.Repeat("─", termWidth()), cReset)
	}
}

// fetchModels returns the endpoint's model ids in the order the API lists
// them, retrying once if -key-cmd yields a fresh key after a failure.
func (s *Session) fetchModels(ctx context.Context) ([]string, error) {
	s.refreshKeyIfStale()
	ids, err := s.fetchModelsOnce(ctx)
	if err == nil || ctx.Err() != nil || !s.refreshKey() {
		return ids, err
	}
	return s.fetchModelsOnce(ctx)
}

func (s *Session) fetchModelsOnce(ctx context.Context) ([]string, error) {
	req, err := s.newRequest(ctx, "GET", "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, sanitize(m.ID)) // ids reach the terminal and the prompt
	}
	return ids, nil
}

func (s *Session) listModels(filter string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	all, err := s.fetchModels(ctx)
	if err != nil {
		return err
	}
	var ids []string
	f := strings.ToLower(filter)
	for _, id := range all {
		if f == "" || strings.Contains(strings.ToLower(id), f) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Println(id)
	}
	fmt.Printf("%s%d models%s\n", cDim, len(ids), cReset)
	return nil
}

func (s *Session) saveTranscript(path string) error {
	if path == "" {
		path = time.Now().Format("chat-20060102-150405.md")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# llm-chat transcript\n\n- endpoint: %s\n- model: %s\n- date: %s\n\n",
		s.baseURL, s.model, time.Now().Format(time.RFC3339))
	if s.sysExpanded != "" {
		fmt.Fprintf(&b, "## system\n\n%s\n\n", s.sysExpanded)
	}
	for _, m := range s.history {
		fmt.Fprintf(&b, "## %s · %s\n\n%s\n\n", m.Role, m.At.Format("2006-01-02 15:04:05"), m.Content)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("%ssaved to %s%s\n", cDim, path, cReset)
	return nil
}

// ---------------------------------------------------------------------------
// conversation save/load

type savedMsg struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	At      time.Time `json:"at,omitzero"`
}

type savedChat struct {
	Endpoint string     `json:"endpoint,omitempty"`
	Model    string     `json:"model,omitempty"`
	System   *string    `json:"system,omitempty"`
	Saved    time.Time  `json:"saved,omitzero"`
	Messages []savedMsg `json:"messages"`
}

func (s *Session) saveJSON(path string) error {
	c := savedChat{Endpoint: s.baseURL, Model: s.model, System: &s.system, Saved: time.Now()}
	for _, h := range s.history {
		c.Messages = append(c.Messages, savedMsg{Role: h.Role, Content: h.Content, At: h.At})
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("%ssaved JSON to %s%s\n", cDim, path, cReset)
	return nil
}

// apply replaces the session's conversation state and returns a summary.
func (s *Session) apply(c savedChat) string {
	// A saved conversation records the endpoint it was held with. Restoring it
	// keeps history, model, and endpoint consistent instead of replaying a
	// local conversation against whatever endpoint happens to be the default;
	// an explicit -url still wins, and the credential is re-resolved for the
	// endpoint actually being used.
	endpointChanged := false
	if c.Endpoint != "" && !s.urlLocked {
		endpointChanged = s.setEndpoint(c.Endpoint)
	}
	if c.System != nil {
		s.system = *c.System
	}
	if c.Model != "" {
		s.model = c.Model
	}
	s.history = nil
	for _, m := range c.Messages {
		if m.Role == "" || strings.TrimSpace(m.Content) == "" {
			continue
		}
		at := m.At
		if at.IsZero() {
			at = time.Now()
		}
		s.history = append(s.history, histMsg{Role: m.Role, Content: m.Content, At: at})
	}
	s.lastPrompt = 0
	s.refreshSystem()
	summary := fmt.Sprintf("loaded %d messages", len(s.history))
	if endpointChanged {
		summary += "; endpoint " + s.baseURL
	}
	if c.Model != "" {
		summary += "; model " + s.model
	}
	if c.System != nil {
		summary += fmt.Sprintf("; system prompt %d chars", len(s.system))
	}
	return summary
}

// loadFromText accepts either a saved JSON conversation or a transcript
// copied from the terminal display, detected by the leading character.
func (s *Session) loadFromText(text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", errors.New("empty input")
	}
	var c savedChat
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &c); err != nil {
			return "", fmt.Errorf("parsing JSON: %w", err)
		}
	} else {
		pc, err := parseTranscript(text)
		if err != nil {
			return "", err
		}
		c = *pc
	}
	return s.apply(c), nil
}

func (s *Session) loadFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return s.loadFromText(string(data))
}

// parseTranscript rebuilds a conversation from the terminal display format:
// "you ❯" and "◆ model" open messages, "[sent: …]" / "[recv: …]" close them
// (supplying timestamps), and a "system ❯" block restores the system prompt.
// Everything outside those blocks (banner, rules, status noise) is ignored.
func parseTranscript(text string) (*savedChat, error) {
	var (
		c        savedChat
		state    string // "", "system", "user", "assistant"
		curLines []string
		sysLines []string
	)
	flush := func(ts time.Time) {
		if state == "user" || state == "assistant" {
			if content := strings.TrimSpace(strings.Join(curLines, "\n")); content != "" {
				c.Messages = append(c.Messages, savedMsg{Role: state, Content: content, At: ts})
			}
		}
		curLines = nil
	}
	parseTs := func(t, prefix string) time.Time {
		rest := strings.TrimPrefix(t, prefix)
		if i := strings.IndexByte(rest, ';'); i > 0 {
			if ts, err := time.ParseInLocation("2006-01-02 15:04:05", rest[:i], time.Local); err == nil {
				return ts
			}
		}
		return time.Time{}
	}
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case t == "you ❯":
			flush(time.Time{})
			state = "user"
		case strings.HasPrefix(t, "◆ "):
			flush(time.Time{})
			state = "assistant"
			model := strings.TrimSpace(strings.TrimPrefix(t, "◆ "))
			if i := strings.Index(model, " · "); i > 0 { // older header format
				model = model[:i]
			}
			c.Model = model
		case t == "system ❯":
			flush(time.Time{})
			state = "system"
		case strings.HasPrefix(t, "[sent: ") && state == "user":
			flush(parseTs(t, "[sent: "))
			state = ""
		case strings.HasPrefix(t, "[recv: ") && state == "assistant":
			flush(parseTs(t, "[recv: "))
			state = ""
		case state == "system":
			sysLines = append(sysLines, ln)
		case state == "assistant" && strings.HasPrefix(t, reasoningMark):
			// display-only reasoning; never part of the answer
		case state == "user" || state == "assistant":
			curLines = append(curLines, ln)
		}
	}
	flush(time.Time{})
	if sys := strings.TrimSpace(strings.Join(sysLines, "\n")); sys != "" {
		c.System = &sys
	}
	if len(c.Messages) == 0 {
		return nil, errors.New("no messages found (expected JSON or a transcript with \"you ❯\" / \"◆ model\" markers)")
	}
	return &c, nil
}

// ---------------------------------------------------------------------------
// commands

func loadSystemArg(arg string) (string, error) {
	if strings.HasPrefix(arg, "@") {
		data, err := os.ReadFile(strings.TrimPrefix(arg, "@"))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(data), "\n"), nil
	}
	return arg, nil
}

func printHelp() {
	fmt.Print(cDim + `commands
  /model [id]        show or set the model
  /models [filter]   list models offered by the endpoint
  /system [text]     show or set the system prompt; @file loads from a file;
                     "/system clear" removes it, "/system default" restores
                     the built-in default. Placeholders {model} {endpoint}
                     {date} {time} expand when the prompt or model changes,
                     not per message (keeps the prefix cacheable).
  /clear             clear conversation history (keeps system prompt)
  /tokens            show context size and session token totals
  /undo              remove the last exchange
  /retry             resend the last user message
  /save [file]       save the conversation: markdown, or JSON if the file
                     name ends in .json
  /load [file]       load a conversation from a JSON save or a transcript
                     copied from the terminal; with no file, paste it at the
                     prompt and end with Ctrl+D
  /quit              exit (or Ctrl+D)

input
  Enter inserts a newline; Ctrl+D sends the message (on empty input: exit).
  A single-line /command runs on Enter directly.
  Arrow keys move the cursor, including up/down across lines. Backspace and
  Delete work; Home/End or Ctrl+A/E jump within the line; Ctrl+K/U kill to
  line end/start; Ctrl+W deletes the previous word; Ctrl+C clears the input.
  Pasting multi-line text inserts it as-is.
  Start with // to send a literal message beginning with "/".
  While a response is streaming, Ctrl+C interrupts it.
  Each message is sent to the model with a [YYYY-MM-DD HH:MM:SS] prefix.
` + cReset)
}

// handleCommand runs a slash command; retry asks the caller to resend the
// last user message, paste asks it to capture a pasted conversation to load.
func handleCommand(s *Session, input string) (quit, retry, paste bool) {
	cmd := strings.Fields(input)[0]
	arg := strings.TrimSpace(strings.TrimPrefix(input, cmd))
	switch cmd {
	case "/help", "/?":
		printHelp()
	case "/quit", "/exit", "/q":
		return true, false, false
	case "/model":
		if arg == "" {
			fmt.Printf("%s%s%s\n", cMag, s.model, cReset)
		} else {
			s.model = arg
			s.refreshSystem()
			fmt.Printf("%smodel set to %s%s\n", cDim, s.model, cReset)
		}
	case "/models":
		if err := s.listModels(arg); err != nil {
			fmt.Fprintf(os.Stderr, "%serror:%s %v\n", cRed, cReset, err)
		}
	case "/system":
		switch {
		case arg == "":
			if s.system == "" {
				fmt.Printf("%s(no system prompt)%s\n", cDim, cReset)
			} else {
				fmt.Println(s.system)
				if s.sysExpanded != s.system {
					fmt.Printf("%s→ %s%s\n", cDim, s.sysExpanded, cReset)
				}
			}
		case arg == "clear", arg == "none":
			s.system = ""
			s.refreshSystem()
			fmt.Printf("%ssystem prompt cleared%s\n", cDim, cReset)
		case arg == "default":
			s.system = s.defaultSys
			s.refreshSystem()
			fmt.Printf("%sdefault system prompt restored%s\n", cDim, cReset)
		default:
			sys, err := loadSystemArg(arg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%serror:%s %v\n", cRed, cReset, err)
				break
			}
			s.system = sys
			s.refreshSystem()
			fmt.Printf("%ssystem prompt set (%d chars)%s\n", cDim, len(sys), cReset)
		}
	case "/clear", "/reset":
		s.history = nil
		s.lastPrompt = 0
		fmt.Printf("%shistory cleared%s\n", cDim, cReset)
	case "/tokens":
		fmt.Printf("%scontext: %d messages, ~%s tokens", cDim, len(s.history), withCommas(approxMessages(s.messages())))
		if s.lastPrompt > 0 {
			fmt.Printf(" (%s in on last request)", withCommas(s.lastPrompt))
		}
		sessMark := ""
		if s.totalApprox {
			sessMark = "~"
		}
		fmt.Printf("\nsession: %s%s in / %s%s out tokens%s\n", sessMark, withCommas(s.totalIn), sessMark, withCommas(s.totalOut), cReset)
	case "/undo":
		if n := len(s.history); n > 0 && s.history[n-1].Role == "assistant" {
			s.history = s.history[:n-1]
		}
		if n := len(s.history); n > 0 && s.history[n-1].Role == "user" {
			s.history = s.history[:n-1]
			fmt.Printf("%slast exchange removed%s\n", cDim, cReset)
		} else {
			fmt.Printf("%snothing to undo%s\n", cDim, cReset)
		}
	case "/retry":
		if n := len(s.history); n > 0 && s.history[n-1].Role == "assistant" {
			s.history = s.history[:n-1]
		}
		if n := len(s.history); n > 0 && s.history[n-1].Role == "user" {
			return false, true, false
		}
		fmt.Printf("%snothing to retry%s\n", cDim, cReset)
	case "/save":
		var err error
		if strings.HasSuffix(strings.ToLower(arg), ".json") {
			err = s.saveJSON(arg)
		} else {
			err = s.saveTranscript(arg)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%serror:%s %v\n", cRed, cReset, err)
		}
	case "/load":
		if arg == "" {
			return false, false, true
		}
		summary, err := s.loadFromFile(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%serror:%s %v\n", cRed, cReset, err)
		} else {
			fmt.Printf("%s%s%s\n", cDim, summary, cReset)
		}
	default:
		fmt.Printf("%sunknown command %s — /help for help, // to send a literal /%s\n", cDim, cmd, cReset)
	}
	return false, false, false
}

// ---------------------------------------------------------------------------

// readDotMessage reads stdin lines until a line holding only "." (ends one
// message) or EOF (eof=true; any pending text is the final message).
func readDotMessage(rd *bufio.Reader) (msg string, eof bool) {
	var b strings.Builder
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			b.WriteString(line)
			return b.String(), true
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return b.String(), false
		}
		b.WriteString(trimmed)
		b.WriteByte('\n')
	}
}

// runNonInteractive drives a multi-turn chat from stdin without the terminal
// editor: messages are '.'-terminated, responses go to stdout, history is
// kept between messages. Exits non-zero on the first API error.
func runNonInteractive(s *Session, initial string, sigc chan os.Signal) {
	rd := bufio.NewReader(os.Stdin)
	sent := false
	for {
		msg, eof := readDotMessage(rd)
		if initial != "" {
			if strings.TrimSpace(msg) != "" {
				msg = initial + "\n\n" + msg
			} else {
				msg = initial
			}
			initial = ""
		}
		msg = strings.TrimSpace(msg)
		if msg != "" {
			sent = true
			s.push("user", msg)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				select {
				case <-sigc:
					cancel()
				case <-done:
				}
			}()
			content, _, err := s.send(ctx)
			close(done)
			cancel()
			if content != "" {
				if !strings.HasSuffix(content, "\n") {
					fmt.Println()
				}
				s.push("assistant", content)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
		}
		if eof {
			break
		}
	}
	if !sent {
		fmt.Fprintln(os.Stderr, "empty input")
		os.Exit(1)
	}
}

// envDuration reads a duration ("55m", "3600s") from the environment.
func envDuration(key string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: ignoring $%s: %v\n", key, err)
		return 0
	}
	return d
}

func envOr(keys []string, fallback string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return fallback
}

func main() {
	defaultURL := "https://openrouter.ai/api/v1"
	if os.Getenv("OPENCLAW_GATEWAY_TOKEN") != "" {
		defaultURL = "http://127.0.0.1:18789/v1" // local OpenClaw gateway
	}
	baseURL := flag.String("url", envOr([]string{"LLM_CHAT_BASE_URL"}, defaultURL),
		"API base URL of an OpenAI-compatible endpoint (default: the local OpenClaw gateway if $OPENCLAW_GATEWAY_TOKEN is set, else OpenRouter)")
	apiKey := flag.String("key", "", "API key (default: -key-cmd if given, else $LLM_CHAT_API_KEY; $OPENCLAW_GATEWAY_TOKEN when talking to the gateway; then $OPENROUTER_API_KEY, $OPENAI_API_KEY)")
	keyCmd := flag.String("key-cmd", os.Getenv("LLM_CHAT_KEY_CMD"),
		"shell command printing the API key; re-run to pick up a rotated key when a request fails")
	keyTTL := flag.Duration("key-ttl", envDuration("LLM_CHAT_KEY_TTL"),
		"re-run -key-cmd once the cached key is older than this, e.g. 55m ($LLM_CHAT_KEY_TTL); 0 refreshes only after a failed request")
	model := flag.String("model", envOr([]string{"LLM_CHAT_MODEL"}, ""), "model id (default: the first model the endpoint lists; see /models)")
	system := flag.String("system", "", "system prompt, @file to load from a file, or \"none\" for no system prompt (default: a generic built-in prompt)")
	temperature := flag.Float64("temperature", -1, "sampling temperature (endpoint default if unset)")
	maxTokens := flag.Int("max-tokens", 0, "max completion tokens (endpoint default if unset)")
	noStream := flag.Bool("no-stream", false, "disable streaming responses")
	interactiveF := flag.Bool("interactive", supportsTerminalEditor && isTerminal(os.Stdin.Fd()),
		"interactive session with the terminal editor (default: stdin is a tty, except on Windows); when false, read '.'-terminated messages from stdin")
	colorF := flag.Bool("color", true, "ANSI colors and markdown rendering (default: true when interactive and stdout is a tty)")
	loadPath := flag.String("load", "", "load a conversation at startup (JSON save or copied transcript)")
	flag.Parse()

	interactive := *interactiveF
	if interactive && !supportsTerminalEditor {
		fmt.Fprintln(os.Stderr, "error: the terminal editor is not supported on Windows; use -interactive=false and end each message with a line containing only '.'")
		os.Exit(1)
	}
	var colorSet, urlSet, keySet bool
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "color":
			colorSet = true
		case "url":
			urlSet = true
		case "key":
			keySet = true
		}
	})
	color := *colorF
	if !colorSet {
		color = interactive && isTerminal(os.Stdout.Fd()) && os.Getenv("NO_COLOR") == ""
	}
	if !color {
		disableColors()
	}

	builtDefault := buildDefaultSystem(color, interactive)
	var sys string
	switch *system {
	case "":
		sys = builtDefault
	case "none":
		sys = ""
	default:
		var err error
		if sys, err = loadSystemArg(*system); err != nil {
			fmt.Fprintf(os.Stderr, "error loading system prompt: %v\n", err)
			os.Exit(1)
		}
	}

	s := &Session{
		client:      &http.Client{},
		baseURL:     strings.TrimRight(*baseURL, "/"),
		apiKey:      *apiKey, // resolveKey fills this in when -key was absent
		model:       *model,
		system:      sys,
		temperature: *temperature,
		maxTokens:   *maxTokens,
		stream:      !*noStream,
		interactive: interactive,
		urlLocked:   urlSet || os.Getenv("LLM_CHAT_BASE_URL") != "",
		keyLocked:   keySet || *keyCmd != "",
		keyCmd:      *keyCmd,
		keyTTL:      *keyTTL,
		defaultSys:  builtDefault,
	}
	s.resolveKey()
	if s.keyTTL < 0 {
		fmt.Fprintln(os.Stderr, "error: -key-ttl cannot be negative")
		os.Exit(1)
	}
	if s.keyTTL > 0 && s.keyCmd == "" {
		fmt.Fprintf(os.Stderr, "%swarning:%s -key-ttl has no effect without -key-cmd\n", cRed, cReset)
	}
	if s.keyCmd != "" { // -key-cmd is the key source; -key only seeds it
		if err := s.initKey(); err != nil {
			fmt.Fprintf(os.Stderr, "error: -key-cmd failed: %v\n", err)
			os.Exit(1)
		}
	}

	var loadSummary string
	if *loadPath != "" {
		var err error
		if loadSummary, err = s.loadFromFile(*loadPath); err != nil {
			fmt.Fprintf(os.Stderr, "error loading conversation: %v\n", err)
			os.Exit(1)
		}
	}

	if s.model == "" { // default to the first model the endpoint offers
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		ids, err := s.fetchModels(ctx)
		cancel()
		if err == nil && len(ids) == 0 {
			err = errors.New("endpoint lists no models")
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not pick a default model from %s (%v); set one with -model or /model\n", s.baseURL, err)
		} else {
			s.model = ids[0]
		}
	}
	s.refreshSystem() // expand once; per-turn requests reuse this verbatim

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)

	initial := strings.TrimSpace(strings.Join(flag.Args(), " "))

	if !interactive {
		runNonInteractive(s, initial, sigc)
		return
	}

	if s.apiKey == "" {
		fmt.Fprintf(os.Stderr, "%swarning: no API key set (-key or $LLM_CHAT_API_KEY / $OPENROUTER_API_KEY)%s\n", cDim, cReset)
	}
	fmt.Printf("%sllm-chat · %s · %s%s%s%s\n", cDim, s.baseURL, cReset+cMag, s.model, cReset+cDim, cReset)
	fmt.Printf("%s/help for commands · Enter for newline · Ctrl+D to send (on empty input: exit)%s\n", cDim, cReset)
	if loadSummary != "" {
		fmt.Printf("%s%s%s\n", cDim, loadSummary, cReset)
	}
	if s.sysExpanded != "" {
		fmt.Printf("\n%ssystem ❯%s\n%s%s%s\n", cMag+cBold, cReset, cDim, s.sysExpanded, cReset)
	}
	fmt.Println()

	rd := bufio.NewReader(os.Stdin)

	if initial != "" {
		fmt.Printf("%syou ❯%s\n%s\n", cCyan+cBold, cReset, initial)
		printSent(initial)
		s.push("user", initial)
		s.runTurn(sigc)
	}

	ed := &editor{
		in:     rd,
		prompt: cCyan + cBold + "you ❯" + cReset,
	}
	for {
		raw, eof := ed.edit()
		if eof {
			break
		}
		msg := strings.TrimSpace(raw)
		if msg == "" {
			continue
		}
		if !strings.Contains(msg, "\n") && strings.HasPrefix(msg, "/") && !strings.HasPrefix(msg, "//") {
			quit, retry, paste := handleCommand(s, msg)
			if quit {
				break
			}
			if retry {
				s.runTurn(sigc)
			}
			if paste {
				fmt.Printf("%spaste a transcript or JSON below, then Ctrl+D (Ctrl+D on empty input cancels)%s\n", cDim, cReset)
				oldPrompt := ed.prompt
				ed.prompt = cMag + cBold + "paste ❯" + cReset
				pasted, cancelled := ed.edit()
				ed.prompt = oldPrompt
				if cancelled || strings.TrimSpace(pasted) == "" {
					fmt.Printf("%sload cancelled%s\n", cDim, cReset)
				} else if summary, err := s.loadFromText(pasted); err != nil {
					fmt.Fprintf(os.Stderr, "%serror:%s %v\n", cRed, cReset, err)
				} else {
					fmt.Printf("%s%s%s\n", cDim, summary, cReset)
				}
			}
			continue
		}
		if strings.HasPrefix(msg, "//") { // "//literal" escape
			msg = msg[1:]
		}
		printSent(msg)
		s.push("user", msg)
		s.runTurn(sigc)
	}
}
