package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"assistant/chat"
)

// Routine represents a scheduled routine stored on disk.
type Routine struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Prompt        string   `json:"prompt"`
	Freq          string   `json:"freq"`          // "recurring" | "once"
	Repeat        string   `json:"repeat"`        // "daily" | "weekly" | "monthly" | "annually"
	DailyMode     string   `json:"dailyMode"`     // "repeat_after" | "custom_times"
	RepeatHours   int      `json:"repeatHours"`
	RepeatStart   string   `json:"repeatStart"`
	RepeatEnd     string   `json:"repeatEnd"`
	CustomTimes   []string `json:"customTimes"`
	WeekDays      []int    `json:"weekDays"`
	WeeklyTime    string   `json:"weeklyTime"`
	MonthDay      int      `json:"monthDay"`
	MonthlyTime   string   `json:"monthlyTime"`
	AnnualDate    string   `json:"annualDate"`
	AnnualTime    string   `json:"annualTime"`
	OnceDate      string   `json:"onceDate"`
	OnceTime      string   `json:"onceTime"`
	Paused        bool     `json:"paused"`
	LastTriggered int64    `json:"lastTriggered"`
	LastStatus    string   `json:"lastStatus"` // "ok" | "failed" | "skipped"
}

// RoutineNotification is a result from a background routine run, ready for the frontend to poll.
type RoutineNotification struct {
	ID        string `json:"id"`
	RoutineID string `json:"routine_id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Timestamp int64  `json:"timestamp"`
}

// RoutineStore persists routines to a JSON file and runs a background scheduler.
type RoutineStore struct {
	mu       sync.RWMutex
	routines []Routine
	filePath string

	// Pending notifications for the frontend to poll.
	notifMu sync.Mutex
	notifs  []RoutineNotification

	// Web push (set by server.New).
	push *PushStore
}

func NewRoutineStore(filePath string) *RoutineStore {
	rs := &RoutineStore{filePath: filePath}
	if data, err := os.ReadFile(filePath); err == nil {
		var routines []Routine
		if json.Unmarshal(data, &routines) == nil {
			rs.routines = routines
		}
	}
	return rs
}

func (rs *RoutineStore) save() {
	data, _ := json.MarshalIndent(rs.routines, "", "  ")
	_ = os.WriteFile(rs.filePath, data, 0o644)
}

func (rs *RoutineStore) GetAll() []Routine {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	out := make([]Routine, len(rs.routines))
	copy(out, rs.routines)
	return out
}

func (rs *RoutineStore) SetAll(routines []Routine) {
	rs.mu.Lock()
	rs.routines = routines
	rs.save()
	rs.mu.Unlock()
}

func (rs *RoutineStore) Add(r Routine) {
	rs.mu.Lock()
	rs.routines = append(rs.routines, r)
	rs.save()
	rs.mu.Unlock()
}

func (rs *RoutineStore) Delete(id string) {
	rs.mu.Lock()
	for i, r := range rs.routines {
		if r.ID == id {
			rs.routines = append(rs.routines[:i], rs.routines[i+1:]...)
			break
		}
	}
	rs.save()
	rs.mu.Unlock()
}

func (rs *RoutineStore) Update(id string, fn func(*Routine)) {
	rs.mu.Lock()
	for i := range rs.routines {
		if rs.routines[i].ID == id {
			fn(&rs.routines[i])
			break
		}
	}
	rs.save()
	rs.mu.Unlock()
}

func (rs *RoutineStore) PushNotification(n RoutineNotification) {
	rs.notifMu.Lock()
	rs.notifs = append(rs.notifs, n)
	// Keep at most 50 pending
	if len(rs.notifs) > 50 {
		rs.notifs = rs.notifs[len(rs.notifs)-50:]
	}
	rs.notifMu.Unlock()
}

func (rs *RoutineStore) DrainNotifications() []RoutineNotification {
	rs.notifMu.Lock()
	out := rs.notifs
	rs.notifs = nil
	rs.notifMu.Unlock()
	return out
}

// parseHHMM parses "HH:MM" and returns hour, minute.
func parseHHMM(s string) (int, int) {
	if s == "" {
		return 0, 0
	}
	parts := strings.SplitN(s, ":", 2)
	h, m := 0, 0
	if len(parts) >= 1 {
		for _, c := range parts[0] {
			h = h*10 + int(c-'0')
		}
	}
	if len(parts) >= 2 {
		for _, c := range parts[1] {
			m = m*10 + int(c-'0')
		}
	}
	return h, m
}

// shouldFire checks whether a routine should fire right now.
func shouldFire(r *Routine, now time.Time) bool {
	if r.Paused {
		return false
	}
	nowMs := now.UnixMilli()

	withinWindow := func(targetH, targetM int) bool {
		target := time.Date(now.Year(), now.Month(), now.Day(), targetH, targetM, 0, 0, now.Location())
		diff := nowMs - target.UnixMilli()
		if diff < 0 {
			diff = -diff
		}
		return diff < 60000 && nowMs-r.LastTriggered > 3600000
	}

	if r.Freq == "once" {
		if r.OnceDate == "" {
			return false
		}
		t, err := time.ParseInLocation("2006-01-02", r.OnceDate, now.Location())
		if err != nil {
			return false
		}
		h, m := parseHHMM(r.OnceTime)
		t = t.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
		diff := nowMs - t.UnixMilli()
		if diff < 0 {
			diff = -diff
		}
		return diff < 60000 && nowMs-r.LastTriggered > 3600000
	}

	switch r.Repeat {
	case "annually":
		if r.AnnualDate == "" {
			return false
		}
		t, err := time.ParseInLocation("2006-01-02", r.AnnualDate, now.Location())
		if err != nil {
			return false
		}
		if now.Month() == t.Month() && now.Day() == t.Day() {
			h, m := parseHHMM(r.AnnualTime)
			return withinWindow(h, m)
		}
	case "monthly":
		if now.Day() == r.MonthDay || (r.MonthDay == 0 && now.Day() == 1) {
			h, m := parseHHMM(r.MonthlyTime)
			return withinWindow(h, m)
		}
	case "weekly":
		for _, d := range r.WeekDays {
			if int(now.Weekday()) == d {
				h, m := parseHHMM(r.WeeklyTime)
				return withinWindow(h, m)
			}
		}
	case "daily":
		if r.DailyMode == "repeat_after" {
			sh, sm := parseHHMM(r.RepeatStart)
			eh, em := parseHHMM(r.RepeatEnd)
			startMin := sh*60 + sm
			endMin := eh*60 + em
			nowMin := now.Hour()*60 + now.Minute()
			if nowMin >= startMin && nowMin <= endMin {
				hours := r.RepeatHours
				if hours <= 0 {
					hours = 2
				}
				ms := int64(hours) * 3600000
				if r.LastTriggered == 0 {
					// First run: fire at start time
					return withinWindow(sh, sm)
				}
				return nowMs-r.LastTriggered >= ms
			}
		} else if r.DailyMode == "custom_times" {
			for _, t := range r.CustomTimes {
				h, m := parseHHMM(t)
				if withinWindow(h, m) {
					return true
				}
			}
		}
	}
	return false
}

// isExpired returns true if a one-time routine's date has passed.
func isExpired(r *Routine, now time.Time) bool {
	if r.Freq != "once" {
		return false
	}
	if r.OnceDate == "" {
		return true
	}
	t, err := time.ParseInLocation("2006-01-02", r.OnceDate, now.Location())
	if err != nil {
		return true
	}
	h, m := parseHHMM(r.OnceTime)
	t = t.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	return now.Sub(t) > 2*time.Minute
}

// RunScheduler starts a goroutine that checks routines every 30 seconds.
// When a routine fires, it enqueues a background model call and collects
// the show_notification result, then fires an OS notification.
func (rs *RoutineStore) RunScheduler(queue *GlobalQueue, pipeline *chat.Pipeline, settings *SettingsStore) {
	go func() {
		for {
			time.Sleep(30 * time.Second)
			rs.tick(queue, pipeline, settings)
		}
	}()
}

func (rs *RoutineStore) tick(queue *GlobalQueue, pipeline *chat.Pipeline, settings *SettingsStore) {
	rs.mu.Lock()
	now := time.Now()

	// Prune expired one-time routines.
	cleaned := rs.routines[:0]
	for _, r := range rs.routines {
		if !isExpired(&r, now) {
			cleaned = append(cleaned, r)
		}
	}
	rs.routines = cleaned

	// Find routines that should fire.
	var toFire []int
	for i := range rs.routines {
		if shouldFire(&rs.routines[i], now) {
			log.Printf("[routines] firing: %s (%s)", rs.routines[i].Name, rs.routines[i].ID)
			toFire = append(toFire, i)
			rs.routines[i].LastTriggered = now.UnixMilli()
		}
	}
	// Mark expired once-routines that just fired for removal next tick.

	rs.save()
	// Copy routines to fire so we can release the lock.
	var fireCopies []Routine
	for _, i := range toFire {
		fireCopies = append(fireCopies, rs.routines[i])
	}
	rs.mu.Unlock()

	// Fire each routine in background (sequentially — model is single-threaded).
	for _, r := range fireCopies {
		rs.fireRoutine(r, queue, pipeline, settings)
	}
}

func (rs *RoutineStore) fireRoutine(r Routine, queue *GlobalQueue, pipeline *chat.Pipeline, settings *SettingsStore) {
	userPrompt := r.Prompt
	if userPrompt == "" {
		userPrompt = r.Name
	}
	prompt := "[ROUTINE] This message is from an automated routine, NOT a live user. " +
		"You MUST call show_notification(title, body) with your response. " +
		"Do NOT reply with plain text — the user cannot see text responses from routines. " +
		"The ONLY way to reach the user is through show_notification.\n\n" +
		"Routine task: " + userPrompt

	// Run the model in background via the existing queue.
	entry := &queueEntry{
		req: chat.ChatRequest{
			SessionID: "routine_" + r.ID,
			Message:   prompt,
			Incognito: true, // don't persist routine chats
		},
		events: make(chan chat.StreamEvent, 2000),
	}

	// Inject settings.
	cfg := settings.Get()
	entry.req.RAGSnippets = cfg.RAGSnippets
	entry.req.FileRAGChunks = cfg.FileRAGChunks
	opts := &chat.ChatOptions{NumCtx: 4096}
	entry.req.ModelOverride = opts
	entry.req.MaxCtxTok = 4096 - 800

	if !queue.Enqueue(entry) {
		log.Printf("[routines] queue full, failed to enqueue: %s", r.Name)
		rs.updateStatus(r.ID, "failed")
		return
	}
	log.Printf("[routines] enqueued model call for: %s", r.Name)

	// Only notify if the model explicitly calls show_notification.
	var notifTitle, notifBody string
	var gotNotif bool

	for ev := range entry.events {
		if ev.Type == "action" && ev.Name == "show_notification" {
			notifTitle, _ = ev.Args["title"].(string)
			notifBody, _ = ev.Args["body"].(string)
			gotNotif = true
		}
		if ev.Type == "error" {
			rs.updateStatus(r.ID, "failed")
			return
		}
	}

	rs.updateStatus(r.ID, "ok")

	if !gotNotif {
		log.Printf("[routines] %s: model did not call show_notification, skipping", r.Name)
		return
	}

	// Store for frontend polling.
	rs.PushNotification(RoutineNotification{
		ID:        r.ID + "_" + time.Now().Format("150405"),
		RoutineID: r.ID,
		Title:     notifTitle,
		Body:      notifBody,
		Timestamp: time.Now().UnixMilli(),
	})

	// Fire OS-level notification (desktop).
	osNotify(notifTitle, notifBody)

	// Send web push notification (mobile + laptop browsers, even when closed).
	if rs.push != nil {
		log.Printf("[routines] sending web push: %s — %s", notifTitle, notifBody)
		go rs.push.SendAll(notifTitle, notifBody)
	} else {
		log.Printf("[routines] push store is nil, skipping web push")
	}
}

func (rs *RoutineStore) updateStatus(id, status string) {
	rs.mu.Lock()
	for i := range rs.routines {
		if rs.routines[i].ID == id {
			rs.routines[i].LastStatus = status
			break
		}
	}
	rs.save()
	rs.mu.Unlock()
}

// osNotify sends a desktop notification via notify-send (Linux). Fails silently.
func osNotify(title, body string) {
	_ = exec.Command("notify-send", "--app-name=Assistant", title, body).Run()
}

// ── HTTP handlers ────────────────────────────────────────────

func (s *Server) handleRoutines(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.routines.GetAll())

	case http.MethodPost:
		var routine Routine
		if err := json.NewDecoder(r.Body).Decode(&routine); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if routine.ID == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		s.routines.Add(routine)
		writeJSON(w, map[string]any{"ok": true})

	case http.MethodPut:
		var routines []Routine
		if err := json.NewDecoder(r.Body).Decode(&routines); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.routines.SetAll(routines)
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "GET, POST, or PUT", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRoutineByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/routines/")
	if id == "notifications" {
		s.handleRoutineNotifications(w, r)
		return
	}
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		s.routines.Delete(id)
		writeJSON(w, map[string]any{"ok": true})

	case http.MethodPatch:
		var body struct {
			Paused *bool `json:"paused"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if body.Paused != nil {
			s.routines.Update(id, func(r *Routine) {
				r.Paused = *body.Paused
			})
		}
		writeJSON(w, map[string]any{"ok": true})

	default:
		http.Error(w, "DELETE or PATCH", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRoutineNotifications(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	notifs := s.routines.DrainNotifications()
	if notifs == nil {
		notifs = []RoutineNotification{}
	}
	writeJSON(w, notifs)
}
