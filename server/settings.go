package server

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
)

// RuntimeSettings holds all parameters that can be tuned at runtime via the UI.
type RuntimeSettings struct {
	MaxContextTokens int `json:"max_context_tokens"`
	RAGSnippets      int `json:"rag_snippets"`        // top-k chunks returned from Search (context slots used)
	TargetSources    int `json:"target_sources"`      // number of pages the RAG scraper aims to collect
	MaxSearchResults int `json:"max_search_results"`  // DDG result pool size
	ChunkSize        int `json:"chunk_size"`
	ChunkOverlap     int `json:"chunk_overlap"`
	MaxCharsPerSite  int `json:"max_chars_per_site"`
	FileRAGChunks    int `json:"file_rag_chunks"`     // >0: use semantic file retrieval instead of dumping all content
}

// SettingsStore persists RuntimeSettings to a JSON file and notifies a callback on change.
type SettingsStore struct {
	mu       sync.RWMutex
	cur      RuntimeSettings
	filePath string
	onChange func(RuntimeSettings) // called after each successful update
}

func NewSettingsStore(filePath string, defaults RuntimeSettings, onChange func(RuntimeSettings)) *SettingsStore {
	ss := &SettingsStore{filePath: filePath, cur: defaults, onChange: onChange}
	// Load persisted overrides on top of defaults.
	if data, err := os.ReadFile(filePath); err == nil {
		var saved RuntimeSettings
		if json.Unmarshal(data, &saved) == nil {
			if saved.MaxContextTokens > 0 {
				ss.cur.MaxContextTokens = saved.MaxContextTokens
			}
			if saved.RAGSnippets > 0 {
				ss.cur.RAGSnippets = saved.RAGSnippets
			}
			if saved.TargetSources > 0 {
				ss.cur.TargetSources = saved.TargetSources
			}
			if saved.MaxSearchResults > 0 {
				ss.cur.MaxSearchResults = saved.MaxSearchResults
			}
			if saved.ChunkSize > 0 {
				ss.cur.ChunkSize = saved.ChunkSize
			}
			if saved.ChunkOverlap >= 0 {
				ss.cur.ChunkOverlap = saved.ChunkOverlap
			}
			if saved.MaxCharsPerSite > 0 {
				ss.cur.MaxCharsPerSite = saved.MaxCharsPerSite
			}
			if saved.FileRAGChunks >= 0 {
				ss.cur.FileRAGChunks = saved.FileRAGChunks
			}
		}
	}
	return ss
}

func (ss *SettingsStore) Get() RuntimeSettings {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.cur
}

func (ss *SettingsStore) set(s RuntimeSettings) error {
	ss.mu.Lock()
	ss.cur = s
	ss.mu.Unlock()
	if ss.onChange != nil {
		ss.onChange(s)
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(ss.filePath, data, 0o644)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.settings.Get())

	case http.MethodPost:
		var patch RuntimeSettings
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Merge: use incoming values when provided, keep current otherwise.
		merged := s.settings.Get()
		if patch.MaxContextTokens > 0 {
			merged.MaxContextTokens = patch.MaxContextTokens
		}
		if patch.RAGSnippets > 0 {
			merged.RAGSnippets = patch.RAGSnippets
		}
		if patch.TargetSources > 0 {
			merged.TargetSources = patch.TargetSources
			// Auto-scale search pool: fetch ~1.5x target to handle scrape failures.
			merged.MaxSearchResults = merged.TargetSources + merged.TargetSources/2
		}
		if patch.ChunkSize > 0 {
			merged.ChunkSize = patch.ChunkSize
		}
		if patch.ChunkOverlap > 0 {
			merged.ChunkOverlap = patch.ChunkOverlap
		}
		if patch.MaxCharsPerSite > 0 {
			merged.MaxCharsPerSite = patch.MaxCharsPerSite
		}
		if patch.FileRAGChunks > 0 {
			merged.FileRAGChunks = patch.FileRAGChunks
		}
		// Clamp after merge
		merged.MaxContextTokens = clampInt(merged.MaxContextTokens, 1000, 131072)
		merged.RAGSnippets = clampInt(merged.RAGSnippets, 1, 30)
		merged.TargetSources = clampInt(merged.TargetSources, 2, 10)
		merged.MaxSearchResults = clampInt(merged.MaxSearchResults, merged.TargetSources, 40)
		merged.ChunkSize = clampInt(merged.ChunkSize, 100, 8000)
		merged.ChunkOverlap = clampInt(merged.ChunkOverlap, 0, merged.ChunkSize/2)
		merged.MaxCharsPerSite = clampInt(merged.MaxCharsPerSite, 1000, 500_000)
		merged.FileRAGChunks = clampInt(merged.FileRAGChunks, 0, 50)
		if err := s.settings.set(merged); err != nil {
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "settings": merged})

	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// DELETE /data — wipes all sessions and uploaded files.
func (s *Server) handleDeleteAllData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	if err := s.sessions.DeleteAll(); err != nil {
		http.Error(w, "sessions: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.files.DeleteAll()
	writeJSON(w, map[string]any{"ok": true})
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
