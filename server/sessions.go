package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"assistant/chat"
	"assistant/sessions"
)

func (s *Server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		// Create an empty session (e.g. for project mode before first message)
		id, _, err := s.sessions.GetOrCreate("")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"session_id": id})
		return
	}
	if r.Method != "GET" {
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}
	list, err := s.sessions.List()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"sessions": list})
}

func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if id == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}

	// Route /sessions/{id}/project and /sessions/{id}/files
	if parts := strings.SplitN(id, "/", 2); len(parts) == 2 {
		sessionID := parts[0]
		sub := parts[1]
		switch {
		case sub == "project":
			s.handleToggleProject(w, r, sessionID)
		case sub == "files":
			s.handleProjectFiles(w, r, sessionID)
		case strings.HasPrefix(sub, "files/"):
			fileID := strings.TrimPrefix(sub, "files/")
			s.handleProjectFileByID(w, r, sessionID, fileID)
		case sub == "title":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Title string `json:"title"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Title == "" {
				http.Error(w, "title required", http.StatusBadRequest)
				return
			}
			if err := s.sessions.SetTitle(sessionID, body.Title); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case sub == "settings":
			s.handleSessionModelSettings(w, r, sessionID)
		case sub == "messages":
			if r.Method != http.MethodDelete {
				http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
				return
			}
			if err := s.sessions.ClearMessages(sessionID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case sub == "compress":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			_, msgs, err := s.sessions.Get(sessionID)
			if err != nil || len(msgs) == 0 {
				http.Error(w, "no messages to compress", http.StatusBadRequest)
				return
			}
			summary, err := s.ollama.Summarize(r.Context(), msgs)
			if err != nil {
				http.Error(w, "summarise error: "+err.Error(), http.StatusInternalServerError)
				return
			}
			compressed := []chat.Message{{Role: "assistant", Content: "[Context summary]\n" + summary}}
			if err := s.sessions.ReplaceHistory(sessionID, compressed); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"ok": true, "summary": summary})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}

	switch r.Method {
	case "GET":
		meta, msgs, err := s.sessions.Get(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		files, _ := s.sessions.GetProjectFiles(id)
		writeJSON(w, map[string]any{"session": meta, "messages": msgs, "project_files": files})
	case "DELETE":
		fileIDs, err := s.sessions.Delete(id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// Clean up uploaded files associated with this session
		for _, fid := range fileIDs {
			s.files.Delete(fid)
		}
		writeJSON(w, map[string]any{"deleted": id})
	default:
		http.Error(w, "GET or DELETE", http.StatusMethodNotAllowed)
	}
}

// PUT /sessions/{id}/project  body: {"is_project": true/false}
func (s *Server) handleToggleProject(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != "PUT" {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		IsProject bool `json:"is_project"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := s.sessions.SetProject(sessionID, body.IsProject); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "is_project": body.IsProject})
}

// POST /sessions/{id}/files  body: {"file_id": "...", "filename": "..."}
// GET  /sessions/{id}/files
func (s *Server) handleProjectFiles(w http.ResponseWriter, r *http.Request, sessionID string) {
	switch r.Method {
	case "GET":
		files, err := s.sessions.GetProjectFiles(sessionID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"files": files})
	case "POST":
		var body struct {
			FileID   string `json:"file_id"`
			Filename string `json:"filename"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := s.sessions.AddProjectFile(sessionID, body.FileID, body.Filename); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// DELETE /sessions/{id}/files/{fileId}
func (s *Server) handleProjectFileByID(w http.ResponseWriter, r *http.Request, sessionID, fileID string) {
	if r.Method != "DELETE" {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	if err := s.sessions.RemoveProjectFile(sessionID, fileID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// GET /sessions/{id}/settings  — return per-session model settings (falls back to global defaults)
// PUT /sessions/{id}/settings  — save per-session model settings
func (s *Server) handleSessionModelSettings(w http.ResponseWriter, r *http.Request, sessionID string) {
	switch r.Method {
	case http.MethodGet:
		ms, err := s.sessions.GetModelSettings(sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ms == nil {
			// No session override — return zeros so the UI knows nothing is pinned
			ms = &sessions.SessionModelSettings{}
		}
		writeJSON(w, ms)

	case http.MethodPut:
		var body sessions.SessionModelSettings
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := s.sessions.SetModelSettings(sessionID, body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "GET or PUT", http.StatusMethodNotAllowed)
	}
}

// GET  /folders           — list all folders with session_ids
// POST /folders           — create folder {name, section}
// PUT  /folders/{id}      — rename {name}
// DELETE /folders/{id}    — delete folder
// POST /folders/{id}/sessions         — add session {session_id}
// DELETE /folders/{id}/sessions/{sid} — remove session from folder
func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/folders")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		switch r.Method {
		case http.MethodGet:
			folders, err := s.sessions.GetFolders()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if folders == nil {
				folders = []sessions.Folder{}
			}
			writeJSON(w, map[string]any{"folders": folders})
		case http.MethodPost:
			var body struct {
				Name    string `json:"name"`
				Section string `json:"section"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
				http.Error(w, "name and section required", http.StatusBadRequest)
				return
			}
			f, err := s.sessions.CreateFolder(body.Name, body.Section)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, f)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
		return
	}

	parts := strings.SplitN(path, "/", 3)
	folderID := parts[0]

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPut:
			var body struct{ Name string `json:"name"` }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
				http.Error(w, "name required", http.StatusBadRequest)
				return
			}
			if err := s.sessions.RenameFolder(folderID, body.Name); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case http.MethodDelete:
			if err := s.sessions.DeleteFolder(folderID); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		default:
			http.Error(w, "PUT or DELETE", http.StatusMethodNotAllowed)
		}
		return
	}

	// /folders/{id}/sessions  or  /folders/{id}/sessions/{session_id}
	if parts[1] == "sessions" {
		if len(parts) == 2 {
			// POST /folders/{id}/sessions
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			var body struct{ SessionID string `json:"session_id"` }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SessionID == "" {
				http.Error(w, "session_id required", http.StatusBadRequest)
				return
			}
			if err := s.sessions.AddSessionToFolder(folderID, body.SessionID); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		} else {
			// DELETE /folders/{id}/sessions/{session_id}
			if r.Method != http.MethodDelete {
				http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
				return
			}
			sessionID := parts[2]
			if err := s.sessions.RemoveSessionFromFolder(sessionID); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		}
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
