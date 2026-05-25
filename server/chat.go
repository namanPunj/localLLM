package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"assistant/chat"
)

// urlFilenameRe matches hostnames stored when a web page is scraped into workspace files.
// e.g. "github.com", "docs.python.org" — no file extension, looks like a domain.
var urlFilenameRe = regexp.MustCompile(`(?i)^[a-z0-9-]+(\.[a-z0-9-]*)*\.(com|org|net|io|dev|co|ai|app|me|info|edu|gov|xyz|uk|de|fr|in|jp|ru|br|ca|au)$`)

func isLinkFilename(name string) bool {
	return urlFilenameRe.MatchString(name)
}

// handleChat enqueues the request in the global queue, then streams the
// response back to the originating HTTP connection.
func (s *Server) handleChat(terminalMode bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req chat.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Message == "" {
			http.Error(w, "message required", http.StatusBadRequest)
			return
		}

		// Inject runtime settings.
		cfg := s.settings.Get()
		req.RAGSnippets = cfg.RAGSnippets
		req.FileRAGChunks = cfg.FileRAGChunks

		numCtx := 4096
		if req.SessionID != "" {
			if ss, err := s.sessions.GetModelSettings(req.SessionID); err == nil && ss != nil {
				if ss.NumCtx > 0 {
					numCtx = ss.NumCtx
				}
			}
		}
		// Think mode uses the same context size — no automatic upgrade.
		opts := &chat.ChatOptions{NumCtx: numCtx, Think: req.Think}
		req.ModelOverride = opts
		req.MaxCtxTok = numCtx - 800

		if req.SessionID != "" {
			projectFiles, err := s.sessions.GetProjectFiles(req.SessionID)
			if err == nil && len(projectFiles) > 0 {
				for _, pf := range projectFiles {
					req.AgentFiles = append(req.AgentFiles, chat.FileInfo{
						ID:     pf.FileID,
						Name:   pf.Filename,
						IsLink: isLinkFilename(pf.Filename),
					})
				}
				// Workspace mode → use coder model with structured think blocks
				opts.UseCoder = true
				opts.Think = true
				if opts.NumCtx < 35840 {
					opts.NumCtx = 35840
					req.MaxCtxTok = 35840 - 800
				}
			}
		}

		// Large buffer so the worker never blocks even if the HTTP client is slow.
		entry := &queueEntry{
			req:      req,
			termMode: terminalMode,
			events:   make(chan chat.StreamEvent, 2000),
		}
		if !s.queue.Enqueue(entry) {
			http.Error(w, "queue full", http.StatusServiceUnavailable)
			return
		}

		stream, ok := NewStreamer(w)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		for ev := range entry.events {
			_ = stream.Write(ev)
		}
	}
}

// POST /chat/cancel   body: {"session_id":"..."}
// Cancels any in-flight or queued requests for the given session.
func (s *Server) handleChatCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.SessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	s.queue.CancelSession(body.SessionID)
	writeJSON(w, map[string]any{"ok": true})
}

// GET /chat/stream/{sid}
// Replays the in-progress stream for a session and continues forwarding live
// events, allowing clients to reconnect after a page reload.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimPrefix(r.URL.Path, "/chat/stream/")
	if sid == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	sr := s.queue.GetReplay(sid)
	if sr == nil {
		// No active or recent stream for this session.
		writeJSON(w, map[string]any{"active": false})
		return
	}

	stream, ok := NewStreamer(w)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Buffer size matches the broadcast channel size used in subscribe.
	liveCh := make(chan chat.StreamEvent, 256)
	replayed := sr.subscribe(liveCh)

	// Send buffered events first.
	for _, ev := range replayed {
		_ = stream.Write(ev)
		if ev.Type == "done" || ev.Type == "error" {
			sr.unsubscribe(liveCh)
			return
		}
	}

	// Forward live events.
	for ev := range liveCh {
		_ = stream.Write(ev)
		if ev.Type == "done" || ev.Type == "error" {
			break
		}
	}
	sr.unsubscribe(liveCh)
}

// GET /chat/queue/{sid}
// Returns {"active": bool, "queued": int} for the given session.
func (s *Server) handleQueueStatus(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimPrefix(r.URL.Path, "/chat/queue/")
	if sid == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	active, queued := s.queue.Status(sid)
	writeJSON(w, map[string]any{"active": active, "queued": queued})
}
