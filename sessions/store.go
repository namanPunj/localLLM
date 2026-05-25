package sessions

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"assistant/chat"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type SessionMeta struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	CreatedAt  time.Time `json:"created_at"`
	LastActive time.Time `json:"last_active"`
	IsProject  bool      `json:"is_project"`
}

type ProjectFile struct {
	FileID   string `json:"file_id"`
	Filename string `json:"filename"`
}

func NewStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// Migrations (idempotent)
	db.Exec(migrationV2)
	if _, err := db.Exec(projectFilesSchema); err != nil {
		return nil, err
	}
	if _, err := db.Exec(migrationV3); err != nil {
		return nil, err
	}
	db.Exec(migrationV4)      // add thinking column
	db.Exec(migrationV5)      // add meta_events column
	db.Exec(migrationV6sess)  // add think_budget to session_settings
	if _, err := db.Exec(foldersSchema); err != nil {
		return nil, err
	}
	db.Exec(sessionContextSchema) // idempotent
	return &Store{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  last_active INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id);
`

// migrationV2 adds the is_project column. ALTER TABLE will fail silently
// if the column already exists (error is ignored).
const migrationV2 = `ALTER TABLE sessions ADD COLUMN is_project INTEGER NOT NULL DEFAULT 0;`

const projectFilesSchema = `
CREATE TABLE IF NOT EXISTS project_files (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  file_id TEXT NOT NULL,
  filename TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_project_files_session ON project_files(session_id);
`

// migrationV3 adds per-session model settings.
const migrationV3 = `CREATE TABLE IF NOT EXISTS session_settings (
  session_id TEXT PRIMARY KEY,
  model TEXT NOT NULL DEFAULT '',
  temperature REAL NOT NULL DEFAULT 0,
  num_ctx INTEGER NOT NULL DEFAULT 0,
  max_ctx_tok INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);`

// migrationV4 adds thinking column to messages.
const migrationV4 = `ALTER TABLE messages ADD COLUMN thinking TEXT NOT NULL DEFAULT '';`

// migrationV5 adds meta_events column (JSON blob of pipeline stage events for UI restoration).
const migrationV5 = `ALTER TABLE messages ADD COLUMN meta_events TEXT NOT NULL DEFAULT '';`

// migrationV6 adds per-session think_budget to session_settings.
// -2 = inherit global default, 0 = normal mode, >0 or -1 = think mode with that budget.
const migrationV6sess = `ALTER TABLE session_settings ADD COLUMN think_budget INTEGER NOT NULL DEFAULT -2;`

// sessionContextSchema adds persistent context compaction state (migration V6).
const sessionContextSchema = `
CREATE TABLE IF NOT EXISTS session_context (
  session_id TEXT PRIMARY KEY,
  summary TEXT NOT NULL DEFAULT '',
  cursor INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);`

// foldersSchema adds folder support (migration V5).
const foldersSchema = `
CREATE TABLE IF NOT EXISTS folders (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  section TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS folder_sessions (
  folder_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  PRIMARY KEY (folder_id, session_id),
  FOREIGN KEY(folder_id) REFERENCES folders(id) ON DELETE CASCADE,
  FOREIGN KEY(session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_folder_sessions_folder ON folder_sessions(folder_id);
CREATE INDEX IF NOT EXISTS idx_folder_sessions_session ON folder_sessions(session_id);
`

// SessionModelSettings holds per-session model overrides.
// Zero values mean "use global default".
type SessionModelSettings struct {
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	NumCtx      int     `json:"num_ctx"`
	MaxCtxTok   int     `json:"max_ctx_tok"`
	ThinkMode   bool    `json:"think_mode"`
}

// GetOrCreate returns the session id, creating a new row if needed, and returns its history.
func (s *Store) GetOrCreate(id string) (string, []chat.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if id == "" {
		id = uuid.NewString()
	}

	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM sessions WHERE id = ?`, id).Scan(&exists); err != nil {
		return "", nil, err
	}
	if exists == 0 {
		if _, err := s.db.Exec(`INSERT INTO sessions(id, created_at, last_active, is_project) VALUES(?,?,?,0)`, id, now, now); err != nil {
			return "", nil, err
		}
		return id, nil, nil
	}
	if _, err := s.db.Exec(`UPDATE sessions SET last_active = ? WHERE id = ?`, now, id); err != nil {
		return "", nil, err
	}

	rows, err := s.db.Query(`SELECT role, content, meta_events, created_at FROM messages WHERE session_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var msgs []chat.Message
	for rows.Next() {
		var m chat.Message
		var ts int64
		if err := rows.Scan(&m.Role, &m.Content, &m.MetaEvents, &ts); err != nil {
			return "", nil, err
		}
		m.CreatedAt = time.Unix(ts, 0)
		msgs = append(msgs, m)
	}
	return id, msgs, nil
}

func (s *Store) AppendMessage(sessionID string, m chat.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`INSERT INTO messages(session_id, role, content, thinking, meta_events, created_at) VALUES(?,?,?,?,?,?)`,
		sessionID, m.Role, m.Content, "", m.MetaEvents, m.CreatedAt.Unix())
	if err != nil {
		return err
	}
	// auto-title from first user message
	if m.Role == "user" {
		_, _ = s.db.Exec(`UPDATE sessions SET title = ?, last_active = ?
		                  WHERE id = ? AND (title IS NULL OR title = '')`,
			truncate(m.Content, 60), now, sessionID)
	}
	_, _ = s.db.Exec(`UPDATE sessions SET last_active = ? WHERE id = ?`, now, sessionID)
	return nil
}

func (s *Store) List() ([]SessionMeta, error) {
	// Exclude sessions with no messages (orphaned empty sessions from tab reloads),
	// unless they are project/workspace sessions which may have files but no messages yet.
	rows, err := s.db.Query(`
		SELECT id, title, created_at, last_active, is_project FROM sessions
		WHERE is_project = 1
		   OR id IN (SELECT DISTINCT session_id FROM messages)
		ORDER BY last_active DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var c, l int64
		var proj int
		if err := rows.Scan(&m.ID, &m.Title, &c, &l, &proj); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(c, 0)
		m.LastActive = time.Unix(l, 0)
		m.IsProject = proj != 0
		out = append(out, m)
	}
	return out, nil
}

func (s *Store) Get(id string) (SessionMeta, []chat.Message, error) {
	var m SessionMeta
	var c, l int64
	var proj int
	err := s.db.QueryRow(`SELECT id, title, created_at, last_active, is_project FROM sessions WHERE id = ?`, id).
		Scan(&m.ID, &m.Title, &c, &l, &proj)
	if err == sql.ErrNoRows {
		return m, nil, fmt.Errorf("session not found")
	}
	if err != nil {
		return m, nil, err
	}
	m.CreatedAt = time.Unix(c, 0)
	m.LastActive = time.Unix(l, 0)
	m.IsProject = proj != 0

	rows, err := s.db.Query(`SELECT role, content, meta_events, created_at FROM messages WHERE session_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return m, nil, err
	}
	defer rows.Close()
	var msgs []chat.Message
	for rows.Next() {
		var msg chat.Message
		var ts int64
		if err := rows.Scan(&msg.Role, &msg.Content, &msg.MetaEvents, &ts); err != nil {
			return m, nil, err
		}
		msg.CreatedAt = time.Unix(ts, 0)
		msgs = append(msgs, msg)
	}
	return m, msgs, nil
}

// Delete removes a session and returns the file IDs that were associated with it
// (so the caller can clean up files on disk).
func (s *Store) Delete(id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Collect project file IDs before deletion (CASCADE will remove them)
	var fileIDs []string
	rows, err := s.db.Query(`SELECT file_id FROM project_files WHERE session_id = ?`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var fid string
			if rows.Scan(&fid) == nil {
				fileIDs = append(fileIDs, fid)
			}
		}
	}
	_, err = s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return fileIDs, err
}

// DeleteAll removes every session and folder (cascades to all child rows).
func (s *Store) DeleteAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM folders`)
	return err
}

// SetTitle updates the display title for a session.
func (s *Store) SetTitle(id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, title, id)
	return err
}

// ClearMessages deletes all messages for a session without deleting the session itself.
func (s *Store) ClearMessages(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID)
	return err
}

