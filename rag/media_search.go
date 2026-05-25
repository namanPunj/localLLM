package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// ── Result types ──────────────────────────────────────────────

type ImageResult struct {
	Title        string
	ImageURL     string // direct hotlink to full image
	ThumbnailURL string // DDG-cached thumbnail (smaller, loads faster)
	SourceURL    string // page the image is on
	Width        int
	Height       int
}

type VideoResult struct {
	Title       string
	URL         string
	Thumbnail   string
	Duration    string
	Publisher   string
	Description string
}

type ShopResult struct {
	Title       string
	Price       string
	Currency    string
	URL         string
	Image       string
	Merchant    string
	Description string
}

// ── VQD token ─────────────────────────────────────────────────

var vqdRe = regexp.MustCompile(`vqd[=:]['"]?([0-9\-]+)['"]?`)

// getVQD fetches the DDG home page for the given query and extracts the
// short-lived vqd token required by the media-search API endpoints.
func (p *Pipeline) getVQD(ctx context.Context, query string) (string, error) {
	u := "https://duckduckgo.com/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return "", err
	}

	m := vqdRe.FindSubmatch(body)
	if len(m) < 2 {
		return "", fmt.Errorf("vqd token not found in DDG response")
	}
	return string(m[1]), nil
}

// ── Images ────────────────────────────────────────────────────

// SearchImages queries DuckDuckGo's image index and returns up to max results.
func (p *Pipeline) SearchImages(ctx context.Context, query string, max int) ([]ImageResult, error) {
	vqd, err := p.getVQD(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("images vqd: %w", err)
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("vqd", vqd)
	params.Set("o", "json")
	params.Set("l", "us-en")
	params.Set("f", ",,,,,")
	params.Set("p", "1")

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://duckduckgo.com/i.js?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Referer", "https://duckduckgo.com/")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("images: status %d", resp.StatusCode)
	}

	var raw struct {
		Results []struct {
			Title     string `json:"title"`
			Image     string `json:"image"`
			Thumbnail string `json:"thumbnail"`
			URL       string `json:"url"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("images decode: %w", err)
	}

	var out []ImageResult
	for _, r := range raw.Results {
		if r.Image == "" {
			continue
		}
		out = append(out, ImageResult{
			Title:        r.Title,
			ImageURL:     r.Image,
			ThumbnailURL: r.Thumbnail,
			SourceURL:    r.URL,
			Width:        r.Width,
			Height:       r.Height,
		})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// ── Videos ────────────────────────────────────────────────────

// SearchVideos queries DuckDuckGo's video index and returns up to max results.
func (p *Pipeline) SearchVideos(ctx context.Context, query string, max int) ([]VideoResult, error) {
	vqd, err := p.getVQD(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("videos vqd: %w", err)
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("vqd", vqd)
	params.Set("o", "json")
	params.Set("l", "us-en")
	params.Set("f", ",,,,,")

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://duckduckgo.com/v.js?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Referer", "https://duckduckgo.com/")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("videos: status %d", resp.StatusCode)
	}

	var raw struct {
		Results []struct {
			Title       string `json:"title"`
			Content     string `json:"content"`
			Description string `json:"description"`
			Duration    string `json:"duration"`
			Publisher   string `json:"publisher"`
			Uploader    string `json:"uploader"`
			Images      struct {
				Large string `json:"large"`
				Small string `json:"small"`
			} `json:"images"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("videos decode: %w", err)
	}

	var out []VideoResult
	for _, r := range raw.Results {
		if r.Content == "" {
			continue
		}
		pub := r.Publisher
		if r.Uploader != "" && r.Uploader != pub {
			pub = pub + " / " + r.Uploader
		}
		thumb := r.Images.Large
		if thumb == "" {
			thumb = r.Images.Small
		}
		out = append(out, VideoResult{
			Title:       r.Title,
			URL:         r.Content,
			Thumbnail:   thumb,
			Duration:    r.Duration,
			Publisher:   pub,
			Description: r.Description,
		})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// ── Shopping ──────────────────────────────────────────────────

// SearchShopping queries DuckDuckGo's shopping index.
// It tries the spice endpoint first (no vqd needed), then falls back to
// Bing Shopping HTML scraping.
func (p *Pipeline) SearchShopping(ctx context.Context, query string, max int) ([]ShopResult, error) {
	if r, err := p.ddgSpiceShopping(ctx, query, max); err == nil && len(r) > 0 {
		return r, nil
	}
	return p.bingShopping(ctx, query, max)
}

// ddgSpiceShopping uses DDG's spice/shopping endpoint (JSONP, no vqd needed).
func (p *Pipeline) ddgSpiceShopping(ctx context.Context, query string, max int) ([]ShopResult, error) {
	u := "https://duckduckgo.com/js/spice/shopping/" + url.PathEscape(query)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Referer", "https://duckduckgo.com/")
	req.Header.Set("Accept", "*/*")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("spice shopping: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}
	jsonBody := extractJSON(body)

	var raw struct {
		Results []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Price       string `json:"price"`
			Currency    string `json:"currency"`
			URL         string `json:"url"`
			Image       string `json:"image"`
			Merchant    string `json:"merchant"`
		} `json:"results"`
	}
	if err := json.Unmarshal(jsonBody, &raw); err != nil {
		return nil, fmt.Errorf("spice shopping decode: %w", err)
	}

	var out []ShopResult
	for _, r := range raw.Results {
		if r.URL == "" {
			continue
		}
		out = append(out, ShopResult{
			Title:       r.Title,
			Price:       r.Price,
			Currency:    r.Currency,
			URL:         r.URL,
			Image:       r.Image,
			Merchant:    r.Merchant,
			Description: r.Description,
		})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// bingShopping scrapes Bing Shopping HTML for product results.
func (p *Pipeline) bingShopping(ctx context.Context, query string, max int) ([]ShopResult, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("scope", "shopping")
	params.Set("FORM", "SHOPTB")

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://www.bing.com/shop?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bing shopping: status %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	var out []ShopResult
	// Bing Shopping product cards
	doc.Find(".br-item, .pa-item, .ite_cont, [data-appns='SHOP']").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		title := strings.TrimSpace(s.Find(".br-title, .item-title, .pa-title, [class*='title']").First().Text())
		price := strings.TrimSpace(s.Find(".br-price, .pa-price, [class*='price']").First().Text())
		merchant := strings.TrimSpace(s.Find(".br-sellerName, .pa-merchant, [class*='merchant'], [class*='seller']").First().Text())
		href, _ := s.Find("a").First().Attr("href")
		if href != "" && !strings.HasPrefix(href, "http") {
			href = "https://www.bing.com" + href
		}
		img, _ := s.Find("img").First().Attr("src")
		if title == "" && href == "" {
			return true
		}
		out = append(out, ShopResult{
			Title:    title,
			Price:    price,
			Merchant: merchant,
			URL:      href,
			Image:    img,
		})
		return len(out) < max
	})
	return out, nil
}

// extractJSON strips a JSONP wrapper like "ddg_spice_shopping(...)" if present,
// returning the bare JSON object.
func extractJSON(data []byte) []byte {
	s := strings.TrimSpace(string(data))
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		return data
	}
	start := strings.Index(s, "(")
	end := strings.LastIndex(s, ")")
	if start >= 0 && end > start {
		return []byte(s[start+1 : end])
	}
	return data
}
