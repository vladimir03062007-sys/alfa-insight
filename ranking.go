// Персонализированный отбор и ранжирование новостей для конкретной компании и роли.
//
//	score = w_exposure*exposure + w_sector*sector + w_semantic*semantic + w_region*region
//	      + w_fresh*freshness + w_cred*credibility + w_alfa*alfa_coverage + w_feedback*feedback
//
// Каждое событие получает список причин (reasons) — почему оно попало в ленту.
// Это и объяснение пользователю, и часть трассировки вывода.
package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type RankingConfig struct {
	Weights     map[string]float64 `json:"weights"`
	MinScore    float64            `json:"min_score"`
	NoiseFactor float64            `json:"noise_penalty"`
	FreshDays   float64            `json:"freshness_half_life_days"`
	DedupThresh float64            `json:"dedup_threshold"`
}

func LoadRankingConfig() RankingConfig {
	rc := RankingConfig{
		Weights: map[string]float64{"exposure": 0.38, "sector": 0.17, "semantic": 0.10, "region": 0.05,
			"freshness": 0.10, "credibility": 0.06, "alfa": 0.06, "feedback": 0.15},
		MinScore: 0.22, NoiseFactor: 0.3, FreshDays: 5, DedupThresh: 0.55,
	}
	if err := readJSON(cfg.ConfigDir+"/ranking.json", &rc); err != nil && !os.IsNotExist(err) {
		logWarn("ranking.json: %v — используются веса по умолчанию", err)
	}
	return rc
}

type FactorMatch struct {
	Factor    string   `json:"factor"`
	Name      string   `json:"name"`
	Direction string   `json:"direction"`
	Weight    float64  `json:"weight"`
	Impact    string   `json:"impact"` // risk | opportunity | mixed | neutral
	Effect    string   `json:"effect"`
	Reason    string   `json:"reason"`
	Fields    []string `json:"fields"`
	Evidence  string   `json:"evidence"`
	// Channels — по каждому каналу влияния (кредиты, депозиты, ВЭД, контрагенты) своё следствие.
	Channels []ChannelEffect `json:"channels"`
}

type ChannelEffect struct {
	Reason string   `json:"reason"`
	Field  string   `json:"field"`
	Fields []string `json:"fields"`
	Effect string   `json:"effect"`
	Impact string   `json:"impact"`
}

// effectFor — следствие изменения фактора для одного канала с учётом направления.
func effectFor(factor, direction string, sign float64, up, down string) string {
	if direction == "flat" {
		switch {
		case sign == 0:
			return "" // косвенный канал без изменения фактора — не показываем
		case factor == "key_rate" || factor == "credit_market":
			if sign < 0 {
				return "стоимость заимствований остаётся высокой — снижения процентных расходов пока ждать не стоит"
			}
			return "доходность размещения свободных средств остаётся высокой"
		case factor == "fx_rub":
			return "курс стабилен — рублёвая стоимость валютных контрактов не меняется"
		}
		return "изменений по этому каналу нет — текущие условия сохраняются"
	}
	if direction == "down" {
		return down
	}
	if direction == "unknown" {
		return up + " (направление изменения в новости не указано)"
	}
	return up
}

func impactOf(sign, d float64, direction string) string {
	v := sign * d
	switch {
	case sign == 0 || direction == "unknown":
		return "mixed"
	case v > 0.1:
		return "opportunity"
	case v < -0.1:
		return "risk"
	}
	return "neutral"
}

type RankedEvent struct {
	Event      EventView          `json:"event"`
	Score      float64            `json:"score"`
	Components map[string]float64 `json:"components"`
	Reasons    []string           `json:"reasons"`
	Matches    []FactorMatch      `json:"matches"`
	Impact     string             `json:"impact"`
	Shown      bool               `json:"shown"`
	ev         *Event
}

func dirSign(d string) float64 {
	switch d {
	case "up":
		return 1
	case "down":
		return -1
	}
	return 0
}

