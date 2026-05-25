package chat

import (
	"fmt"
	"strings"
	"time"
)

// workspaceSystemPrompt is used when the user has project files loaded.
// It replaces the general assistant prompt with a focused coding persona.
const workspaceSystemPrompt = `You are Ed, an expert software engineer acting as a code editor.

TO PROPOSE AN EDIT, use SEARCH/REPLACE blocks — copy the old lines EXACTLY as they appear in the file, then provide the replacement:

exact old lines (copy verbatim — spaces, tabs, and all)

You can include multiple blocks for multiple changes.
After the blocks, briefly state which file was changed and why.

TOOLS (use when you need more file context):
- read_file(file_id, start_line, end_line): Read specific lines from a file.
- ask_followup(question, options): Ask a clarifying question instead of guessing.
- save_memory(text): Save a user preference.

RULES:
- Only use content you can see in context. Never invent or guess file content.
- Smallest possible change per block.
- Reference line numbers and file names in your explanation.`

const systemPrompt = `You are "Ed", a concise daily assistant with access to tools.

TOOLS AVAILABLE:
- search_web(query, type): Search the web. type = "web" (default) | "image" | "video" | "shopping".
- set_alarm(time, label): Set an alarm for today (or tomorrow if time has passed).
- add_task(title, due_date, notes, recurrence): Schedule a reminder/event. Use RFC3339 for due_date.
- get_events(days): List upcoming calendar events.
- delete_event(event_id): Delete a calendar event by ID.
- ask_followup(question, options): Ask a clarifying question with 3–5 selectable options.
- save_memory(text): Persist an important fact about the user across all sessions.

HOW TO RESPOND:
1. Knowledge questions (code, math, explanations) → Answer directly. No tools needed.
2. Current info (news, weather, prices, recent events, facts you are unsure about) → search_web(query, "web").
3. Images requested or useful → ALWAYS call search_web(query, "image") first. NEVER write image URLs from memory.
4. Videos → search_web(query, "video").
5. Shopping/prices → search_web(query, "shopping").
6. Reminders/alarms/events → Calendar tools.
7. Ambiguous requests → ask_followup. Never ask questions as plain text.

IMAGE RULES — READ CAREFULLY:
- NEVER include an image URL you did not receive from a search_web(type="image") tool call in this conversation.
- Every image in your response MUST come from a tool result returned in this turn.
- Copy the exact ![title](url) lines from the tool result into your response verbatim. Do not alter the URLs.
- If the user asks for images and you have not yet called search_web(type="image"), call it now before responding.

IMPORTANT RULES:
- Think in natural language. NEVER output raw JSON or function-call syntax in your thinking.
- Be concise. Cite sources when using web results.
- You may chain multiple tool calls when needed.
- If you are about to type a question mark, call ask_followup instead.`

// estimateTokens gives a rough token count (1 token ≈ 4 chars).
func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// estimateTokensMsg estimates tokens for a slice of Messages.
func estimateTokensMsg(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += estimateTokens(m.Content) + 4 // +4 for role/formatting overhead
	}
	return n
}

// ContextUsage holds the token-count breakdown for a single turn.
type ContextUsage struct {
	System    int `json:"system"`
	History   int `json:"history"`
	Files     int `json:"files"`
	RAG       int `json:"rag"`
	UserTurn  int `json:"user_turn"`
	Available int `json:"available"`
	Limit     int `json:"limit"`
	Compressed int `json:"compressed"` // messages dropped from history
}