// ReplaceHistory atomically replaces all messages for a session with msgs.
// Used by the rolling-summary compressor to persist condensed history.
func (s *Store) ReplaceHistory(sessionID string, msgs []chat.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		tx.Rollback()
		return err
	}
	for _, m := range msgs {
		if m.CreatedAt.IsZero() {
			m.CreatedAt = time.Now()
		}
		if _, err := tx.Exec(
			`INSERT INTO messages(session_id, role, content, thinking, meta_events, created_at) VALUES(?,?,?,?,?,?)`,
			sessionID, m.Role, m.Content, "", m.MetaEvents, m.CreatedAt.Unix(),
		); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ── Project methods ──

func (s *Store) SetProject(id string, isProject bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := 0
	if isProject {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE sessions SET is_project = ? WHERE id = ?`, v, id)
	return err
}

func (s *Store) AddProjectFile(sessionID, fileID, filename string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Skip duplicates — same file_id in the same session
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM project_files WHERE session_id = ? AND file_id = ?`,
		sessionID, fileID).Scan(&exists); err == nil && exists > 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO project_files(session_id, file_id, filename, created_at) VALUES(?,?,?,?)`,
		sessionID, fileID, filename, time.Now().Unix())
	return err
}

func (s *Store) GetProjectFiles(sessionID string) ([]ProjectFile, error) {
	rows, err := s.db.Query(`SELECT file_id, filename FROM project_files WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectFile
	for rows.Next() {
		var pf ProjectFile
		if err := rows.Scan(&pf.FileID, &pf.Filename); err != nil {
			return nil, err
		}
		out = append(out, pf)
	}
	return out, nil
}

func (s *Store) RemoveProjectFile(sessionID, fileID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM project_files WHERE session_id = ? AND file_id = ?`, sessionID, fileID)
	return err
}

// IsProject returns true if the session has the project flag set.
func (s *Store) IsProject(sessionID string) (bool, error) {
	var v int
	err := s.db.QueryRow(`SELECT is_project FROM sessions WHERE id = ?`, sessionID).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return v != 0, err
}

// GetModelSettings returns the per-session model settings, or nil if none are saved.
func (s *Store) GetModelSettings(sessionID string) (*SessionModelSettings, error) {
	var m SessionModelSettings
	var thinkBudget int
	err := s.db.QueryRow(
		`SELECT model, temperature, num_ctx, max_ctx_tok, think_budget FROM session_settings WHERE session_id = ?`,
		sessionID,
	).Scan(&m.Model, &m.Temperature, &m.NumCtx, &m.MaxCtxTok, &thinkBudget)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// think_budget: -2 = inherit, 0 = normal, >0 or -1 = think mode
	m.ThinkMode = thinkBudget != 0 && thinkBudget != -2
	return &m, nil
}

// SetModelSettings saves (or replaces) per-session model settings.
func (s *Store) SetModelSettings(sessionID string, m SessionModelSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	thinkBudget := 0 // normal mode
	if m.ThinkMode {
		thinkBudget = -1 // think mode (unlimited budget)
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO session_settings(session_id, model, temperature, num_ctx, max_ctx_tok, think_budget)
		 VALUES(?,?,?,?,?,?)`,
		sessionID, m.Model, m.Temperature, m.NumCtx, m.MaxCtxTok, thinkBudget,
	)
	return err
}