// MatchExposure — пересечение факторов события с чувствительностями компании.
func MatchExposure(e *Event, p *CompanyProfile, role Role) []FactorMatch {
	var out []FactorMatch
	pauseAfterCuts := containsAny(e.Content(), []string{"пауз", "после десяти", "после снижен", "впервые с", "прерв", "цикл снижения", "цикла снижения"})
	for _, fs := range e.Factors {
		ex := p.Exposure(fs.Factor)
		if ex == nil {
			continue
		}
		m := FactorMatch{Factor: fs.Factor, Name: fs.Name, Direction: fs.Direction,
			Weight: round(clamp(ex.Weight*role.Boost(fs.Factor), 0, 1), 3),
			Reason: strings.Join(ex.Reasons, "; "), Fields: ex.Fields, Evidence: fs.Evidence}
		d := dirSign(fs.Direction)
		// «Сохранение ставки после серии снижений» — для заёмщика это отложенное облегчение.
		if fs.Direction == "flat" && fs.Factor == "key_rate" && pauseAfterCuts {
			d = 0.5
		}
		m.Impact = impactOf(ex.Sign, d, fs.Direction)
		m.Effect = effectFor(fs.Factor, fs.Direction, ex.Sign, ex.EffectUp, ex.EffectDown)
		// Каналы: самые весомые первыми, общие макро-каналы без поля данных («—») — только если других нет.
		chs := append([]Channel{}, ex.Channels...)
		sort.SliceStable(chs, func(i, j int) bool { return chs[i].Weight > chs[j].Weight })
		for _, ch := range chs {
			if ch.Field == "—" && len(m.Channels) > 0 {
				continue
			}
			ce := ChannelEffect{Reason: ch.Reason, Field: ch.Field, Fields: []string{ch.Field},
				Effect: effectFor(fs.Factor, fs.Direction, ch.Sign, ch.EffectUp, ch.EffectDown), Impact: impactOf(ch.Sign, d, fs.Direction)}
			// Каналы с одинаковым следствием объединяются: «кредиты, лизинг → стоимость заимствований остаётся высокой».
			merged := false
			for k := range m.Channels {
				if m.Channels[k].Effect == ce.Effect && m.Channels[k].Impact == ce.Impact {
					m.Channels[k].Reason += "; " + ce.Reason
					m.Channels[k].Fields = uniqueStrings(append(m.Channels[k].Fields, ce.Field))
					merged = true
					break
				}
			}
			if !merged {
				m.Channels = append(m.Channels, ce)
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	return out
}

func overallImpact(ms []FactorMatch) string {
	risk, opp := 0.0, 0.0
	for _, m := range ms {
		switch m.Impact {
		case "risk":
			risk += m.Weight
		case "opportunity":
			opp += m.Weight
		case "mixed":
			risk += m.Weight / 2
			opp += m.Weight / 2
		}
	}
	switch {
	case risk == 0 && opp == 0:
		return "neutral"
	case risk > 0 && opp > 0 && math.Abs(risk-opp) < 0.3:
		return "mixed"
	case risk > opp:
		return "risk"
	}
	return "opportunity"
}

func freshness(t *time.Time, now time.Time, halfLifeDays float64) float64 {
	if t == nil {
		return 0.5
	}
	ageH := math.Max(0, now.Sub(t.UTC()).Hours())
	return math.Max(0.05, math.Pow(0.5, ageH/(24*halfLifeDays)))
}

// Rank ранжирует события для компании и роли.
func Rank(events []*Event, p *CompanyProfile, role Role, vec VectorizerBackend, kb *KnowledgeBase,
	fb *FeedbackStore, rc RankingConfig) ([]RankedEvent, error) {
	if len(events) == 0 {
		return []RankedEvent{}, nil
	}
	docs := make([]string, len(events))
	for i, e := range events {
		docs[i] = e.Content()
	}
	dv, qv, err := vec.Embed(docs, []string{p.ProfileText()})
	if err != nil {
		return nil, err
	}
	sims := cosineMatrix(dv, qv)
	coverage := map[string]bool{}
	if kb != nil {
		coverage = kb.CoverageFactors()
	}
	now := time.Now().UTC()
	var newest *time.Time
	for _, e := range events {
		if t := e.PublishedAt(); t != nil && (newest == nil || t.After(*newest)) {
			newest = t
		}
	}
	if newest != nil && now.Sub(*newest) > 72*time.Hour {
		now = *newest // статичный (reference) набор: «сейчас» = самая свежая новость
	}
	w := rc.Weights
	out := make([]RankedEvent, 0, len(events))
	for i, e := range events {
		matches := MatchExposure(e, p, role)
		var reasons []string

		exposure := 0.0
		for k, m := range matches {
			if k == 0 {
				exposure = m.Weight
			} else {
				exposure += 0.25 * m.Weight
			}
			if k < 3 {
				reasons = append(reasons, fmt.Sprintf("%s: %s", m.Name, m.Reason))
			}
		}
		exposure = clamp(exposure, 0, 1)

		sector := 0.0
		if contains(e.Sectors, p.Company.Sector) {
			sector = 1
			reasons = append(reasons, "новость об отрасли компании — "+strings.ToLower(p.SectorName))
		} else if sp := specializationFor(p.Company.OKVEDCode); sp != nil {
			for _, rs := range sp.Related {
				if contains(e.Sectors, rs) {
					sector = 0.8
					reasons = append(reasons, "новость о смежной отрасли — "+strings.ToLower(sectorName(rs))+" (специализация: "+sp.Label+")")
					break
				}
			}
		}
		for _, s := range e.Sectors {
			if share := p.CounterSectors[s]; share > 0.05 && s != p.Company.Sector {
				v := clamp(share*1.5, 0, 0.7)
				if v > sector {
					sector = v
				}
				reasons = append(reasons, fmt.Sprintf("затрагивает отрасль контрагентов «%s» (%.0f%% оборота)", sectorName(s), share*100))
			}
		}

		region := 0.0
		for _, r := range e.Regions {
			if r == p.Company.Region || regionStem(r) == regionStem(p.Company.City) {
				region = 1
				reasons = append(reasons, "касается региона присутствия компании")
			} else if p.PersonCities[r] > 0 && region < 0.5 {
				region = 0.5
				reasons = append(reasons, "касается города, где находятся связанные с компанией лица ("+r+")")
			}
		}
		semantic := math.Max(0, sims[i][0])
		fresh := freshness(e.PublishedAt(), now, rc.FreshDays)
		cred := e.Credibility()
		alfa := 0.0
		for _, f := range e.FactorKeys() {
			if coverage[f] {
				alfa = 1
			}
		}
		fbScore := 0.0
		if fb != nil {
			fbScore = fb.Score(e)
			if fbScore > 0.2 {
				reasons = append(reasons, "похожие события вы отмечали как полезные")
			} else if fbScore < -0.2 {
				reasons = append(reasons, "похожие события вы отмечали как неинтересные")
			}
		}

		score := w["exposure"]*exposure + w["sector"]*sector + w["semantic"]*semantic + w["region"]*region +
			w["freshness"]*fresh + w["credibility"]*cred + w["alfa"]*alfa + w["feedback"]*fbScore
		if len(matches) == 0 && sector == 0 {
			score *= rc.NoiseFactor // событие не связано с бизнесом компании — «шум»
		}
		if e.Suspicious {
			score *= 0.5
			reasons = append(reasons, "⚠ в тексте найдены подозрительные инструкции — источник понижен")
		}
		score = clamp(score, 0, 1)
		out = append(out, RankedEvent{
			Event: e.View(), Score: round(score, 4),
			Components: map[string]float64{"exposure": round(exposure, 3), "sector": round(sector, 3),
				"semantic": round(semantic, 3), "region": region, "freshness": round(fresh, 3),
				"credibility": round(cred, 3), "alfa_coverage": alfa, "feedback": round(fbScore, 3)},
			Reasons: uniqueStrings(reasons), Matches: matches, Impact: overallImpact(matches),
			Shown: score >= rc.MinScore, ev: e,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// ---------- обратная связь ----------

type FeedbackStore struct {
	Path string                  `json:"-"`
	Data map[string]FeedbackItem `json:"items"`
	mu   sync.Mutex
}

type FeedbackItem struct {
	EventID string   `json:"event_id"`
	Vote    string   `json:"vote"` // like | dislike
	Factors []string `json:"factors"`
	Sectors []string `json:"sectors"`
	Title   string   `json:"title"`
	Time    string   `json:"time"`
	Comment string   `json:"comment,omitempty"`
}

func NewFeedbackStore(company, role string) *FeedbackStore {
	dir := cfg.StateDir + "/feedback"
	_ = os.MkdirAll(dir, 0o755)
	fs := &FeedbackStore{Path: fmt.Sprintf("%s/%s__%s.json", dir, safeName(company), safeName(role)), Data: map[string]FeedbackItem{}}
	_ = readJSON(fs.Path, fs)
	if fs.Data == nil {
		fs.Data = map[string]FeedbackItem{}
	}
	return fs
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == '.' || r == ' ' {
			r = '_'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (fs *FeedbackStore) Add(e *Event, vote, comment string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.Data[e.EventID] = FeedbackItem{EventID: e.EventID, Vote: vote, Factors: e.FactorKeys(), Sectors: e.Sectors,
		Title: e.Title, Time: time.Now().UTC().Format(time.RFC3339), Comment: comment}
	return writeJSON(fs.Path, fs)
}

// Score — насколько событие похоже на ранее оценённые (по факторам и отраслям), -1..1.
func (fs *FeedbackStore) Score(e *Event) float64 {
	if len(fs.Data) == 0 {
		return 0
	}
	adj := map[string]float64{}
	for _, it := range fs.Data {
		v := 1.0
		if it.Vote == "dislike" {
			v = -1
		}
		for _, f := range it.Factors {
			adj["f:"+f] += v
		}
		for _, s := range it.Sectors {
			adj["s:"+s] += 0.5 * v
		}
	}
	var xs []float64
	for _, f := range e.FactorKeys() {
		xs = append(xs, math.Tanh(adj["f:"+f]/2))
	}
	for _, s := range e.Sectors {
		xs = append(xs, math.Tanh(adj["s:"+s]/2))
	}
	if it, ok := fs.Data[e.EventID]; ok {
		if it.Vote == "like" {
			return 1
		}
		return -1
	}
	return clamp(mean(xs), -1, 1)
}

func (fs *FeedbackStore) Stats() (likes, dislikes int) {
	for _, it := range fs.Data {
		if it.Vote == "like" {
			likes++
		} else {
			dislikes++
		}
	}
	return
}
