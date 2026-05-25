package server

import (
	"encoding/json"
	"net/http"
)

// POST /model/wake   body: {"num_ctx": 8192}
// Unloads and reloads the model with the requested context size.
func (s *Server) handleModelWake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		NumCtx int `json:"num_ctx"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	numCtx := body.NumCtx
	if numCtx <= 0 {
		numCtx = 4096
	}

	model := s.ollama.ChatModel
	_ = s.ollama.UnloadModel(r.Context(), model)

	if err := s.ollama.PreloadModel(r.Context(), model, numCtx); err != nil {
		http.Error(w, "model wake failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{"ok": true, "num_ctx": numCtx, "model": model})
}

// POST /model/swap   body: {"mode": "think"|"normal"}
// Preloads the appropriate model so the next chat request is instant.
func (s *Server) handleModelSwap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	var model string
	numCtx := 4096
	switch body.Mode {
	case "think":
		model = s.ollama.ChatModel
	case "normal":
		if s.ollama.InstructModel != "" {
			model = s.ollama.InstructModel
		} else {
			model = s.ollama.ChatModel
		}
	case "workspace":
		if s.ollama.CoderModel != "" {
			model = s.ollama.CoderModel
		} else {
			model = s.ollama.ChatModel
		}
		numCtx = 35840
	default:
		http.Error(w, "mode must be 'think', 'normal', or 'workspace'", http.StatusBadRequest)
		return
	}

	if err := s.ollama.PreloadModel(r.Context(), model, numCtx); err != nil {
		http.Error(w, "model swap failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "model": model, "mode": body.Mode, "num_ctx": numCtx})
}
