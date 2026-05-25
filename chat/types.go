package chat

import "time"

// FileInfo describes a project file available to the model during agentic reading.
type FileInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	IsLink bool   `json:"is_link,omitempty"` // true when the content came from a scraped URL
}

type Message struct {
	Role       string    `json:"role"` // "user", "assistant", "system"
	Content    string    `json:"content"`
	MetaEvents string    `json:"meta_events,omitempty"` // JSON-encoded []map[string]any for blob restoration
	CreatedAt  time.Time `json:"created_at"`
}

type ChatRequest struct {
	SessionID string   `json:"session_id"`
	Message   string   `json:"message"`
	SearchWeb bool     `json:"search_web"`
	Think     bool     `json:"think"`
	FileID    string   `json:"file_id,omitempty"`
	FileIDs   []string `json:"file_ids,omitempty"`
	Incognito bool     `json:"incognito,omitempty"`
	// Server-injected runtime overrides — never read from client JSON.
	MaxCtxTok     int          `json:"-"`
	RAGSnippets   int          `json:"-"`
	FileRAGChunks int          `json:"-"` // >0: use semantic search to pick relevant file chunks
	ModelOverride *ChatOptions `json:"-"` // per-session model/sampling override
	AgentFiles    []FileInfo   `json:"-"` // project files: model reads them on demand via read_file tool
}

// StreamEvent is one line of the NDJSON stream sent to clients.
type StreamEvent struct {
	Type    string         `json:"type"`              // "token" | "action" | "meta" | "done" | "error"
	Content string         `json:"content,omitempty"` // text for "token"
	Name    string         `json:"name,omitempty"`    // action name
	Args    map[string]any `json:"args,omitempty"`    // action args
	Meta    map[string]any `json:"meta,omitempty"`    // metadata (session_id, sources, etc.)
	Error   string         `json:"error,omitempty"`
}

type Action struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}
