// Индекс новостей: загрузка (reference / file / live), дедупликация, классификация.
package main

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

type NewsIndex struct {
	mu       sync.RWMutex
	Events   []*Event
	byID     map[string]*Event
	Mode     string
	Path     string
	LoadedAt time.Time
	Status   map[string]string
	PubCount int
}

func newsPathFor(mode, file string) string {
	switch mode {
	case "file":
		return file
	case "holdout":
		return filepath.Join(cfg.NewsDir, "holdout_news.json")
	}
	return filepath.Join(cfg.NewsDir, "sample_news.json")
}

// LoadNews собирает публикации, склеивает дубликаты и размечает события.
func LoadNews(mode, file string, vec VectorizerBackend, rc RankingConfig, regionHints []string) (*NewsIndex, error) {
	idx := &NewsIndex{Mode: mode, Status: map[string]string{}}
	var pubs []Publication
	var err error
	switch mode {
	case "live":
		var sources []Source
		sources, err = LoadSources()
		if err == nil {
			pubs, idx.Status = Collect(sources, cfg.NewsHours)
		}
		if err == nil && len(pubs) == 0 {
			err = fmt.Errorf("ни один источник не вернул новостей — проверьте сеть и configs/sources.json")
		}
	case "reference", "file", "holdout":
		if mode == "file" && file == "" {
			return nil, fmt.Errorf("для режима file укажите NEWS_FILE или --news-file")
		}
		idx.Path = newsPathFor(mode, file)
		pubs, err = LoadPublications(idx.Path)
	default:
		err = fmt.Errorf("неизвестный режим новостей: %s", mode)
	}
	if err != nil {
		return nil, err
	}
	idx.PubCount = len(pubs)
	events, err := Deduplicate(pubs, vec, rc.DedupThresh)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		ClassifyEvent(e, regionHints)
	}
	idx.Events = events
	idx.byID = map[string]*Event{}
	for _, e := range events {
		idx.byID[e.EventID] = e
	}
	idx.LoadedAt = time.Now()
	logInfo("Новости: %d публикаций -> %d событий (режим %s)", len(pubs), len(events), mode)
	return idx, nil
}

func (n *NewsIndex) Get(id string) (*Event, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	e, ok := n.byID[id]
	return e, ok
}

func (n *NewsIndex) All() []*Event {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return append([]*Event{}, n.Events...)
}

// regionHints — список регионов и городов из данных о компаниях.
func regionHints(store *CompanyStore) []string {
	var out []string
	for _, id := range store.Order {
		c := store.Companies[id]
		out = append(out, c.Region, c.City)
		if len(out) > 2000 {
			break
		}
	}
	for _, g := range store.personGeo { // регионы связанных физлиц (RC_NAME, CITY_NAME)
		if len(out) > 4000 {
			break
		}
		out = append(out, g[0]) // город связанного лица (RC_NAME — макрорегион банка, не регион)
	}
	return uniqueStrings(out)
}

// Synthetic — все ли события в ленте синтетические (для предупреждения в интерфейсе).
func (n *NewsIndex) Synthetic() bool {
	if len(n.Events) == 0 {
		return false
	}
	for _, e := range n.Events {
		if !e.Synthetic {
			return false
		}
	}
	return true
}
