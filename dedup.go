// Дедупликация: несколько публикаций об одном и том же объединяются в одно событие.
// Этап 1 — точное совпадение нормализованного заголовка.
// Этап 2 — семантическая кластеризация (эмбеддинги + union-find) с защитой
// от склейки разных событий по общему слову.
package main

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

func normTitle(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		} else {
			space = true
		}
	}
	return b.String()
}

func eventIDFor(pubs []Publication) string {
	ids := make([]string, len(pubs))
	for i := range pubs {
		ids[i] = pubs[i].ID()
	}
	sort.Strings(ids)
	return "EVT-" + shortHash(ids...)
}

// makeEvent: представитель кластера — публикация с самым полным текстом.
func makeEvent(cluster []Publication) *Event {
	rep := 0
	for i := range cluster {
		if runeLen(cluster[i].Content()) > runeLen(cluster[rep].Content()) {
			rep = i
		}
	}
	// Сначала — самые надёжные источники.
	sorted := append([]Publication{}, cluster...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Credibility > sorted[j].Credibility })
	e := &Event{EventID: eventIDFor(cluster), Title: cluster[rep].Title, Text: cluster[rep].Text, Publications: sorted}
	for _, p := range cluster {
		if p.Synthetic {
			e.Synthetic = true
		}
	}
	return e
}

var genericWords = map[string]bool{
	"россии": true, "россия": true, "рынок": true, "рынка": true, "компании": true, "компаний": true,
	"новости": true, "году": true, "года": true, "может": true, "будет": true, "после": true,
	"ставка": true, "ставку": true, "банк": true, "банка": true,
}

var titleTokenRe = regexp.MustCompile(`[a-zA-Zа-яА-ЯёЁ0-9_-]{4,}`)

func Deduplicate(pubs []Publication, vectorizer VectorizerBackend, threshold float64) ([]*Event, error) {
	if len(pubs) == 0 {
		return []*Event{}, nil
	}
	groups := map[string][]Publication{}
	var order []string
	for _, p := range pubs {
		key := normTitle(p.Title)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], p)
	}
	stage := make([]*Event, 0, len(order))
	for _, k := range order {
		stage = append(stage, makeEvent(groups[k]))
	}
	if len(stage) <= 1 {
		return stage, nil
	}

	texts := make([]string, len(stage))
	for i, e := range stage {
		texts[i] = e.Title + ". " + truncRunes(e.Text, 300)
	}
	docs, _, err := vectorizer.Embed(texts, nil)
	if err != nil {
		return nil, err
	}
	sims := cosineMatrix(docs, docs)
	n := len(stage)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	tokens := make([]map[string]bool, n)
	for i, e := range stage {
		t := map[string]bool{}
		for _, w := range titleTokenRe.FindAllString(strings.ToLower(e.Title), -1) {
			if !genericWords[w] {
				t[truncRunes(w, 6)] = true // грубый стемминг
			}
		}
		tokens[i] = t
	}
	shared := func(a, b map[string]bool) int {
		k := 0
		for t := range a {
			if b[t] {
				k++
			}
		}
		return k
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			sh := shared(tokens[i], tokens[j])
			// Склеиваем, если тексты близки и есть ≥2 общих «конкретных» слова в заголовке,
			// либо заголовки делят ≥2 слова, а публикации вышли с разницей не больше 2 суток.
			if (sims[i][j] >= threshold && (sh >= 2 || sims[i][j] >= 0.95)) ||
				(sh >= 2 && sims[i][j] >= 0.2 && closeInTime(stage[i], stage[j], 48*time.Hour)) {
				if ri, rj := find(i), find(j); ri != rj {
					parent[rj] = ri
				}
			}
		}
	}
	clusters := map[int][]Publication{}
	var roots []int
	for i, e := range stage {
		r := find(i)
		if _, ok := clusters[r]; !ok {
			roots = append(roots, r)
		}
		clusters[r] = append(clusters[r], e.Publications...)
	}
	out := make([]*Event, 0, len(roots))
	for _, r := range roots {
		out = append(out, makeEvent(clusters[r]))
	}
	return out, nil
}

func closeInTime(a, b *Event, d time.Duration) bool {
	ta, tb := a.PublishedAt(), b.PublishedAt()
	if ta == nil || tb == nil {
		return false
	}
	diff := ta.Sub(*tb)
	if diff < 0 {
		diff = -diff
	}
	return diff <= d
}
