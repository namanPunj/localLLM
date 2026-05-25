package server

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"assistant/rag"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

// AssistantCalendarName is the display name of the dedicated calendar the
// executor creates on first use. Items the assistant adds go here so they
// don't clutter the user's primary calendar.
const AssistantCalendarName = "Assistant Tasks"

// RAGSearcher is the interface the executor needs for web search and media search.
type RAGSearcher interface {
	Search(ctx context.Context, query string, k int, progress func(string, map[string]any)) ([]string, []string, error)
	SearchImages(ctx context.Context, query string, max int) ([]rag.ImageResult, error)
	SearchVideos(ctx context.Context, query string, max int) ([]rag.VideoResult, error)
	SearchShopping(ctx context.Context, query string, max int) ([]rag.ShopResult, error)
}

// ProgressEmitter is set by the pipeline before each Execute call so that
// long-running tools (web_search) can stream detailed progress to the UI.
type ProgressEmitter func(stage string, data map[string]any)

// ToolExecutor implements chat.ToolExecutor. Routes tool calls to
// Google Calendar (alarms/reminders), web search, and memory.
type ToolExecutor struct {
	CalendarService *calendar.Service
	RAG             RAGSearcher
	Memory          *PersonalMemory // for save_memory tool
	OnProgress      ProgressEmitter // set per-call by pipeline

	mu             sync.Mutex
	calendarIDOnce string // cached calendar ID; "" until resolved
}

func (e *ToolExecutor) SetProgress(fn func(stage string, data map[string]any)) {
	e.OnProgress = fn
}

