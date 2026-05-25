package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// POST /files/edit
// Applies an accepted edit to a file in the store.
// Body: { file_id, old_content, new_content }
// Returns: { ok, new_file_id, filename }
func (s *Server) handleFileEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		FileID     string `json:"file_id"`
		Filename   string `json:"filename"`
		OldContent string `json:"old_content"`
		NewContent string `json:"new_content"`
		SessionID  string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.FileID == "" || body.Filename == "" {
		http.Error(w, "file_id and filename required", http.StatusBadRequest)
		return
	}

	// Get current content
	current, ok := s.files.Get(body.FileID)
	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	// Apply the replacement
	if !strings.Contains(current, body.OldContent) {
		http.Error(w, "old_content not found in file", http.StatusConflict)
		return
	}
	updated := strings.Replace(current, body.OldContent, body.NewContent, 1)

	// Save as new file version (Save returns id, preview, error)
	newID, _, err := s.files.Save(body.Filename, []byte(updated))
	if err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Update the session's project file entry to point at new file ID
	if body.SessionID != "" {
		_ = s.sessions.RemoveProjectFile(body.SessionID, body.FileID)
		_ = s.sessions.AddProjectFile(body.SessionID, newID, body.Filename)
	}

	writeJSON(w, map[string]any{
		"ok":          true,
		"new_file_id": newID,
		"filename":    body.Filename,
	})
}

// GET /files/raw/{file_id}
// Returns raw file content as plain text, so the frontend can display it.
func (s *Server) handleFileRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	fileID := strings.TrimPrefix(r.URL.Path, "/files/raw/")
	if fileID == "" {
		http.Error(w, "file_id required", http.StatusBadRequest)
		return
	}
	content, ok := s.files.Get(fileID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(content))
}
