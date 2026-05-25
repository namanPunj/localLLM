# Assistant

A lean local-LLM chat server in Go with a Claude-style web UI.

**What it does**
- Multi-session chat with persistent history (SQLite)
- Web search → scrape → chunk → embed → RAG, on-demand per message
- File upload (`.txt .md .pdf .docx` and most code files) → injected as context
- Tool/function calling (alarms, tasks, etc — bring your own executor)
- Two endpoints:
  - `POST /chat/web` — NDJSON stream for browsers
  - `POST /chat/terminal` — same stream, but tokens are pre-translated from markdown to ANSI for raw terminal output
- Ghost protocol (client-side incognito hiding)

## Prerequisites

1. **Go 1.21+**
2. **Ollama** running locally with two models pulled:
   ```bash
   ollama pull qwen3:4b-instruct-2507-q4_K_M
   ollama pull nomic-embed-text
   ```

## Run

```bash
cd assistant
go mod tidy        # download deps
go run .           # starts on :8080
```

Open http://localhost:8080

## Config (env vars)

| Variable          | Default                                | Purpose                       |
|-------------------|----------------------------------------|-------------------------------|
| `ADDR`            | `:8080`                                | Listen address                |
| `OLLAMA_URL`      | `http://localhost:11434`               | Ollama base URL               |
| `CHAT_MODEL`      | `qwen3:4b-instruct-2507-q4_K_M`        | Main chat model               |
| `EMBED_MODEL`     | `nomic-embed-text`                     | Embedding model for RAG       |
| `DB_PATH`         | `./data/assistant.db`                  | SQLite path                   |
| `UPLOAD_DIR`      | `./data/uploads`                       | Where uploaded files land     |

## API quick reference

### Chat (NDJSON stream)
```
POST /chat/web         # or /chat/terminal
Content-Type: application/json

{
  "session_id": "abc"   // optional; created if missing
  "message":    "hello",
  "search_web": false,
  "file_id":    ""      // optional, from /upload
}
```

Response is a stream of newline-delimited JSON events:
```
{"type":"meta","meta":{"session_id":"…","incognito":false}}
{"type":"meta","meta":{"stage":"search_start","query":"…"}}
{"type":"meta","meta":{"stage":"search_results","count":5,"urls":["…"]}}
{"type":"meta","meta":{"stage":"fetch_start","url":"https://…"}}
{"type":"meta","meta":{"stage":"fetch_done","url":"https://…","chars":4523}}
{"type":"meta","meta":{"stage":"embed_start","chunks":12}}
{"type":"meta","meta":{"stage":"rank_done","selected":5}}
{"type":"token","content":"Hello"}
{"type":"token","content":", how"}
{"type":"action","name":"set_alarm","args":{"time":"19:30","label":"call mom"}}
{"type":"done","meta":{"session_id":"…","actions":1}}
```

Stage events let the UI show "Researching… Reading techcrunch.com…" exactly
like Claude's research panel.

When `incognito: true` is sent, the chat is routed to an in-memory store
that's never persisted to disk and disappears after an hour of inactivity.
It also doesn't appear in `/sessions`.

For `/chat/terminal`, `token` events contain raw text with embedded ANSI escape
codes — pipe straight to a terminal and it renders correctly.

### Sessions
```
GET    /sessions            → { sessions: [{ id, title, created_at, last_active }, …] }
GET    /sessions/{id}       → { session, messages: [{ role, content, created_at }, …] }
DELETE /sessions/{id}
```

### Upload
```
POST /upload   (multipart, field name "file")
→ { file_id, name, size, preview }
```

Pass the returned `file_id` in the next chat request.

## Terminal client (curl)

```bash
curl -N -X POST http://localhost:8080/chat/terminal \
     -H "Content-Type: application/json" \
     -d '{"message":"explain go channels in 3 bullets"}'
```

Because tokens are emitted inside NDJSON, in a real terminal client you'd
read each line and print the `content` field. A 5-line shell wrapper does it:

```bash
curl -sN -X POST http://localhost:8080/chat/terminal \
     -H "Content-Type: application/json" \
     -d '{"message":"hi"}' \
| jq -j 'select(.type=="token") | .content'
```

## Project layout

```
assistant/
├── main.go              entry point
├── config.go            env-based config
├── chat/
│   ├── pipeline.go      orchestrator: build context → call model → stream
│   ├── ollama.go        streaming Ollama HTTP client
│   ├── prompt.go        system prompt + context injection
│   ├── tools.go         function-calling tool definitions
│   └── types.go         shared types
├── sessions/store.go    SQLite-backed session store
├── rag/
│   ├── pipeline.go      full RAG flow
│   ├── search.go        DuckDuckGo HTML search
│   ├── scraper.go       fetch + clean HTML
│   ├── chunker.go       text → chunks
│   ├── embed.go         (interface only; Ollama client implements)
│   └── vector.go        cosine + top-K
├── files/extract.go     extract text from txt/md/pdf/docx
├── terminal/translate.go   streaming markdown → ANSI translator
├── server/
│   ├── server.go        HTTP setup + static files
│   ├── chat.go          /chat/web and /chat/terminal handlers
│   ├── sessions.go      session CRUD
│   ├── upload.go        file upload
│   └── stream.go        NDJSON streaming helper
└── web/
    ├── index.html       structure
    ├── style.css        styles
    └── script.js        UI logic + NDJSON stream parser
```

## Notes on speed

- Ollama is pinned with `keep_alive: 30m` so the model stays loaded between turns.
- The chat and embedding models swap on disk because they don't both fit in 4GB
  VRAM. The swap happens only when `search_web: true`. Plain chat stays hot.
- SQLite is in WAL mode, single writer.
- NDJSON streams flush every event; no buffering.

## Adding more tools

Edit `chat/tools.go` and add an entry to `DefaultTools`. The model will be told
about it automatically and emit `{"type":"action", "name":"...", "args":{...}}`
events when it wants to call it. Your client decides what to do with the call —
e.g. dispatch to the Google Tasks API.
