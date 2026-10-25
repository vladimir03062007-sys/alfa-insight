// Парсер новостей: RSS/Atom-ленты, публичные Telegram-каналы (через веб-превью
// t.me/s/<канал>) и автоопределение RSS на обычной странице.
// Реализован только на стандартной библиотеке Go (без внешних зависимостей).
package main

import (
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Source struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	Kind        string  `json:"kind"` // rss | telegram | auto
	Credibility float64 `json:"credibility"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

func LoadSources() ([]Source, error) {
	var file struct {
		Sources []Source `json:"sources"`
	}
	if err := readJSON(cfg.ConfigDir+"/sources.json", &file); err != nil {
		return nil, err
	}
	var out []Source
	for _, s := range file.Sources {
		if s.Enabled != nil && !*s.Enabled {
			continue
		}
		if s.Kind == "" {
			s.Kind = "auto"
		}
		if s.Credibility == 0 {
			s.Credibility = 0.7
		}
		out = append(out, s)
	}
	return out, nil
}

var httpClient = &http.Client{}

func httpGet(u string) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	httpClient.Timeout = time.Duration(cfg.RequestTimeout) * time.Second
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("HTTP %d для %s", resp.StatusCode, u)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.Header.Get("Content-Type"), err
}

var (
	tagRe   = regexp.MustCompile(`<[^>]+>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// clean: убрать HTML-теги и сущности, схлопнуть пробелы, обрезать до limit символов.
func clean(text string, limit int) string {
	text = tagRe.ReplaceAllString(text, " ")
	text = strings.TrimSpace(spaceRe.ReplaceAllString(html.UnescapeString(text), " "))
	return truncRunes(text, limit)
}

// ---------- кодировки ----------

// cp1251 — таблица символов 0x80..0xFF для windows-1251 (часть российских RSS).
var cp1251 = []rune("ЂЃ‚ѓ„…†‡€‰Љ‹ЊЌЋЏђ‘’“”•–—�™љ›њќћџ ЎўЈ¤Ґ¦§Ё©Є«¬­®Ї°±Ііґµ¶·ё№є»јЅѕї" +
	"АБВГДЕЖЗИЙКЛМНОПРСТУФХЦЧШЩЪЫЬЭЮЯабвгдежзийклмнопрстуфхцчшщъыьэюя")

func decodeCP1251(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c < 0x80 {
			sb.WriteByte(c)
		} else if int(c-0x80) < len(cp1251) {
			sb.WriteRune(cp1251[c-0x80])
		}
	}
	return sb.String()
}

func charsetReader(label string, input io.Reader) (io.Reader, error) {
	label = strings.ToLower(label)
	if strings.Contains(label, "1251") || strings.Contains(label, "cp1251") {
		data, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		return strings.NewReader(decodeCP1251(data)), nil
	}
	return input, nil // utf-8 и совместимые
}

// toUTF8 — перекодирует HTML-страницу, если она не в UTF-8.
func toUTF8(data []byte, contentType string) string {
	if utf8.Valid(data) {
		return string(data)
	}
	if strings.Contains(strings.ToLower(contentType), "1251") || !utf8.Valid(data) {
		return decodeCP1251(data)
	}
	return string(data)
}

// ---------- RSS / Atom ----------

type rssLink struct {
	Href string `xml:"href,attr"`
	Text string `xml:",chardata"`
}

type rssItem struct {
	Title       string    `xml:"title"`
	Links       []rssLink `xml:"link"`
	GUID        string    `xml:"guid"`
	ID          string    `xml:"id"`
	Description string    `xml:"description"`
	Summary     string    `xml:"summary"`
	Content     string    `xml:"content"`
	FullText    string    `xml:"full-text"`
	Encoded     string    `xml:"encoded"`
	PubDate     string    `xml:"pubDate"`
	Published   string    `xml:"published"`
	Updated     string    `xml:"updated"`
	Date        string    `xml:"date"`
}

type rssDoc struct {
	Items   []rssItem `xml:"channel>item"`
	Entries []rssItem `xml:"entry"`
	RDF     []rssItem `xml:"item"`
}

func parseFeed(data []byte, src Source) ([]Publication, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.CharsetReader = charsetReader
	var doc rssDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	items := append(append(doc.Items, doc.Entries...), doc.RDF...)
	var out []Publication
	for _, it := range items {
		title := clean(it.Title, 400)
		link := ""
		for _, l := range it.Links {
			if l.Href != "" {
				link = l.Href
				break
			}
			if t := strings.TrimSpace(l.Text); t != "" {
				link = t
				break
			}
		}
		if link == "" {
			link = strings.TrimSpace(it.GUID + it.ID)
		}
		body := it.FullText
		for _, alt := range []string{it.Encoded, it.Content, it.Description, it.Summary} {
			if runeLen(alt) > runeLen(body) {
				body = alt
			}
		}
		date := it.PubDate
		for _, alt := range []string{it.Published, it.Updated, it.Date} {
			if date == "" {
				date = alt
			}
		}
		if title == "" || link == "" {
			continue
		}
		out = append(out, Publication{
			SourceID: src.ID, SourceName: src.Name, Title: title, URL: link,
			Text: clean(body, 4000), PublishedAt: parseDate(date), Credibility: src.Credibility,
		})
	}
	return out, nil
}

