package rag

import "strings"

// Chunk splits text into pieces of approximately targetChars characters
// with overlapChars of overlap. We split on paragraph then sentence boundaries
// to avoid cutting mid-thought.
func Chunk(text string, targetChars, overlapChars int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if len(text) <= targetChars {
		return []string{text}
	}

	var chunks []string
	paras := strings.Split(text, "\n\n")
	var cur strings.Builder
	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if cur.Len()+len(p)+2 > targetChars && cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			// start next chunk with overlap from end of previous
			prev := cur.String()
			cur.Reset()
			if overlapChars > 0 && len(prev) > overlapChars {
				cur.WriteString(prev[len(prev)-overlapChars:])
				cur.WriteString("\n\n")
			}
		}
		cur.WriteString(p)
		cur.WriteString("\n\n")
	}
	if cur.Len() > 0 {
		chunks = append(chunks, strings.TrimSpace(cur.String()))
	}
	return chunks
}
