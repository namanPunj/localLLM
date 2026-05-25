package rag

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// FetchClean retrieves a URL and returns cleaned, readable text.
// Removes scripts/styles/nav/footer/aside/ads and collapses whitespace.
func (p *Pipeline) FetchClean(ctx context.Context, pageURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("fetch %s: status %d", pageURL, resp.StatusCode)
	}
	// Skip non-HTML responses (PDF, JSON, etc.)
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") {
		return "", fmt.Errorf("not html: %s", ct)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}

	// Strip junk
	doc.Find("script, style, noscript, iframe, nav, footer, aside, form, header, .ad, .ads, .advert, [aria-hidden=true]").Remove()

	// Prefer main / article content if present
	root := doc.Find("article, main, [role=main]").First()
	if root.Length() == 0 {
		root = doc.Find("body")
	}

	var sb strings.Builder
	root.Find("h1, h2, h3, h4, p, li, pre, code, blockquote").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		if t == "" {
			return
		}
		sb.WriteString(t)
		sb.WriteString("\n\n")
	})

	out := collapseWS(sb.String())
	if len(out) < 100 {
		// fallback to all visible text
		out = collapseWS(root.Text())
	}
	return out, nil
}

func collapseWS(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\r':
			if !prevSpace {
				sb.WriteByte(' ')
				prevSpace = true
			}
		case '\n':
			sb.WriteByte('\n')
			prevSpace = false
		default:
			sb.WriteRune(r)
			prevSpace = false
		}
	}
	// collapse 3+ newlines to 2
	res := sb.String()
	for strings.Contains(res, "\n\n\n") {
		res = strings.ReplaceAll(res, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(res)
}
