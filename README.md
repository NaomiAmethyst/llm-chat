# llm-chat

A minimal terminal chat client for OpenAI-compatible endpoints — OpenRouter,
OpenClaw, Ollama, vLLM, llama.cpp, LM Studio, and anything else that speaks
`/chat/completions`. Single static binary, no dependencies, no tools, no TUI.

Linux only: it drives the terminal through raw termios/ioctl syscalls
directly.

## Build

Requires Go 1.24+:

```sh
go build -o llm-chat .
```

## Usage

```sh
# OpenRouter (the default endpoint)
export OPENROUTER_API_KEY=sk-or-...
./llm-chat -model anthropic/claude-sonnet-4.5

# Any other OpenAI-compatible endpoint
./llm-chat -url http://localhost:11434/v1 -model llama3.1

# System prompt inline or from a file
./llm-chat -system "You are terse."
./llm-chat -system @prompts/reviewer.md

# Scripting: pipe stdin, output goes to stdout (see "Non-interactive mode")
./llm-chat "summarize this file:" < notes.txt
git diff | ./llm-chat -system "write a commit message"
```

### Flags

| flag | default | meaning |
|---|---|---|
| `-url` | OpenRouter, or the OpenClaw gateway (see below) | API base URL (`$LLM_CHAT_BASE_URL`) |
| `-key` | *(env)* | API key; falls back to `$LLM_CHAT_API_KEY`, `$OPENCLAW_GATEWAY_TOKEN` (gateway only), `$OPENROUTER_API_KEY`, `$OPENAI_API_KEY` |
| `-model` | first model listed by the endpoint | model id (`$LLM_CHAT_MODEL`); list with `/models` |
| `-system` | *(built-in)* | system prompt, `@file` to load from a file, or `none` for no system prompt |
| `-temperature` | endpoint default | sampling temperature |
| `-max-tokens` | endpoint default | max completion tokens |
| `-no-stream` | off | disable streaming |
| `-interactive` | stdin is a tty | terminal editor session; `-interactive=false` reads `.`-terminated messages from stdin |
| `-color` | on when interactive | ANSI colors + markdown rendering; `-color=false` disables all decorative ANSI |
| `-load` | *(none)* | load a conversation at startup (JSON save or copied transcript) |

Positional arguments become (or are prepended to) the first user message.

If `-model` and `$LLM_CHAT_MODEL` are both unset, the endpoint's `/models`
list is fetched at startup and the **first model it returns** becomes the
default (a warning is printed if the list can't be fetched — pick one with
`/model`).

**OpenClaw**: if `$OPENCLAW_GATEWAY_TOKEN` is set, the default endpoint
becomes the local OpenClaw gateway (`http://127.0.0.1:18789/v1`) with the
token as the API key — so `export OPENCLAW_GATEWAY_TOKEN=…` followed by a
bare `./llm-chat` just works. The token is only used as a key when talking
to the gateway; an explicit `-url`/`-key` always wins.

### Non-interactive mode

With `-interactive=false` (the default when stdin is not a tty), the terminal
editor is not used. Instead stdin carries one or more messages, each ended by
a line containing only `.` — history is kept between them, so this is a real
multi-turn conversation. EOF sends any pending text as the final message, so
a plain `echo hi | llm-chat` still works. Responses go to stdout with nothing
else added; pass `-color` if you want ANSI markdown highlighting in the
piped output (emitted line-by-line, with no cursor-movement codes).

```sh
printf 'summarize the plan\n.\nnow list the risks\n.\n' | ./llm-chat
```

### Commands

| command | effect |
|---|---|
| `/model [id]` | show or switch the model (mid-conversation is fine) |
| `/models [filter]` | list the endpoint's models, optionally substring-filtered |
| `/system [text\|@file\|clear\|default]` | show, set, load, remove, or restore the system prompt |
| `/clear` | wipe conversation history (keeps the system prompt) |
| `/tokens` | show context size and session token totals |
| `/undo` | remove the last exchange |
| `/retry` | resend the last user message (e.g. after an error or `/model` switch) |
| `/save [file]` | save the conversation — markdown, or JSON if the name ends in `.json` |
| `/load [file]` | load a conversation (JSON save or copied transcript); no file = paste it at the prompt |
| `/quit` | exit (Ctrl+D also works) |

### Input editor

The prompt is a multi-line editor:

- **Enter** inserts a newline; **Ctrl+D** sends the message (on an empty
  prompt it exits). A single-line `/command` runs on Enter directly.
- **Arrow keys** move the cursor, including up/down across lines (and across
  wrapped rows); **Backspace** and **Delete** work as usual.
- **Home/End** (or Ctrl+A/Ctrl+E) jump within the line; Ctrl+K/Ctrl+U kill to
  line end/start; Ctrl+W deletes the previous word; **Ctrl+C clears the
  input**; Ctrl+L redraws the screen.
