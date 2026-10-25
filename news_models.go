// Модели новостей: Publication (одна публикация из источника)
// и Event (событие — группа публикаций об одном и том же после дедупликации).
package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Publication struct {
	SourceID    string         `json:"source_id"`
	SourceName  string         `json:"source_name"`
	Title       string         `json:"title"`
	URL         string         `json:"url"`
	Text        string         `json:"text"`
	PublishedAt *time.Time     `json:"-"`
	Credibility float64        `json:"credibility"` // 0..1, из configs/sources.json
	Synthetic   bool           `json:"synthetic"`   // тестовые (синтетические) данные
	Metadata    map[string]any `json:"metadata,omitempty"`
}

func (p *Publication) Content() string {
	return strings.TrimSpace(p.Title + ". " + strings.TrimSpace(p.Text))
}

func (p *Publication) ID() string { return "PUB-" + shortHash(p.SourceID, p.URL, p.Title) }

// publicationFile — формат публикации во входных JSON-файлах (data/news/*.json).
type publicationFile struct {
	SourceID    string         `json:"source_id"`
	SourceName  string         `json:"source_name"`
	Source      string         `json:"source"`
	Title       string         `json:"title"`
	URL         string         `json:"url"`
	Link        string         `json:"link"`
	Text        string         `json:"text"`
	Summary     string         `json:"summary"`
	PublishedAt string         `json:"published_at"`
	Credibility float64        `json:"credibility"`
	Synthetic   bool           `json:"synthetic"`
	Metadata    map[string]any `json:"metadata"`
}

// LoadPublications читает массив публикаций (или объект {"items": [...]}).
func LoadPublications(path string) ([]Publication, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []publicationFile
	if err := jsonUnmarshalFlexible(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]Publication, 0, len(list))
	for _, x := range list {
		p := Publication{
			SourceID: x.SourceID, SourceName: x.SourceName, Title: x.Title,
			URL: x.URL, Text: x.Text, Credibility: x.Credibility,
			Synthetic: x.Synthetic, Metadata: x.Metadata,
		}
		if p.SourceName == "" {
			p.SourceName = x.Source
		}
		if p.SourceID == "" {
			p.SourceID = strings.ToLower(strings.ReplaceAll(p.SourceName, " ", "_"))
		}
		if p.URL == "" {
			p.URL = x.Link
		}
		if p.Text == "" {
			p.Text = x.Summary
		}
		if p.Credibility == 0 {
			p.Credibility = 0.7
		}
		p.PublishedAt = parseDate(x.PublishedAt)
		if strings.TrimSpace(p.Title) == "" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ---------- Event ----------

// FactorSignal — макро/отраслевой фактор, затронутый событием, и направление его изменения.
type FactorSignal struct {
	Factor    string `json:"factor"`    // ключ фактора (key_rate, fx_rub, oil, ...)
	Name      string `json:"name"`      // человекочитаемое название
	Direction string `json:"direction"` // up | down | flat | unknown
	Evidence  string `json:"evidence"`  // фрагмент текста, на котором основан вывод
}

type Event struct {
	EventID      string         `json:"event_id"`
	Title        string         `json:"title"`
	Text         string         `json:"text"`
	EventType    string         `json:"event_type"` // monetary_policy | fx | commodities | regulation | ...
	Factors      []FactorSignal `json:"factors"`
	Sectors      []string       `json:"sectors"` // ключи отраслей из okved.go
	Regions      []string       `json:"regions"`
	Entities     []string       `json:"entities"`
	KeyFacts     []string       `json:"key_facts"` // предложения с цифрами/датами
	Publications []Publication  `json:"publications"`
	Suspicious   bool           `json:"suspicious"` // найдены признаки prompt-injection
	Synthetic    bool           `json:"synthetic"`
}

func (e *Event) URLs() []string {
	var out []string
	for _, p := range e.Publications {
		if p.URL != "" {
			out = append(out, p.URL)
		}
	}
	return uniqueStrings(out)
}

func (e *Event) Sources() []string {
	var out []string
	for _, p := range e.Publications {
		out = append(out, p.SourceName)
	}
	return uniqueStrings(out)
}

func (e *Event) PublishedAt() *time.Time {
	var best *time.Time
	for i := range e.Publications {
		d := e.Publications[i].PublishedAt
		if d != nil && (best == nil || d.After(*best)) {
			best = d
		}
	}
	return best
}

func (e *Event) Credibility() float64 {
	best := 0.0
	for _, p := range e.Publications {
		if p.Credibility > best {
			best = p.Credibility
		}
	}
	// Подтверждение несколькими независимыми источниками повышает доверие.
	bonus := 0.05 * float64(len(e.Sources())-1)
	return clamp(best+bonus, 0, 1)
}

func (e *Event) Content() string {
	return strings.TrimSpace(e.Title + ". " + e.Text)
}

func (e *Event) FactorKeys() []string {
	var out []string
	for _, f := range e.Factors {
		out = append(out, f.Factor)
	}
	return out
}

// EventView — JSON-представление события для API.
type EventView struct {
	EventID     string         `json:"event_id"`
	Title       string         `json:"title"`
	Text        string         `json:"text"`
	EventType   string         `json:"event_type"`
	Factors     []FactorSignal `json:"factors"`
	Sectors     []string       `json:"sectors"`
	SectorNames []string       `json:"sector_names"`
	Regions     []string       `json:"regions"`
	KeyFacts    []string       `json:"key_facts"`
	PublishedAt *string        `json:"published_at"`
	Sources     []SourceRef    `json:"sources"`
	Credibility float64        `json:"credibility"`
	Suspicious  bool           `json:"suspicious"`
	Synthetic   bool           `json:"synthetic"`
}

type SourceRef struct {
	ID    string `json:"id"`
	Type  string `json:"type"` // news | alfa | client_data
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	Name  string `json:"name,omitempty"`
	Date  string `json:"date,omitempty"`
	File  string `json:"file,omitempty"` // для аналитики банка: файл материала
	Page  int    `json:"page,omitempty"` // и страница — чтобы проверить цитату по первоисточнику
}

func (e *Event) View() EventView {
	v := EventView{
		EventID: e.EventID, Title: e.Title, Text: e.Text, EventType: e.EventType,
		Factors: e.Factors, Sectors: e.Sectors, Regions: e.Regions, KeyFacts: e.KeyFacts,
		PublishedAt: dtToStr(e.PublishedAt()), Credibility: round(e.Credibility(), 2),
		Suspicious: e.Suspicious, Synthetic: e.Synthetic,
	}
	for _, s := range e.Sectors {
		v.SectorNames = append(v.SectorNames, sectorName(s))
	}
	for i, p := range e.Publications {
		date := ""
		if p.PublishedAt != nil {
			date = p.PublishedAt.Format("2006-01-02")
		}
		v.Sources = append(v.Sources, SourceRef{
			ID: fmt.Sprintf("N%d", i+1), Type: "news", Title: p.Title, URL: p.URL, Name: p.SourceName, Date: date,
		})
	}
	if v.Factors == nil {
		v.Factors = []FactorSignal{}
	}
	return v
}
