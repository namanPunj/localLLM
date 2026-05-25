package chat

import (
	"encoding/json"
	"strings"
)

// readFileTool is the tool definition given to the model during agentic file reading.
var readFileTool = tool(
	"read_file",
	"Read a project file with line numbers. "+
		"Use start_line and end_line to read only the section relevant to the user's query — avoid reading the entire file unless necessary. "+
		"You MUST call this before editing any file. Never guess content.",
	map[string]any{
		"file_id":    strProp("The file_id as listed in the project manifest."),
		"start_line": strProp("Optional: first line to read (1-based). Omit to start from line 1."),
		"end_line":   strProp("Optional: last line to read (1-based, inclusive). Omit to read to end of file."),
		"reason":     strProp("What you are looking for in this section."),
	},
	[]string{"file_id"},
)

// editFileTool lets the model propose a targeted replacement in a project file.
var editFileTool = tool(
	"edit_file",
	"Propose a targeted edit to a project file. "+
		"You MUST call read_file first to get the current content. "+
		"Specify ONLY the exact lines that should change (old_content) and what they should become (new_content). "+
		"Be precise — old_content must match the file verbatim. "+
		"The user will see a diff and Accept or Reject the change before it is applied. "+
		"After proposing, wait — do NOT assume the edit was accepted.",
	map[string]any{
		"file_id":     strProp("The file_id of the file to edit, as listed in the project manifest."),
		"description": strProp("One-sentence summary of what this edit does (shown to user in the diff header)."),
		"old_content": strProp("The exact block of text to replace. Must appear verbatim in the file."),
		"new_content": strProp("The new text to replace old_content with."),
	},
	[]string{"file_id", "description", "old_content", "new_content"},
)

// AgentFileTools extends DefaultTools with the read_file and edit_file tools for project sessions.
var AgentFileTools = append(append([]any{}, DefaultTools...), readFileTool, editFileTool)

// WorkspaceTools is a minimal tool set for workspace/coder mode.
// Web search, calendar, and notification tools are excluded — the coder model
// should stay focused on code tasks only.
var WorkspaceTools = []any{
	// Clarifying questions before edits
	tool("ask_followup",
		"Ask the user a clarifying question with selectable options before proceeding. "+
			"Use this when the task is ambiguous or you need more context. "+
			"Provide 3-5 short options. Always use this instead of asking questions as plain text.",
		map[string]any{
			"question": strProp("The clarifying question to display."),
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "3-5 short option labels the user can select.",
			},
		}, []string{"question", "options"}),
	// Persist user preferences across sessions
	tool("save_memory",
		"Save an important preference or fact about the user (e.g. preferred language, coding style) that should persist across all sessions.",
		map[string]any{
			"text": strProp("The fact to save — brief, one sentence."),
		}, []string{"text"}),
	readFileTool,
	editFileTool,
}

