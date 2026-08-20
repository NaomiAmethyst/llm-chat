// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

// Streaming markdown highlighter with two output modes: on a terminal (live)
// content is echoed raw as it streams and each line is repainted in place
// with ANSI colors once it completes; off-terminal (e.g. -color on a pipe)
// each line is emitted highlighted exactly once, with no cursor movement.
// All markdown characters are kept (markers drawn in dark gray) so copied
// text is the model's verbatim output.

import (
	"fmt"
	"os"
	"strings"
	"unicode"
)

const (
	mGray = "\x1b[90m" // markdown punctuation
	mCode = "\x1b[33m" // inline code
	mHead = "\x1b[1;94m"
	mBold = "\x1b[1m"
	mItal = "\x1b[3m"
	mUndr = "\x1b[4m"
	mStr  = "\x1b[32m" // strings inside code blocks
	mKw   = "\x1b[94m" // keywords inside code blocks
	mNum  = "\x1b[36m" // numbers inside code blocks
)

var codeKeywords = map[string]struct{}{}

func init() {
	for _, w := range strings.Fields(`func def return if else elif for while break continue
		var let const class struct type interface import from package use pub fn match switch
		case default try catch except finally throw raise new in not and or nil null None
		true false True False static void int int64 uint string bool float float64 double
		char byte rune range map chan go defer select async await yield lambda print println
		public private protected extends implements enum impl trait where mut self this super`) {
		codeKeywords[w] = struct{}{}
	}
}

// ansiEmitter builds a line's output, wrapping visible runes manually at the
// terminal width so color codes never affect cursor arithmetic.
type ansiEmitter struct {
	b     strings.Builder
	width int
	col   int
}

func (a *ansiEmitter) code(s string) { a.b.WriteString(s) }

func (a *ansiEmitter) text(s string) {
	for _, r := range s {
		a.b.WriteRune(r)
		a.col++
		if a.col >= a.width {
			a.b.WriteString("\r\n")
			a.col = 0
		}
	}
}

type mdWriter struct {
	plain   bool   // pass-through mode (colors disabled)
	live    bool   // terminal mode: echo raw while streaming, repaint in place
	width   int    // captured at the start of each logical line (live mode)
	col     int    // visual column of the raw echo (live mode)
	wraps   int    // rows the current logical line has wrapped onto (live mode)
	line    []rune // raw runes of the current logical line
	inFence bool
}

// newMdWriter returns a renderer for one response. live selects in-place
// repainting for a terminal; when false, each line is emitted highlighted
// once it completes, with no cursor movement (safe for pipes).
func newMdWriter(live bool) *mdWriter { return &mdWriter{plain: cReset == "", live: live} }

func (m *mdWriter) WriteString(s string) {
	if m.plain {
		os.Stdout.WriteString(s)
		return
	}
	var raw strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			if m.live {
				os.Stdout.WriteString(raw.String())
				raw.Reset()
			}
			m.endLine(true)
		case r == '\t':
			for i := 0; i < 4; i++ {
				m.echoRune(' ', &raw)
			}
		case r == '\r' || r < 0x20:
			// strip carriage returns and stray control characters
		default:
			m.echoRune(r, &raw)
		}
	}
	if m.live {
		os.Stdout.WriteString(raw.String())
	}
}

func (m *mdWriter) echoRune(r rune, raw *strings.Builder) {
	if m.live && len(m.line) == 0 && m.wraps == 0 && m.col == 0 {
		m.width = termWidth()
	}
	m.line = append(m.line, r)
	if !m.live {
		return
	}
	raw.WriteRune(r)
	m.col++
	if m.col >= m.width {
		raw.WriteString("\r\n")
		m.col = 0
		m.wraps++
	}
}

// endLine emits the completed logical line with highlighting; in live mode it
// repaints over the raw echo.
func (m *mdWriter) endLine(newline bool) {
	width := 1 << 30 // no wrapping off-terminal
	if m.live {
		if width = m.width; width == 0 {
			width = termWidth()
		}
	}
	em := &ansiEmitter{width: width}
	m.renderLine(string(m.line), em)
	var b strings.Builder
	if m.live {
		b.WriteString("\r")
		if m.wraps > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", m.wraps)
		}
		b.WriteString("\x1b[J")
	}
	b.WriteString(em.b.String())
	if newline {
		if m.live {
			b.WriteString("\r\n")
		} else {
			b.WriteString("\n")
		}
	}
	os.Stdout.WriteString(b.String())
	m.line, m.col, m.wraps = m.line[:0], 0, 0
}

// Close flushes a trailing unterminated line, leaving the cursor after it.
func (m *mdWriter) Close() {
	if m.plain {
		return
	}
	if len(m.line) > 0 {
		m.endLine(false)
	}
}

func headerLevel(line string) int {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	n := 0
	for i+n < len(line) && line[i+n] == '#' {
		n++
	}
	if n >= 1 && n <= 6 && (i+n == len(line) || line[i+n] == ' ') {
		return i + n
	}
	return 0
}