var rssLinkRe = regexp.MustCompile(`(?i)<link[^>]+type=["']application/(rss|atom)\+xml["'][^>]*>`)
var hrefRe = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)

func fetchAuto(src Source) ([]Publication, error) {
	data, ct, err := httpGet(src.URL)
	if err != nil {
		return nil, err
	}
	if items, err := parseFeed(data, src); err == nil && len(items) > 0 {
		return items, nil
	}
	// Обычная страница: ищем <link rel="alternate" type="application/rss+xml">.
	page := toUTF8(data, ct)
	if m := rssLinkRe.FindString(page); m != "" {
		if h := hrefRe.FindStringSubmatch(m); len(h) > 1 {
			base, _ := url.Parse(src.URL)
			ref, _ := url.Parse(html.UnescapeString(h[1]))
			if base != nil && ref != nil {
				feed := base.ResolveReference(ref).String()
				d2, _, err := httpGet(feed)
				if err == nil {
					return parseFeed(d2, src)
				}
			}
		}
	}
	return nil, fmt.Errorf("на странице %s не найдена RSS-лента", src.URL)
}

// ---------- публичные Telegram-каналы ----------

var (
	tgMsgRe = regexp.MustCompile(`(?s)data-post="([^"]+)".*?tgme_widget_message_text[^>]*>(.*?)</div>.*?<time[^>]+datetime="([^"]+)"`)
)

func fetchTelegram(src Source) ([]Publication, error) {
	u := src.URL
	if !strings.Contains(u, "/s/") {
		parts := strings.Split(strings.TrimRight(u, "/"), "/")
		u = "https://t.me/s/" + strings.TrimLeft(parts[len(parts)-1], "@")
	}
	data, ct, err := httpGet(u)
	if err != nil {
		return nil, err
	}
	page := toUTF8(data, ct)
	var out []Publication
	for _, m := range tgMsgRe.FindAllStringSubmatch(page, -1) {
		text := clean(strings.ReplaceAll(m[2], "<br/>", ". "), 4000)
		if runeLen(text) < 40 {
			continue
		}
		out = append(out, Publication{
			SourceID: src.ID, SourceName: src.Name, Title: shorten(text, 140), URL: "https://t.me/" + m[1],
			Text: text, PublishedAt: parseDate(m[3]), Credibility: src.Credibility,
		})
	}
	return out, nil
}

// ---------- параллельный сбор ----------

func fetchSource(src Source) ([]Publication, error) {
	switch src.Kind {
	case "telegram":
		return fetchTelegram(src)
	case "rss":
		data, _, err := httpGet(src.URL)
		if err != nil {
			return nil, err
		}
		return parseFeed(data, src)
	}
	return fetchAuto(src)
}

// Collect опрашивает все источники параллельно и оставляет публикации не старше hours часов.
func Collect(sources []Source, hours int) ([]Publication, map[string]string) {
	type result struct {
		items []Publication
		err   error
	}
	results := make([]result, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Add(1)
		go func(i int, s Source) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[i] = result{err: fmt.Errorf("panic: %v", r)}
				}
			}()
			items, err := fetchSource(s)
			results[i] = result{items, err}
		}(i, s)
	}
	wg.Wait()

	status := map[string]string{}
	cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	var out []Publication
	for i, s := range sources {
		r := results[i]
		if r.err != nil {
			logWarn("Источник %s недоступен: %v", s.Name, r.err)
			status[s.Name] = "ошибка: " + r.err.Error()
			continue
		}
		n := 0
		for _, item := range r.items {
			if item.PublishedAt != nil && item.PublishedAt.Before(cutoff) {
				continue
			}
			if n >= cfg.MaxPerSource {
				break
			}
			out = append(out, item)
			n++
		}
		status[s.Name] = fmt.Sprintf("ok: %d", n)
		logInfo("%s: %d материалов", s.Name, n)
	}
	seen := map[string]bool{}
	var unique []Publication
	for _, item := range out {
		key := strings.TrimRight(strings.SplitN(item.URL, "?", 2)[0], "/")
		if key == "" {
			key = strings.ToLower(item.Title)
		}
		if !seen[key] {
			seen[key] = true
			unique = append(unique, item)
		}
	}
	if len(unique) > cfg.MaxTotal {
		unique = unique[:cfg.MaxTotal]
	}
	return unique, status
}
