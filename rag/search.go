package rag

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type SearchResult struct {
	Title string
	URL   string
	Snip  string
}

// Search_DDG queries DuckDuckGo via the html endpoint first (POST), then falls
// back to the simpler lite endpoint if that returns nothing usable.
func (p *Pipeline) Search_DDG(ctx context.Context, query string, max int) ([]SearchResult, error) {
	if r, err := p.ddgHTML(ctx, query, max); err == nil && len(r) > 0 {
		return r, nil
	}
	return p.ddgLite(ctx, query, max)
}

func (p *Pipeline) ddgHTML(ctx context.Context, query string, max int) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", query)
	form.Set("kl", "us-en")
	form.Set("kp", "-2") // safe-search off

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://duckduckgo.com/")

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ddg html: status %d (%s)", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	var out []SearchResult
	doc.Find(".result, .web-result").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		a := s.Find("a.result__a, .result__title a").First()
		title := strings.TrimSpace(a.Text())
		href, _ := a.Attr("href")
		href = cleanDDGRedirect(href)
		snip := strings.TrimSpace(s.Find(".result__snippet, .result-snippet").Text())
		if title == "" || href == "" || !strings.HasPrefix(href, "http") {
			return true
		}
		out = append(out, SearchResult{Title: title, URL: href, Snip: snip})
		return len(out) < max
	})
	return out, nil
}

func (p *Pipeline) ddgLite(ctx context.Context, query string, max int) ([]SearchResult, error) {
	form := url.Values{}
	form.Set("q", query)
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://lite.duckduckgo.com/lite/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ddg lite: status %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	var out []SearchResult
	doc.Find("a.result-link").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		title := strings.TrimSpace(a.Text())
		href, _ := a.Attr("href")
		href = cleanDDGRedirect(href)
		if title == "" || !strings.HasPrefix(href, "http") {
			return true
		}
		snip := ""
		row := a.Closest("tr")
		next := row.Next()
		if next.Length() > 0 {
			snip = strings.TrimSpace(next.Find(".result-snippet").Text())
			if snip == "" {
				snip = strings.TrimSpace(next.Text())
			}
		}
		out = append(out, SearchResult{Title: title, URL: href, Snip: snip})
		return len(out) < max
	})
	return out, nil
}

// DDG sometimes wraps links: //duckduckgo.com/l/?uddg=<encoded-real-url>
func cleanDDGRedirect(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if real := u.Query().Get("uddg"); real != "" {
		if decoded, err := url.QueryUnescape(real); err == nil {
			return decoded
		}
	}
	return raw
}
