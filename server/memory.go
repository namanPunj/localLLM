package server

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
)

// PersonalMemory stores brief user-defined memory items that persist across sessions.
// Items are injected into the system prompt so the model always has context.
type PersonalMemory struct {
	mu       sync.RWMutex
	items    []string
	filePath string
}

func NewPersonalMemory(filePath string) *PersonalMemory {
	pm := &PersonalMemory{filePath: filePath}
	if data, err := os.ReadFile(filePath); err == nil {
		var items []string
		if json.Unmarshal(data, &items) == nil {
			pm.items = items
		}
	}
	return pm
}

// GetItems returns a copy of the memory items (implements chat.MemoryStore).
func (pm *PersonalMemory) GetItems() []string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make([]string, len(pm.items))
	copy(out, pm.items)
	return out
}

func (pm *PersonalMemory) SetItems(items []string) error {
	pm.mu.Lock()
	pm.items = items
	pm.mu.Unlock()
	data, _ := json.MarshalIndent(items, "", "  ")
	return os.WriteFile(pm.filePath, data, 0o644)
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"items": s.memory.GetItems()})

	case http.MethodPost:
		var body struct {
			Items []string `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.memory.SetItems(body.Items); err != nil {
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "items": body.Items})

	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}