func (e *ToolExecutor) Execute(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "search_web", "web_search": // web_search kept for backward compat
		query, _ := args["query"].(string)
		switch args["type"] {
		case "image":
			return e.searchImages(ctx, query)
		case "video":
			return e.searchVideos(ctx, query)
		case "shopping":
			return e.searchShopping(ctx, query)
		default:
			return e.webSearch(ctx, query)
		}

	case "add_task":
		title, _ := args["title"].(string)
		dueRaw, _ := args["due_date"].(string)
		notes, _ := args["notes"].(string)
		recurrence, _ := args["recurrence"].(string)
		return e.addTask(ctx, title, dueRaw, notes, recurrence)

	case "set_alarm":
		timeStr, _ := args["time"].(string)
		label, _ := args["label"].(string)
		if label == "" {
			label = "Alarm"
		}
		due, err := resolveAlarmTime(timeStr, time.Now())
		if err != nil {
			return "", fmt.Errorf("set_alarm: %w", err)
		}
		return e.addTask(ctx, label, due.Format(time.RFC3339), "", "")

	case "get_events":
		daysStr, _ := args["days"].(string)
		days := 7
		if n, err := strconv.Atoi(daysStr); err == nil && n > 0 {
			days = n
		}
		return e.getEvents(ctx, days)

	case "delete_event":
		eventID, _ := args["event_id"].(string)
		return e.deleteEvent(ctx, eventID)

	case "save_memory":
		text, _ := args["text"].(string)
		return e.saveMemory(text)

	case "show_notification":
		title, _ := args["title"].(string)
		body, _ := args["body"].(string)
		if title == "" {
			title = "Assistant"
		}
		return fmt.Sprintf("Notification shown to user — title: %s, body: %s", title, body), nil

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// ── Personal memory ───────────────────────────────────────────

func (e *ToolExecutor) saveMemory(text string) (string, error) {
	if text == "" {
		return "", fmt.Errorf("save_memory: text is required")
	}
	if e.Memory == nil {
		return "", fmt.Errorf("save_memory: memory not configured")
	}
	items := e.Memory.GetItems()
	// Check for duplicates (case-insensitive)
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, item := range items {
		if strings.ToLower(strings.TrimSpace(item)) == lower {
			return "Already remembered: " + text, nil
		}
	}
	items = append(items, strings.TrimSpace(text))
	if err := e.Memory.SetItems(items); err != nil {
		return "", fmt.Errorf("save_memory: %w", err)
	}
	return "Saved to memory: " + text, nil
}

// ── Web search ─────────────────────────────────────────────────

func (e *ToolExecutor) webSearch(ctx context.Context, query string) (string, error) {
	if query == "" {
		return "", fmt.Errorf("web_search: query is required")
	}
	if e.RAG == nil {
		return "", fmt.Errorf("web_search: search not configured")
	}

	progress := func(stage string, data map[string]any) {}
	if e.OnProgress != nil {
		progress = func(stage string, data map[string]any) { e.OnProgress(stage, data) }
	}

	snippets, sources, err := e.RAG.Search(ctx, query, 5, progress)
	if err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	if len(snippets) == 0 {
		return "No relevant results found for: " + query, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Web search results for '%s':\n\n", query))
	for i, s := range snippets {
		src := ""
		if i < len(sources) {
			src = sources[i]
		}
		sb.WriteString(fmt.Sprintf("[%d] %s\n%s\n\n", i+1, src, s))
	}
	return sb.String(), nil
}

// ── Media search ───────────────────────────────────────────────

func (e *ToolExecutor) searchImages(ctx context.Context, query string) (string, error) {
	if query == "" {
		return "", fmt.Errorf("search_images: query is required")
	}
	if e.RAG == nil {
		return "", fmt.Errorf("search_images: search not configured")
	}
	results, err := e.RAG.SearchImages(ctx, query, 8)
	if err != nil {
		return "", fmt.Errorf("search_images: %w", err)
	}
	if len(results) == 0 {
		return "No image results found for: " + query, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Image search results for '%s' — embed each using the markdown below:\n\n", query))
	shown := 0
	for _, r := range results {
		// Use only the DDG-cached thumbnail — it's reliably accessible.
		// Direct source URLs (Unsplash, Dreamstime, etc.) can 404 or block hotlinking.
		displayURL := r.ThumbnailURL
		if displayURL == "" {
			continue // no thumbnail, skip to avoid broken images
		}
		shown++
		sb.WriteString(fmt.Sprintf("%d. **%s**\n", shown, r.Title))
		sb.WriteString(fmt.Sprintf("   ![%s](%s)\n", r.Title, displayURL))
		if r.SourceURL != "" {
			sb.WriteString(fmt.Sprintf("   Source: %s\n", r.SourceURL))
		}
		sb.WriteString("\n")
	}
	if shown == 0 {
		return "No image results with usable thumbnails found for: " + query, nil
	}
	sb.WriteString("Copy the ![title](url) lines above verbatim into your response to show the images inline.\n")
	return sb.String(), nil
}

func (e *ToolExecutor) searchVideos(ctx context.Context, query string) (string, error) {
	if query == "" {
		return "", fmt.Errorf("search_videos: query is required")
	}
	if e.RAG == nil {
		return "", fmt.Errorf("search_videos: search not configured")
	}
	results, err := e.RAG.SearchVideos(ctx, query, 8)
	if err != nil {
		return "", fmt.Errorf("search_videos: %w", err)
	}
	if len(results) == 0 {
		return "No video results found for: " + query, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Video search results for '%s':\n\n", query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("[%d] %s\n", i+1, r.Title))
		sb.WriteString(fmt.Sprintf("    URL: %s\n", r.URL))
		if r.Duration != "" {
			sb.WriteString(fmt.Sprintf("    Duration: %s\n", r.Duration))
		}
		if r.Publisher != "" {
			sb.WriteString(fmt.Sprintf("    Publisher: %s\n", r.Publisher))
		}
		if r.Description != "" {
			desc := r.Description
			if len(desc) > 200 {
				desc = desc[:200] + "…"
			}
			sb.WriteString(fmt.Sprintf("    Description: %s\n", desc))
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

func (e *ToolExecutor) searchShopping(ctx context.Context, query string) (string, error) {
	if query == "" {
		return "", fmt.Errorf("search_shopping: query is required")
	}
	if e.RAG == nil {
		return "", fmt.Errorf("search_shopping: search not configured")
	}
	results, err := e.RAG.SearchShopping(ctx, query, 8)
	if err != nil {
		return "", fmt.Errorf("search_shopping: %w", err)
	}
	if len(results) == 0 {
		return "No shopping results found for: " + query, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Shopping results for '%s':\n\n", query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("[%d] %s\n", i+1, r.Title))
		if r.Price != "" {
			price := r.Price
			if r.Currency != "" {
				price += " " + r.Currency
			}
			sb.WriteString(fmt.Sprintf("    Price: %s\n", price))
		}
		if r.Merchant != "" {
			sb.WriteString(fmt.Sprintf("    Merchant: %s\n", r.Merchant))
		}
		sb.WriteString(fmt.Sprintf("    URL: %s\n", r.URL))
		if r.Description != "" {
			desc := r.Description
			if len(desc) > 150 {
				desc = desc[:150] + "…"
			}
			sb.WriteString(fmt.Sprintf("    Description: %s\n", desc))
		}
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// ── Calendar tools ──────────────────────────────────────────────

// addTask creates a Calendar event with an optional RRULE for recurrence.
func (e *ToolExecutor) addTask(ctx context.Context, title, dueRaw, notes, recurrence string) (string, error) {
	if title == "" {
		return "", fmt.Errorf("add_task: title is required")
	}
	if e.CalendarService == nil {
		return "", fmt.Errorf("add_task: calendar service not configured")
	}

	due, err := parseDue(dueRaw)
	if err != nil {
		return "", fmt.Errorf("add_task: %w", err)
	}

	calID, err := e.ensureCalendar(ctx)
	if err != nil {
		return "", fmt.Errorf("add_task: ensure calendar: %w", err)
	}

	tz := resolveIANATimeZone()
	end := due.Add(15 * time.Minute)

	ev := &calendar.Event{
		Summary:     title,
		Description: notes,
		Start:       &calendar.EventDateTime{DateTime: due.Format(time.RFC3339), TimeZone: tz},
		End:         &calendar.EventDateTime{DateTime: end.Format(time.RFC3339), TimeZone: tz},
		Reminders: &calendar.EventReminders{
			UseDefault: false,
			Overrides: []*calendar.EventReminder{
				{Method: "popup", Minutes: 1},
			},
			ForceSendFields: []string{"UseDefault"},
		},
	}

	if recurrence != "" {
		ev.Recurrence = []string{recurrence}
	}

	_, err = e.CalendarService.Events.Insert(calID, ev).Do()
	if err != nil {
		return "", fmt.Errorf("failed to create event: %w", err)
	}

	msg := fmt.Sprintf("Reminder '%s' set for %s", title, due.Format("Jan 2 at 3:04 PM"))
	if recurrence != "" {
		msg += " (recurring)"
	}
	return msg, nil
}

// getEvents returns upcoming calendar events for the next N days.
func (e *ToolExecutor) getEvents(ctx context.Context, days int) (string, error) {
	if e.CalendarService == nil {
		return "", fmt.Errorf("get_events: calendar service not configured")
	}

	calID, err := e.ensureCalendar(ctx)
	if err != nil {
		return "", fmt.Errorf("get_events: ensure calendar: %w", err)
	}

	now := time.Now()
	end := now.AddDate(0, 0, days)

	events, err := e.CalendarService.Events.List(calID).
		TimeMin(now.Format(time.RFC3339)).
		TimeMax(end.Format(time.RFC3339)).
		SingleEvents(true).
		OrderBy("startTime").
		MaxResults(50).
		Do()
	if err != nil {
		return "", fmt.Errorf("list events: %w", err)
	}

	if len(events.Items) == 0 {
		return fmt.Sprintf("No events in the next %d day(s)", days), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Upcoming events** (next %d day%s):\n", days, pluralS(days)))
	for i, ev := range events.Items {
		start := ev.Start.DateTime
		if start == "" {
			start = ev.Start.Date // all-day event
		}
		t, _ := time.Parse(time.RFC3339, start)
		sb.WriteString(fmt.Sprintf("%d. **%s** — %s", i+1, ev.Summary, t.Format("Mon Jan 2 at 3:04 PM")))
		if ev.Description != "" {
			sb.WriteString(fmt.Sprintf(" _%s_", ev.Description))
		}
		sb.WriteString(fmt.Sprintf(" `[id:%s]`\n", ev.Id))
	}
	return sb.String(), nil
}

// deleteEvent removes a calendar event by ID.
func (e *ToolExecutor) deleteEvent(ctx context.Context, eventID string) (string, error) {
	if eventID == "" {
		return "", fmt.Errorf("delete_event: event_id is required")
	}
	if e.CalendarService == nil {
		return "", fmt.Errorf("delete_event: calendar service not configured")
	}

	calID, err := e.ensureCalendar(ctx)
	if err != nil {
		return "", fmt.Errorf("delete_event: ensure calendar: %w", err)
	}

	if err := e.CalendarService.Events.Delete(calID, eventID).Do(); err != nil {
		return "", fmt.Errorf("delete event: %w", err)
	}
	return "Event deleted", nil
}

// ensureCalendar returns the ID of the "Assistant Tasks" calendar, creating
// it on first use.
func (e *ToolExecutor) ensureCalendar(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.calendarIDOnce != "" {
		return e.calendarIDOnce, nil
	}

	list, err := e.CalendarService.CalendarList.List().Do()
	if err != nil {
		return "", fmt.Errorf("list calendars: %w", err)
	}
	for _, item := range list.Items {
		if item.Summary == AssistantCalendarName {
			e.calendarIDOnce = item.Id
			return item.Id, nil
		}
	}

	created, err := e.CalendarService.Calendars.Insert(&calendar.Calendar{
		Summary:     AssistantCalendarName,
		Description: "Auto-created by Assistant for tasks and alarms.",
		TimeZone:    resolveIANATimeZone(),
	}).Do()
	if err != nil {
		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == 409 {
			list, err2 := e.CalendarService.CalendarList.List().Do()
			if err2 == nil {
				for _, item := range list.Items {
					if item.Summary == AssistantCalendarName {
						e.calendarIDOnce = item.Id
						return item.Id, nil
					}
				}
			}
		}
		return "", fmt.Errorf("create calendar: %w", err)
	}
	e.calendarIDOnce = created.Id
	return created.Id, nil
}

// ── Time parsing helpers ────────────────────────────────────────

func parseDue(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultMorning(time.Now()), nil
	}
	if len(s) == 10 {
		if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
			return defaultMorning(t), nil
		}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse due_date %q (expected RFC3339)", s)
	}
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
		return defaultMorning(t), nil
	}
	return t, nil
}

func defaultMorning(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 9, 0, 0, 0, time.Local)
}

func resolveAlarmTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	layouts := []string{"15:04", "3:04pm", "3:04 pm", "3pm", "3 pm", "1504"}
	var t time.Time
	var err error
	for _, l := range layouts {
		if t, err = time.Parse(l, s); err == nil {
			break
		}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse time %q", s)
	}
	out := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !out.After(now) {
		out = out.Add(24 * time.Hour)
	}
	return out, nil
}

// resolveIANATimeZone returns a real IANA timezone name that Google Calendar
// will accept. Resolution: $TZ → /etc/timezone → /etc/localtime → UTC.
func resolveIANATimeZone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if b, err := os.Open("/etc/timezone"); err == nil {
		defer b.Close()
		scanner := bufio.NewScanner(b)
		if scanner.Scan() {
			tz := strings.TrimSpace(scanner.Text())
			if _, err := time.LoadLocation(tz); err == nil {
				return tz
			}
		}
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		const marker = "zoneinfo/"
		if idx := strings.Index(link, marker); idx != -1 {
			tz := link[idx+len(marker):]
			if _, err := time.LoadLocation(tz); err == nil {
				return tz
			}
		}
		if tz := filepath.Base(filepath.Dir(link)) + "/" + filepath.Base(link); strings.Count(tz, "/") == 1 {
			if _, err := time.LoadLocation(tz); err == nil {
				return tz
			}
		}
	}
	return "UTC"
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