func (m *mdWriter) renderLine(raw string, em *ansiEmitter) {
	trimmed := strings.TrimLeft(raw, " ")
	switch {
	case strings.HasPrefix(trimmed, "```"):
		em.code(mGray)
		em.text(raw)
		em.code(cReset)
		m.inFence = !m.inFence
	case m.inFence:
		renderCode(raw, em)
	case headerLevel(raw) > 0:
		cut := headerLevel(raw)
		em.code(mGray)
		em.text(raw[:cut])
		em.code(cReset + mHead)
		em.text(raw[cut:])
		em.code(cReset)
	default:
		renderInline(raw, em)
	}
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func indexRune(rs []rune, from int, r rune) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

func indexPair(rs []rune, from int, r rune) int {
	for i := from; i+1 < len(rs); i++ {
		if rs[i] == r && rs[i+1] == r {
			return i
		}
	}
	return -1
}

func span(em *ansiEmitter, marker, content, style string) {
	em.code(mGray)
	em.text(marker)
	em.code(cReset + style)
	em.text(content)
	em.code(cReset + mGray)
	em.text(marker)
	em.code(cReset)
}

// renderInline colorizes list/quote markers and `code`/**bold**/__underline__/
// *italic*/_italic_ spans within a normal text line.
func renderInline(line string, em *ansiEmitter) {
	rs := []rune(line)
	i := 0
	for i < len(rs) && rs[i] == ' ' {
		i++
	}
	em.text(string(rs[:i]))
	switch {
	case i < len(rs) && (rs[i] == '-' || rs[i] == '*' || rs[i] == '+') && i+1 < len(rs) && rs[i+1] == ' ':
		em.code(mGray)
		em.text(string(rs[i : i+2]))
		em.code(cReset)
		i += 2
	case i < len(rs) && rs[i] == '>':
		k := i
		for k < len(rs) && (rs[k] == '>' || rs[k] == ' ') {
			k++
		}
		em.code(mGray)
		em.text(string(rs[i:k]))
		em.code(cReset)
		i = k
	default:
		k := i
		for k < len(rs) && rs[k] >= '0' && rs[k] <= '9' {
			k++
		}
		if k > i && k+1 < len(rs) && rs[k] == '.' && rs[k+1] == ' ' {
			em.code(mGray)
			em.text(string(rs[i : k+2]))
			em.code(cReset)
			i = k + 2
		}
	}
	for i < len(rs) {
		r := rs[i]
		switch {
		case r == '`':
			if j := indexRune(rs, i+1, '`'); j >= 0 {
				span(em, "`", string(rs[i+1:j]), mCode)
				i = j + 1
				continue
			}
		case r == '*' && i+1 < len(rs) && rs[i+1] == '*':
			if j := indexPair(rs, i+2, '*'); j > i+2 {
				span(em, "**", string(rs[i+2:j]), mBold)
				i = j + 2
				continue
			}
		case r == '_' && i+1 < len(rs) && rs[i+1] == '_':
			if j := indexPair(rs, i+2, '_'); j > i+2 {
				span(em, "__", string(rs[i+2:j]), mUndr)
				i = j + 2
				continue
			}
		case r == '*':
			if j := indexRune(rs, i+1, '*'); j > i+1 && rs[i+1] != ' ' && rs[j-1] != ' ' {
				span(em, "*", string(rs[i+1:j]), mItal)
				i = j + 1
				continue
			}
		case r == '_':
			prevOK := i == 0 || !isWordRune(rs[i-1])
			if j := indexRune(rs, i+1, '_'); prevOK && j > i+1 && rs[i+1] != ' ' && rs[j-1] != ' ' &&
				(j+1 >= len(rs) || !isWordRune(rs[j+1])) {
				span(em, "_", string(rs[i+1:j]), mItal)
				i = j + 1
				continue
			}
		}
		em.text(string(r))
		i++
	}
}

// renderCode gives fenced-code lines light generic highlighting: comments,
// strings, numbers, and a common keyword set.
func renderCode(line string, em *ansiEmitter) {
	rs := []rune(line)
	i := 0
	for i < len(rs) {
		r := rs[i]
		atBoundary := i == 0 || rs[i-1] == ' ' || rs[i-1] == '\t'
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		if atBoundary && (r == '#' || (r == '/' && next == '/') || (r == '-' && next == '-')) {
			em.code(mGray)
			em.text(string(rs[i:]))
			em.code(cReset)
			return
		}
		if r == '"' || r == '\'' || r == '`' {
			j := i + 1
			for j < len(rs) && rs[j] != r {
				if rs[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(rs) {
				j = len(rs) - 1
			}
			em.code(mStr)
			em.text(string(rs[i : j+1]))
			em.code(cReset)
			i = j + 1
			continue
		}
		if r == '_' || unicode.IsLetter(r) {
			j := i
			for j < len(rs) && isWordRune(rs[j]) {
				j++
			}
			w := string(rs[i:j])
			if _, ok := codeKeywords[w]; ok {
				em.code(mKw)
				em.text(w)
				em.code(cReset)
			} else {
				em.text(w)
			}
			i = j
			continue
		}
		if r >= '0' && r <= '9' {
			j := i
			for j < len(rs) && (isWordRune(rs[j]) || rs[j] == '.') {
				j++
			}
			em.code(mNum)
			em.text(string(rs[i:j]))
			em.code(cReset)
			i = j
			continue
		}
		em.text(string(r))
		i++
	}
}