// DefaultTools is the set of function-calling tools exposed to the model.
var DefaultTools = []any{
	// ── Unified search ──
	tool("search_web",
		"Search the web. Choose the type based on what the user needs:\n"+
			"  • 'web'      — general web search (news, facts, current info). Default.\n"+
			"  • 'image'    — find images/photos. ALWAYS use this when the user asks for images, "+
			"pictures, or photos, or when you decide to illustrate your answer visually. "+
			"Returns ready-to-embed markdown lines: ![title](url). Copy them verbatim into your response.\n"+
			"  • 'video'    — find videos, tutorials, clips.\n"+
			"  • 'shopping' — find products with prices and merchants.\n"+
			"Start with ONE focused query. Call again with a refined query only if the first result was insufficient.",
		map[string]any{
			"query": strProp("Search query — be specific and concise."),
			"type":  strProp("Search type: 'web' (default), 'image', 'video', or 'shopping'."),
		}, []string{"query"}),

	// ── Personal memory ──
	tool("save_memory",
		"Save an important fact to the user's personal memory that persists across all sessions. "+
			"Use this when the user says 'remember this', 'always remember', or tells you their name, preferences, or important context. "+
			"Also use it proactively when you learn something clearly important about the user (e.g. their name, role, recurring preferences). "+
			"Keep entries brief — one short sentence each.",
		map[string]any{
			"text": strProp("The memory to save — brief, one sentence (e.g. 'User's name is Alex', 'Always verify answers with web search')."),
		}, []string{"text"}),

	// ── Follow-up question ──
	tool("ask_followup",
		"Ask the user a clarifying question with selectable options before proceeding. "+
			"Use this when the user's request is ambiguous, incomplete, or when you need more context. "+
			"Provide 3-5 short options the user can pick from. The user can also type a custom reply.",
		map[string]any{
			"question": strProp("The clarifying question to display."),
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "3-5 short option labels the user can select (e.g. 'Full day', 'Half day', 'Evening only').",
			},
		}, []string{"question", "options"}),

	// ── Calendar tools ──
	tool("set_alarm",
		"Use this tool to set an alarm. "+
			"Set an alarm at a specific wall-clock time today (or tomorrow if the time has already passed). "+
			"Creates a Calendar event with a popup notification on the 'Assistant Tasks' calendar.",
		map[string]any{
			"time":  strProp("Time like '19:30' or '7:30pm'. Wall-clock only — do not include a date."),
			"label": strProp("Short label for the alarm (e.g., 'Take medication'). Defaults to 'Alarm' if omitted."),
		}, []string{"time"}),

	tool("add_task",
		"Use this tool whenever the user asks to schedule something, add a reminder, or set a task with a deadline. "+
			"Creates a Calendar event with a popup notification at the scheduled time. "+
			"If the user gave a specific time, the event fires at that time. "+
			"If only a date is given, the event is anchored at 9:00 AM local on that date.",
		map[string]any{
			"title": strProp("Task or reminder title"),
			"due_date": strProp(
				"RFC3339 timestamp computed from the current system time (provided in the system prompt). " +
					"CRITICAL: Calculate time carefully (base-60). For example, 30 minutes after 15:45 is 16:15, NOT 16:45. " +
					"If the user gave a specific time of day, INCLUDE IT with their local UTC offset (e.g., '2026-05-16T15:30:00-04:00' for UTC-4). " +
					"Use the offset from the system prompt — do NOT assume any particular timezone. " +
					"If only a date was given, send a date-only string like '2026-05-16' — the system " +
					"will anchor it at 9 AM local."),
			"notes": strProp("Optional notes — preserve the user's wording verbatim."),
			"recurrence": strProp(
				"Optional RRULE for recurring events. Examples: " +
					"'RRULE:FREQ=DAILY' (every day), " +
					"'RRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR' (Mon/Wed/Fri), " +
					"'RRULE:FREQ=MONTHLY;BYMONTHDAY=1' (1st of each month), " +
					"'RRULE:FREQ=YEARLY' (every year on that date). " +
					"Leave empty for one-off events."),
		}, []string{"title"}),

	tool("get_events",
		"Retrieve upcoming calendar events. Use this when the user asks what's on their schedule, "+
			"what events are coming up, or what they have planned.",
		map[string]any{
			"days": strProp("Number of days ahead to look (default '7'). Use '1' for today only."),
		}, nil),

	tool("delete_event",
		"Delete a calendar event by its ID. Use get_events first to find the event ID, then delete it.",
		map[string]any{
			"event_id": strProp("The event ID returned by get_events."),
		}, []string{"event_id"}),

	// ── Notification ──
	tool("show_notification",
		"Show a notification to the user. Use this when a routine triggers and you need to alert the user, "+
			"or whenever the user asks you to remind or notify them about something. "+
			"The notification appears as both an in-app toast and a browser notification.",
		map[string]any{
			"title": strProp("Short notification title (e.g. 'Drink Water', 'Latest News')."),
			"body":  strProp("Notification body with the details or summary."),
		}, []string{"title", "body"}),
}

func tool(name, desc string, props map[string]any, required []string) map[string]any {
	params := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		params["required"] = required
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters":  params,
		},
	}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// extractToolCallsFromText recovers structured tool calls when the model
// dumps them as raw JSON text instead of using Ollama's tool_calls field.
func extractToolCallsFromText(text string) ([]ollamaToolCall, string) {
	start := strings.Index(text, "[")
	if start < 0 {
		return nil, text
	}
	end := strings.LastIndex(text, "]")
	if end < 0 || end <= start {
		return nil, text
	}

	candidate := text[start : end+1]

	var raw []struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(candidate), &raw); err != nil {
		return nil, text
	}
	if len(raw) == 0 {
		return nil, text
	}

	known := map[string]bool{
		"search_web": true, "web_search": true, // accept legacy name too
		"ask_followup": true, "set_alarm": true,
		"add_task": true, "get_events": true, "delete_event": true,
		"read_file": true, "edit_file": true, "save_memory": true, "show_notification": true,
	}
	anyKnown := false
	for _, r := range raw {
		if known[r.Name] {
			anyKnown = true
			break
		}
	}
	if !anyKnown {
		return nil, text
	}

	var calls []ollamaToolCall
	for _, r := range raw {
		if r.Name == "" {
			continue
		}
		calls = append(calls, ollamaToolCall{
			Function: struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}{Name: r.Name, Arguments: r.Arguments},
		})
	}

	cleaned := strings.TrimSpace(text[:start] + text[end+1:])
	return calls, cleaned
}
