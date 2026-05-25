package terminal

import "strings"

// ANSI color codes
const (
	reset    = "\033[0m"
	bold     = "\033[1m"
	italic   = "\033[3m"
	dim      = "\033[2m"
	cyan     = "\033[36m"
	yellow   = "\033[33m"
	green    = "\033[32m"
	magenta  = "\033[35m"
	boldCyan = "\033[1;36m"
)

// Translator turns a stream of markdown tokens into ANSI-colored text for terminals.
// It is stateful: feed it tokens via Push() and consume the translated output it returns.
// Call Flush() at the end to drain any remaining buffered chars.
type Translator struct {
	buf          strings.Builder // pending chars we can't yet decide on
	inBold       bool
	inItalic     bool
	inInlineCode bool
	inCodeBlock  bool
	atLineStart  bool
	headingLvl   int // 0 = not in heading
}

func New() *Translator {
	return &Translator{atLineStart: true}
}

// Push processes a chunk of incoming markdown text and returns the translated output.
// Some characters may be buffered (waiting for disambiguation) until the next call or Flush.
func (t *Translator) Push(s string) string {
	var out strings.Builder
	t.buf.WriteString(s)
	work := t.buf.String()
	t.buf.Reset()

	i := 0
	for i < len(work) {
		c := work[i]

		// Inside a fenced code block, only look for the closing fence.
		if t.inCodeBlock {
			if c == '`' && strings.HasPrefix(work[i:], "```") {
				out.WriteString(reset)
				t.inCodeBlock = false
				i += 3
				// consume optional newline right after ```
				if i < len(work) && work[i] == '\n' {
					out.WriteByte('\n')
					i++
					t.atLineStart = true
				}
				continue
			}
			out.WriteByte(c)
			if c == '\n' {
				t.atLineStart = true
			}
			i++
			continue
		}

		// Need lookahead for `, **, ```
		if c == '`' {
			// If we don't have 3 chars to check for a fence, buffer and wait.
			if i+2 >= len(work) && !t.inCodeBlock {
				t.buf.WriteString(work[i:])
				return out.String()
			}
			// could be ``` (fence) or ` (inline)
			if strings.HasPrefix(work[i:], "```") {
				out.WriteString(yellow)
				t.inCodeBlock = true
				i += 3
				// skip language tag up to newline
				nl := strings.IndexByte(work[i:], '\n')
				if nl >= 0 {
					i += nl + 1
					t.atLineStart = true
				} else {
					// language tag straddles the chunk boundary -- buffer rest
					t.buf.WriteString(work[i:])
					t.inCodeBlock = false // not yet entered for real
					out.Reset()           // safer: undo and wait
					t.buf.Reset()
					t.buf.WriteString(work[i-3:])
					return out.String()
				}
				continue
			}
			// inline code toggle
			if t.inInlineCode {
				out.WriteString(reset)
				t.inInlineCode = false
			} else {
				out.WriteString(yellow)
				t.inInlineCode = true
			}
			i++
			continue
		}

		if c == '*' {
			// need to know if next char is also '*'
			if i+1 >= len(work) {
				t.buf.WriteByte('*')
				i++
				continue
			}
			if work[i+1] == '*' {
				if t.inBold {
					out.WriteString(reset)
					t.inBold = false
					if t.inItalic { // restore italic if still active
						out.WriteString(italic)
					}
				} else {
					out.WriteString(bold)
					t.inBold = true
				}
				i += 2
				continue
			}
			// single *: italic toggle
			if t.inItalic {
				out.WriteString(reset)
				t.inItalic = false
				if t.inBold {
					out.WriteString(bold)
				}
			} else {
				out.WriteString(italic)
				t.inItalic = true
			}
			i++
			continue
		}

		if t.atLineStart && c == '#' {
			// count #s up to 6, then expect space
			lvl := 0
			j := i
			for j < len(work) && work[j] == '#' && lvl < 6 {
				j++
				lvl++
			}
			if j < len(work) && work[j] == ' ' {
				out.WriteString("\n")
				switch lvl {
				case 1:
					out.WriteString(boldCyan)
				case 2:
					out.WriteString(bold + cyan)
				default:
					out.WriteString(bold)
				}
				t.headingLvl = lvl
				i = j + 1
				t.atLineStart = false
				continue
			}
			// not a heading -- emit literal
		}

		if c == '\n' {
			if t.headingLvl > 0 {
				out.WriteString(reset)
				t.headingLvl = 0
			}
			out.WriteByte('\n')
			t.atLineStart = true
			i++
			continue
		}

		// list bullet at line start: "- " or "* " or "• "
		if t.atLineStart && (c == '-' || c == '*') && i+1 < len(work) && work[i+1] == ' ' {
			out.WriteString(magenta + "• " + reset)
			i += 2
			t.atLineStart = false
			continue
		}

		// regular char
		out.WriteByte(c)
		if c != ' ' && c != '\t' {
			t.atLineStart = false
		}
		i++
	}
	return out.String()
}

// Flush returns any buffered chars verbatim and closes open styles.
func (t *Translator) Flush() string {
	var out strings.Builder
	out.WriteString(t.buf.String())
	t.buf.Reset()
	if t.inBold || t.inItalic || t.inInlineCode || t.inCodeBlock || t.headingLvl > 0 {
		out.WriteString(reset)
	}
	return out.String()
}