// BuildMessagesWithLimit assembles the message list and compresses history
// when the total would exceed maxTokens. It returns the messages and a usage
// breakdown.
func BuildMessagesWithLimit(
	history []Message,
	userMessage string,
	fileContent string,
	ragSnippets []string,
	ragSources []string,
	maxTokens int,
	personalMemory []string,
	systemOverride ...string,
) ([]Message, ContextUsage) {
	sysContent := systemPrompt
	if len(systemOverride) > 0 && systemOverride[0] != "" {
		sysContent = systemOverride[0]
	}

	// Dynamic context message — changes every turn
	now := time.Now()
	var dyn strings.Builder
	dyn.WriteString("Current date and time: ")
	dyn.WriteString(now.Format("Monday, January 2, 2006, 3:04 PM MST"))
	dyn.WriteString(" (")
	dyn.WriteString(now.Format(time.RFC3339))
	dyn.WriteString(")")

	if len(personalMemory) > 0 {
		dyn.WriteString("\n\n--- PERSONAL MEMORY (important context about the user) ---\n")
		for _, m := range personalMemory {
			dyn.WriteString("- ")
			dyn.WriteString(m)
			dyn.WriteString("\n")
		}
		dyn.WriteString("--- END MEMORY ---")
	}

	if fileContent != "" {
		dyn.WriteString("\n\n--- USER FILE ---\n")
		dyn.WriteString(fileContent)
		dyn.WriteString("\n--- END FILE ---")
	}

	if len(ragSnippets) > 0 {
		dyn.WriteString("\n\n--- WEB SEARCH RESULTS ---\n")
		for i, s := range ragSnippets {
			src := ""
			if i < len(ragSources) {
				src = ragSources[i]
			}
			dyn.WriteString(fmt.Sprintf("[%d] %s\n%s\n\n", i+1, src, s))
		}
		dyn.WriteString("--- END SEARCH ---")
	}

	dynContent := dyn.String()
	sysTok := estimateTokens(sysContent)
	dynTok := estimateTokens(dynContent) + 4
	userTok := estimateTokens(userMessage) + 4

	// Estimate file and RAG portions within the dynamic context
	fileTok := estimateTokens(fileContent)
	ragTok := 0
	for _, s := range ragSnippets {
		ragTok += estimateTokens(s) + 10 // source URL overhead
	}
	baseSysTok := sysTok + dynTok - fileTok - ragTok // prompt + time + memory overhead

	// Budget for history
	reserved := sysTok + dynTok + userTok + 200 // 200 tokens buffer for response start + tool JSON
	historyBudget := maxTokens - reserved
	if historyBudget < 0 {
		historyBudget = 0
	}

	// Compress history if needed: keep newest messages that fit
	compressed := 0
	trimmed := history
	histTok := estimateTokensMsg(history)
	if histTok > historyBudget && len(history) > 0 {
		// Walk from newest to oldest, accumulate
		total := 0
		cutoff := len(history)
		for i := len(history) - 1; i >= 0; i-- {
			t := estimateTokens(history[i].Content) + 4
			if total+t > historyBudget {
				break
			}
			total += t
			cutoff = i
		}
		compressed = cutoff
		if cutoff > 0 && cutoff < len(history) {
			notice := Message{
				Role:    "system",
				Content: fmt.Sprintf("[%d earlier messages were removed to fit context window. Conversation continues below.]", cutoff),
			}
			trimmed = append([]Message{notice}, history[cutoff:]...)
		} else if cutoff >= len(history) {
			// Nothing fits — just take the last 2 messages at minimum
			keep := 2
			if keep > len(history) {
				keep = len(history)
			}
			compressed = len(history) - keep
			trimmed = history[len(history)-keep:]
		}
		histTok = estimateTokensMsg(trimmed)
	}

	// Layout: [static system] [dynamic context] [history...] [user message]
	msgs := []Message{
		{Role: "system", Content: sysContent},
		{Role: "system", Content: dynContent},
	}
	msgs = append(msgs, trimmed...)
	msgs = append(msgs, Message{Role: "user", Content: userMessage})

	used := baseSysTok + fileTok + ragTok + histTok + userTok
	avail := maxTokens - used
	if avail < 0 {
		avail = 0
	}

	return msgs, ContextUsage{
		System:     baseSysTok,
		History:    histTok,
		Files:      fileTok,
		RAG:        ragTok,
		UserTurn:   userTok,
		Available:  avail,
		Limit:      maxTokens,
		Compressed: compressed,
	}
}