- **Pasting** multi-line text inserts it literally, ready for editing before
  you send.
- Start a message with `//` to send a literal message beginning with `/`.

Your message is composed on its own line(s) below the `you ❯` marker, with no
continuation prefixes, so finished messages can be copied cleanly from the
scrollback. On submit a status line is printed, and another follows the
response:

```
you ❯
tell me about foo
[sent: 2026-08-13 22:37:05; 33 bytes]

◆ anthropic/claude-sonnet-4.5
...response...
[recv: 2026-08-13 22:37:12; 6.8s; 1,204 in / 87 out; session 5,410 in / 322 out]

────────────────────────────────────────
```

The dim horizontal rule closes each AI turn before the next prompt.

While a response is streaming, Ctrl+C interrupts it without ending the
session; the partial reply stays in history.

### Saving & loading conversations

`/save chat.json` writes the full conversation state — endpoint, model,
system prompt, and every message with its timestamp — as JSON; any other
file name writes a human-readable markdown transcript. Restore a JSON save
with `/load chat.json`, or at startup with `-load chat.json` (works in
non-interactive mode too, so a script can continue a saved conversation).

`/load` also understands a transcript **copied straight from the terminal**:
it rebuilds messages from the `you ❯` / `◆ model` markers and takes
timestamps from the `[sent: …]` / `[recv: …]` lines (banner, rules, and
status noise are ignored; a copied `system ❯` block restores the system
prompt). Run `/load` with no argument to paste a transcript directly at a
`paste ❯` prompt, ending with Ctrl+D. Loading replaces the current history.

The horizontal rule after each AI turn also gives copied transcripts an
unambiguous shape for the parser.

### Markdown rendering

Responses are highlighted with ANSI colors as they stream: headers, **bold**,
*italic*, `__underline__` (rendered underlined), inline `code`, list/quote
markers, and fenced code blocks with light generic syntax highlighting
(comments, strings, numbers, common keywords). All markdown characters are
kept — markers are just drawn in dark gray — so text copied from the terminal
is the model's verbatim output. Piped output is plain by default; pass
`-color` to get the same highlighting line-by-line without cursor codes.

### System prompt & placeholders

The active system prompt (fully expanded) is printed under a `system ❯`
header at the top of every interactive session, so you always see exactly
what the model was given. If no `-system` is given, a generic default is
used: *"You are a helpful AI
chat assistant running in llm-chat, a simple terminal harness. The model
powering you is {model}. This session began on {date} at {time}. …"* —
continuing with an explanation of the per-message timestamp prefixes; plus,
when color is on, a terse description of the markdown
the harness renders; when color is off, a request for plain-text prose; and,
in interactive sessions, a note that a human is typing live at a terminal.
Pass `-system none` (or `/system clear`) for no system prompt at all, and
`/system default` to restore the built default.

Any system prompt (yours included) may use the placeholders `{model}`,
`{endpoint}`, `{date}`, and `{time}`. They are expanded **once** — at session
start and again only when core state changes (`/system`, `/model`, `/load`) —
never per message, so the request prefix stays byte-identical across turns
and remains cacheable by providers with prompt caching. Fresh times come
from the per-message `[YYYY-MM-DD HH:MM:SS]` prefixes instead. `/system`
with no argument shows the raw template and the expansion currently in use.

### Timestamps

Every message sent to the model carries a `[YYYY-MM-DD HH:MM:SS] ` prefix
(applied on the wire only — display, history, and transcripts stay clean).
The default system prompt tells the model about these prefixes and instructs
it not to emit timestamps of its own; if you supply a custom system prompt,
the prefixes are still applied, so mention them yourself if it matters.
The `[sent: …]` / `[recv: …]` status lines show each turn's wall-clock
timestamps (plus message bytes and response duration), and `/save`
transcripts timestamp every message.

### Token counter

The `[recv: …]` line after each response shows that turn's prompt/completion
tokens and the session's running totals, e.g. `1,204 in / 87 out; session
5,410 in / 322 out`. Counts come from the endpoint's reported usage
(`stream_options.include_usage` is requested when streaming); if the endpoint
reports none, a `~`-prefixed estimate (~4 chars/token) is used instead.
`/tokens` shows the current context size and session totals on demand.

### Notes

- The system prompt is fully under your control: nothing is prepended or
  appended to it (per-message timestamp prefixes are the only content the
  harness adds), and no requests are made beyond `/chat/completions` and
  `/models`.
- Reasoning-model "thinking" deltas (OpenRouter's `delta.reasoning`) are shown
  dimmed and are not added to history.
- Do not wrap it in `rlwrap` — the built-in editor needs the terminal in raw
  mode and provides its own line editing.

## License

GPL-3.0 — see [LICENSE](LICENSE).