// GetContextSummary returns the stored rolling summary and cursor for a session.
// Returns empty values (not an error) if no summary exists yet.
func (s *Store) GetContextSummary(sessionID string) (summary string, cursor int, err error) {
	err = s.db.QueryRow(`SELECT summary, cursor FROM session_context WHERE session_id = ?`, sessionID).
		Scan(&summary, &cursor)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return
}

// SetContextSummary persists the rolling summary and cursor for a session.
func (s *Store) SetContextSummary(sessionID string, summary string, cursor int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO session_context(session_id, summary, cursor, updated_at) VALUES(?,?,?,?)`,
		sessionID, summary, cursor, time.Now().Unix(),
	)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── Folder methods ──

type Folder struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Section    string    `json:"section"` // "workspace" or "chat"
	SessionIDs []string  `json:"session_ids"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Store) CreateFolder(name, section string) (Folder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.NewString()
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO folders(id, name, section, created_at) VALUES(?,?,?,?)`, id, name, section, now)
	if err != nil {
		return Folder{}, err
	}
	return Folder{ID: id, Name: name, Section: section, SessionIDs: []string{}, CreatedAt: time.Unix(now, 0)}, nil
}

func (s *Store) GetFolders() ([]Folder, error) {
	rows, err := s.db.Query(`SELECT id, name, section, created_at FROM folders ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		var ts int64
		if err := rows.Scan(&f.ID, &f.Name, &f.Section, &ts); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(ts, 0)
		f.SessionIDs = []string{}
		out = append(out, f)
	}
	// Attach session IDs to each folder
	for i := range out {
		srows, err := s.db.Query(`SELECT session_id FROM folder_sessions WHERE folder_id = ?`, out[i].ID)
		if err != nil {
			continue
		}
		for srows.Next() {
			var sid string
			if srows.Scan(&sid) == nil {
				out[i].SessionIDs = append(out[i].SessionIDs, sid)
			}
		}
		srows.Close()
	}
	return out, nil
}

func (s *Store) RenameFolder(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE folders SET name = ? WHERE id = ?`, name, id)
	return err
}

func (s *Store) DeleteFolder(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM folders WHERE id = ?`, id)
	return err
}

func (s *Store) AddSessionToFolder(folderID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Remove session from any other folder first (a session can only be in one folder)
	_, _ = s.db.Exec(`DELETE FROM folder_sessions WHERE session_id = ?`, sessionID)
	_, err := s.db.Exec(`INSERT OR IGNORE INTO folder_sessions(folder_id, session_id) VALUES(?,?)`, folderID, sessionID)
	return err
}

func (s *Store) RemoveSessionFromFolder(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM folder_sessions WHERE session_id = ?`, sessionID)
	return err
}
