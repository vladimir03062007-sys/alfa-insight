// Схема персонализированного инсайта (JSON) и офлайн-составитель текста.
//
// Формат отвечает на 6 вопросов кейса:
//  1. Что произошло?               -> what_happened (факты отделены от предположений)
//  2. Почему это важно компании?   -> why_it_matters (признаки клиента с полями-источниками)
//  3. Какие последствия возможны?  -> consequences (риски / возможности / факторы)
//  4. Какие действия релевантны?   -> actions (действия, вопросы, сценарии)
//  5. Как это оценивает банк?      -> bank_view (только из материалов Альфа-Банка, с цитатами)
//  6. На чём основан вывод?        -> basis у каждого пункта + sources + trace_id
//
// Выводы модели (AI) отделены от позиции банка: model_conclusions vs bank_view.
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Fact struct {
	Text      string   `json:"text"`
	SourceIDs []string `json:"source_ids"`
	Status    string   `json:"status"` // confirmed (≥2 источника или надёжный источник) | reported | assumption
}

type Point struct {
	Text    string   `json:"text"`
	Basis   []string `json:"basis"`             // ID источников: N1 (новость), A1.2 (аналитика банка), C:FIELD (данные клиента)
	Horizon string   `json:"horizon,omitempty"` // горизонт влияния
	Factor  string   `json:"factor,omitempty"`
	Origin  string   `json:"origin"` // fact | bank | model
}

type Action struct {
	Text  string   `json:"text"`
	Type  string   `json:"type"` // action | question | scenario
	Basis []string `json:"basis,omitempty"`
}

type Citation struct {
	ID        string `json:"id"`
	DocTitle  string `json:"doc_title"`
	Quote     string `json:"quote"`
	Date      string `json:"date,omitempty"`
	File      string `json:"file,omitempty"`
	Page      int    `json:"page,omitempty"`
	Synthetic bool   `json:"synthetic"`
	Stale     bool   `json:"stale,omitempty"`
	Note      string `json:"note,omitempty"` // «материал 2021 г. — может быть неактуален», «прогноз пересмотрен»
}

type BankView struct {
	Available bool       `json:"available"`
	Label     string     `json:"label"`
	Summary   string     `json:"summary"`
	Points    []Point    `json:"points"`
	Citations []Citation `json:"citations"`
	// Forecasts — актуальные числовые прогнозы банка по факторам события и история их пересмотров.
	Forecasts []ForecastTrack `json:"forecasts"`
	// Revisions — предупреждения: в процитированном материале прогноз, который банк уже пересмотрел.
	Revisions []string `json:"revisions"`
	// AsOf — дата самого свежего материала банка, на который опирается позиция.
	AsOf string `json:"as_of,omitempty"`
}

type ProductHint struct {
	Text    string `json:"text"`
	Product string `json:"product"`
	Label   string `json:"label"` // «Реклама» — нативная реклама явно помечена
	Reason  string `json:"reason"`
}

type SelfCheck struct {
	Passed     bool            `json:"passed"`
	Iterations int             `json:"iterations"`
	Checks     map[string]bool `json:"checks"`
	Issues     []string        `json:"issues"`
	Fixes      []string        `json:"fixes"`
}

type Insight struct {
	InsightID   string   `json:"insight_id"`
	CompanyID   string   `json:"company_id"`
	Role        string   `json:"role"`
	RoleName    string   `json:"role_name"`
	EventID     string   `json:"event_id"`
	GeneratedAt string   `json:"generated_at"`
	Mode        string   `json:"mode"` // llm | offline | llm+offline-fallback
	ModelsUsed  []string `json:"models_used"`

	Headline   string  `json:"headline"`
	Impact     string  `json:"impact"` // risk | opportunity | mixed | neutral
	Relevance  float64 `json:"relevance"`
	Confidence float64 `json:"confidence"`

	WhatHappened struct {
		Summary      string   `json:"summary"`
		Facts        []Fact   `json:"facts"`
		Participants []string `json:"participants"`
		Date         string   `json:"date"`
	} `json:"what_happened"`
	WhyItMatters struct {
		Text           string    `json:"text"`
		ClientFeatures []Feature `json:"client_features"`
		Reasons        []string  `json:"relevance_reasons"`
		Basis          []string  `json:"basis"`
	} `json:"why_it_matters"`
	Consequences struct {
		Risks          []Point  `json:"risks"`
		Opportunities  []Point  `json:"opportunities"`
		FactorsToWatch []string `json:"factors_to_watch"`
	} `json:"consequences"`
	Actions          []Action     `json:"actions"`
	BankView         BankView     `json:"bank_view"`
	ModelConclusions []Point      `json:"model_conclusions"`
	ProductHint      *ProductHint `json:"product_hint,omitempty"`
	// Scenario — калькулятор «что если» в рублях по факторам события (ставка, курс).
	Scenario    *ScenarioPack `json:"scenario,omitempty"`
	Sources     []SourceRef   `json:"sources"`
	DataQuality []string      `json:"data_quality"`
	SelfCheck   SelfCheck     `json:"self_check"`
	TraceID     string        `json:"trace_id"`
	Disclaimer  string        `json:"disclaimer"`
	Synthetic   bool          `json:"synthetic_data"`
}

const disclaimer = "Материал носит информационный характер и не является индивидуальной инвестиционной рекомендацией. " +
	"Блок «Позиция Альфа-Банка» сформирован только на основе аналитических материалов банка; " +
	"блоки, помеченные «Вывод AI», сгенерированы моделью на основе данных и требуют проверки."

// AgentContext — память агента: результаты всех шагов, на которые опирается итоговый вывод.
type AgentContext struct {
	Profile        *CompanyProfile
	Role           Role
	Event          *Event
	Matches        []FactorMatch
	AlfaHits       []ChunkHit
	RelatedNews    []RankedEvent
	Counterparties []Counterparty
	CounterOverlap map[string]float64 // отрасли события, где есть контрагенты
	Relevance      float64
	Reasons        []string
	Notes          []string
	KB             *KnowledgeBase  // база знаний (для прогнозов и календаря)
	Forecasts      []ForecastTrack // актуальные прогнозы банка по факторам события
	Calendar       []CalendarItem  // ближайшие заседания, упомянутые в материалах банка
	ExtraFields    []string        // поля данных клиента, на которые ссылаются расчёты (C:*)
}

var impactLabel = map[string]string{"risk": "Риск", "opportunity": "Возможность", "mixed": "Неоднозначное влияние", "neutral": "Нейтрально"}

func horizonFor(factor string) string {
	switch factor {
	case "key_rate", "credit_market", "fx_rub":
		return "1–6 месяцев"
	case "taxes", "regulation":
		return "с даты вступления в силу"
	case "oil", "gas", "metals", "gold", "agri":
		return "1–3 месяца"
	}
	return "3–12 месяцев"
}

// sourcesFor собирает реестр источников: новости (N), аналитика банка (A), данные клиента (C:).
func sourcesFor(ctx *AgentContext) []SourceRef {
	src := ctx.Event.View().Sources
	for _, h := range ctx.AlfaHits {
		title := h.DocTitle
		if h.Synthetic {
			title += " (синтетический пример)"
		}
		if h.Stale {
			title += " (устаревший материал)"
		}
		src = append(src, SourceRef{ID: h.ID, Type: "alfa", Title: title, Name: "Аналитика Альфа-Банка", Date: h.Date, File: h.File, Page: h.Page})
	}
	seen := map[string]bool{}
	for _, m := range ctx.Matches {
		for _, f := range m.Fields {
			if f != "—" && !seen[f] {
				seen[f] = true
				src = append(src, SourceRef{ID: "C:" + f, Type: "client_data", Title: "Данные о компании: " + fieldTitle(f), Name: "Профиль клиента"})
			}
		}
	}
	for _, f := range ctx.ExtraFields {
		if !seen[f] {
			seen[f] = true
			src = append(src, SourceRef{ID: "C:" + f, Type: "client_data", Title: "Данные о компании: " + fieldTitle(f), Name: "Профиль клиента"})
		}
	}
	if len(ctx.CounterOverlap) > 0 && !seen["df_Uid_payment_hack"] {
		src = append(src, SourceRef{ID: "C:df_Uid_payment_hack", Type: "client_data", Title: "Платежи компании (контрагенты)", Name: "Профиль клиента"})
	}
	for _, r := range ctx.RelatedNews {
		if len(r.Event.Sources) > 0 {
			s := r.Event.Sources[0]
			s.ID = "R:" + r.Event.EventID
			s.Type = "news_related"
			src = append(src, s)
		}
	}
	return src
}

// fieldTitles — понятные названия полей данных клиента (для источников в интерфейсе).
var fieldTitles = map[string]string{
	"MAINOKVED_CODE": "отрасль (ОКВЭД)", "MAINOKVED_NAME": "вид деятельности", "REVENUE_COMPANY": "выручка",
	"CL_PRODUCT_LOA_MARK": "кредиты в банке", "CL_PRODUCT_DEP_MARK": "депозиты", "CL_PRODUCT_LEA_MARK": "лизинг",
	"CL_PRODUCT_GAR_MARK": "банковские гарантии", "CL_PRODUCT_ZPP_MARK": "зарплатный проект", "CL_PRODUCT_RKO_MARK": "расчётный счёт",
	"FACTORING_MARK": "факторинг", "FTS_FLAG": "внешнеэкономическая деятельность", "FTS_COUNTRY_NAME": "страны ВЭД",
	"AMOUNT_WORKERS_FNS_GRADATION": "численность сотрудников", "REGION": "регион", "CITY_NAME": "города связанных лиц",
	"RC_NAME": "макрорегион", "GROUP_ID": "группа компаний", "NEGATIVEMARK": "негативные отметки", "CL_TYPE": "статус в банке",
	"df_Uid_payment_hack": "платежи с контрагентами", "OKFS_NAME": "форма собственности", "COMPANY_AGE_YEARS_GRADATION": "возраст компании",
	"ECO_MARK_LABEL": "стадия развития", "ORG_TYPE": "тип организации",
}

func fieldTitle(f string) string {
	if t, ok := fieldTitles[f]; ok {
		return t
	}
	return f
}

func basisFor(m FactorMatch) []string {
	b := []string{"N1"}
	for _, f := range m.Fields {
		if f != "—" {
			b = append(b, "C:"+f)
		}
	}
	return b
}

// ComposeOffline — детерминированный составитель инсайта (без LLM).
func ComposeOffline(ctx *AgentContext) *Insight {
	e, p := ctx.Event, ctx.Profile
	in := &Insight{CompanyID: p.Company.ID, Role: ctx.Role.Key, RoleName: ctx.Role.Name, EventID: e.EventID,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339), Mode: "offline", ModelsUsed: []string{},
		Impact: overallImpact(ctx.Matches), Relevance: round(ctx.Relevance, 3), Disclaimer: disclaimer,
		Synthetic: p.Synthetic || e.Synthetic}
	in.Sources = sourcesFor(ctx)
	in.DataQuality = append([]string{}, p.DataQuality...)

	// 1. Что произошло.
	sents := splitSentences(e.Text)
	sum := e.Title
	if len(sents) > 0 {
		sum = strings.Join(sents[:min(2, len(sents))], " ")
	}
	in.WhatHappened.Summary = shorten(sum, 450)
	status := "reported"
	if len(e.Sources()) >= 2 || e.Credibility() >= 0.85 {
		status = "confirmed"
	}
	for _, f := range e.KeyFacts {
		in.WhatHappened.Facts = append(in.WhatHappened.Facts, Fact{Text: f, SourceIDs: []string{"N1"}, Status: status})
	}
	in.WhatHappened.Participants = e.Entities
	in.WhatHappened.Date = formatDateRu(e.PublishedAt())

	// 2. Почему важно: по каждому фактору — каналы влияния на компанию с полем данных-источником.
	var parts, alsoTouched []string
	usedFields := map[string]bool{}
	knownDir := 0
	for _, m := range ctx.Matches {
		if m.Direction != "unknown" {
			knownDir++
		}
	}
	shown := 0
	for _, m := range ctx.Matches {
		if m.Direction == "unknown" && knownDir > 0 {
			alsoTouched = append(alsoTouched, strings.ToLower(m.Name))
			continue
		}
		if shown >= 3 {
			break
		}
		shown++
		fs := FactorSignal{Factor: m.Factor, Name: m.Name, Direction: m.Direction}
		var chs []string
		for k, ch := range m.Channels {
			if k >= 3 {
				break
			}
			if ch.Effect == "" {
				continue
			}
			chs = append(chs, fmt.Sprintf("%s → %s", ch.Reason, ch.Effect))
			usedFields[ch.Field] = true
			for _, f := range ch.Fields {
				usedFields[f] = true
			}
		}
		if len(chs) == 0 {
			chs = append(chs, m.Reason+" → "+m.Effect)
			for _, f := range m.Fields {
				usedFields[f] = true
			}
		}
		parts = append(parts, fmt.Sprintf("%s. Для компании: %s", capitalize(directionPhrase(fs)), strings.Join(chs, "; ")))
	}
	for sec, share := range ctx.CounterOverlap {
		parts = append(parts, fmt.Sprintf("Событие затрагивает отрасль «%s», на контрагентов из которой приходится %.0f%% платёжного оборота компании",
			sectorName(sec), share*100))
		usedFields["df_Uid_payment_hack"] = true
	}
	if contains(e.Sectors, p.Company.Sector) {
		parts = append([]string{fmt.Sprintf("Новость напрямую касается отрасли компании (%s)", strings.ToLower(p.SectorName))}, parts...)
		usedFields["MAINOKVED_CODE"] = true
	}
	if len(alsoTouched) > 0 {
		parts = append(parts, "В новости также упоминаются: "+strings.Join(alsoTouched, ", ")+" — направление их изменения не указано, поэтому они не включены в оценку")
	}
	if len(parts) == 0 {
		parts = append(parts, "Прямой связи события с признаками компании не найдено — новость показана как фон рынка")
	}
	in.WhyItMatters.Text = strings.Join(parts, ". ") + "."
	for _, f := range p.Features {
		if usedFields[f.Field] {
			in.WhyItMatters.ClientFeatures = append(in.WhyItMatters.ClientFeatures, f)
		}
	}
	in.WhyItMatters.Reasons = ctx.Reasons
	in.WhyItMatters.Basis = []string{"N1"}
	for f := range usedFields {
		if f != "—" {
			in.WhyItMatters.Basis = append(in.WhyItMatters.Basis, "C:"+f)
		}
	}
	sort.Strings(in.WhyItMatters.Basis)

	// 3. Последствия: по каналам влияния (кредиты — риск, депозиты — возможность и т.д.).
	known := 0
	for _, m := range ctx.Matches {
		if m.Direction != "unknown" {
			known++
		}
	}
	for _, m := range ctx.Matches {
		if m.Direction == "unknown" && known > 0 {
			continue // фактор упомянут в новости вскользь, направление не ясно — не выдаём за последствие
		}
		chs := m.Channels
		if len(chs) == 0 {
			chs = []ChannelEffect{{Reason: m.Reason, Effect: m.Effect, Impact: m.Impact}}
		}
		for k, ch := range chs {
			if k >= 3 {
				break
			}
			if ch.Effect == "" {
				continue
			}
			basis := []string{"N1"}
			for _, f := range append([]string{ch.Field}, ch.Fields...) {
				if f != "" && f != "—" && !contains(basis, "C:"+f) {
					basis = append(basis, "C:"+f)
				}
			}
			pt := Point{Text: capitalize(ch.Effect) + " (" + strings.ToLower(m.Name) + "; " + ch.Reason + ")", Basis: basis,
				Horizon: horizonFor(m.Factor), Factor: m.Factor, Origin: "model"}
			switch ch.Impact {
			case "risk":
				in.Consequences.Risks = append(in.Consequences.Risks, pt)
			case "opportunity":
				in.Consequences.Opportunities = append(in.Consequences.Opportunities, pt)
			case "mixed":
				pt.Text = "Неоднозначный эффект: " + pt.Text
				in.Consequences.Risks = append(in.Consequences.Risks, pt)
			}
		}
		in.Consequences.FactorsToWatch = append(in.Consequences.FactorsToWatch, "Дальнейшая динамика: "+strings.ToLower(m.Name))
	}
	for sec, share := range ctx.CounterOverlap {
		in.Consequences.Risks = append(in.Consequences.Risks, Point{
			Text:  fmt.Sprintf("Изменение положения контрагентов из отрасли «%s» (%.0f%% оборота) может отразиться на сроках и объёмах платежей", sectorName(sec), share*100),
			Basis: []string{"N1", "C:df_Uid_payment_hack"}, Horizon: "1–6 месяцев", Origin: "model"})
	}
	if contains(e.FactorKeys(), "key_rate") {
		in.Consequences.FactorsToWatch = append(in.Consequences.FactorsToWatch, "Сигналы и риторика Банка России к следующему заседанию")
	}
	for i, c := range ctx.Calendar {
		if i >= 3 {
			break
		}
		in.Consequences.FactorsToWatch = append(in.Consequences.FactorsToWatch,
			fmt.Sprintf("%s — %s (дата из материала банка [%s])", capitalize(dateShort(c.Date)), c.Text, c.ChunkID))
	}
	in.Consequences.FactorsToWatch = uniqueStrings(in.Consequences.FactorsToWatch)

	// 4. Действия.
	added := 0
	for _, m := range ctx.Matches {
		if m.Direction == "unknown" && known > 0 {
			continue
		}
		f := factorIndex[m.Factor]
		if f == nil {
			continue
		}
		for _, a := range f.Actions {
			if added >= 4 {
				break
			}
			in.Actions = append(in.Actions, Action{Text: a, Type: "action", Basis: basisFor(m)})
			added++
			if added%2 == 0 {
				break
			}
		}
	}
	for i, m := range ctx.Matches {
		if m.Direction == "unknown" && known > 0 {
			continue
		}
		if f := factorIndex[m.Factor]; f != nil && len(f.Questions) > 0 && i < 2 {
			in.Actions = append(in.Actions, Action{Text: f.Questions[0], Type: "question"})
		}
	}
	if len(ctx.Matches) > 0 {
		m := ctx.Matches[0]
		in.Actions = append(in.Actions, Action{Type: "scenario", Basis: basisFor(m),
			Text: fmt.Sprintf("Сценарии: базовый — фактор «%s» сохраняет текущую динамику (%s); альтернативный — разворот тренда. Сравните бюджет компании в обоих сценариях.",
				m.Name, directionPhrase(FactorSignal{Factor: m.Factor, Direction: m.Direction}))})
	}
	if len(in.Actions) == 0 {
		in.Actions = append(in.Actions, Action{Text: "Действий не требуется: событие показано для информации", Type: "action"})
	}

	// 5. Позиция банка (может добавить фрагмент-источник цитаты — реестр источников пересобирается).
	in.BankView = bankViewFrom(ctx)
	in.Sources = sourcesFor(ctx)

	// Выводы модели (явно помечены как AI).
	if len(ctx.Matches) > 0 {
		in.ModelConclusions = append(in.ModelConclusions, Point{Origin: "model", Basis: in.WhyItMatters.Basis,
			Text: fmt.Sprintf("Совокупная оценка для компании: %s. Наиболее значимый канал влияния — %s.",
				strings.ToLower(impactLabel[in.Impact]), strings.ToLower(ctx.Matches[0].Name))})
	}
	if in.BankView.Available && len(ctx.Matches) > 0 {
		in.ModelConclusions = append(in.ModelConclusions, Point{Origin: "model",
			Basis: append([]string{in.BankView.Citations[0].ID}, basisFor(ctx.Matches[0])...),
			Text:  "Вывод сопоставлен с позицией аналитиков банка (см. блок «Позиция Альфа-Банка»); применение к компании сделано моделью по её признакам."})
	}

	// Рублёвая оценка по калькулятору — для ставки и курса, если они затрагивают компанию.
	if ctx.KB != nil {
		for _, m := range ctx.Matches {
			if (m.Factor != "key_rate" && m.Factor != "fx_rub") || m.Direction == "unknown" {
				continue
			}
			if in.Scenario == nil {
				in.Scenario = BuildScenarios(p, ctx.KB, nil)
			}
			if pt := scenarioConclusion(in.Scenario, m.Factor); pt != nil {
				for _, b := range pt.Basis {
					switch {
					case strings.HasPrefix(b, "C:"):
						ctx.ExtraFields = append(ctx.ExtraFields, strings.TrimPrefix(b, "C:"))
					case strings.HasPrefix(b, "A"):
						have := false
						for _, h := range ctx.AlfaHits {
							have = have || h.ID == b
						}
						if c, ok := ctx.KB.ChunkByID(b); ok && !have {
							ctx.AlfaHits = append(ctx.AlfaHits, ChunkHit{Chunk: c, Score: 0.5})
						}
					}
				}
				in.ModelConclusions = append(in.ModelConclusions, *pt)
				in.Sources = sourcesFor(ctx)
			}
		}
	}

	in.ProductHint = productHintFor(ctx)
	in.Headline = headlineFor(in, e)
	in.Confidence = confidenceFor(ctx, in)
	return in
}

func headlineFor(in *Insight, e *Event) string {
	return fmt.Sprintf("%s: %s", impactLabel[in.Impact], shorten(e.Title, 110))
}

var quoteSplitRe = regexp.MustCompile(`[.!?]\s+`)

var bankOpinionTerms = []string{"мы ожидаем", "мы полагаем", "мы считаем", "мы рекомендуем", "мы ждем", "мы ждём", "наш прогноз",
	"мы прогнозируем", "мы не исключаем", "мы отдаем предпочтение", "мы сохраняем", "на наш взгляд", "по нашему мнению", "мы меняем",
	"мы повышаем", "мы понижаем", "мы смещаем", "мы ориентируемся", "мы не ждем", "мы не видим", "мы не ожидаем", "мы переставляем"}

// bankViewFrom — позиция банка строится только из найденных фрагментов аналитики.
// Порядок цитирования: сначала свежие материалы, устаревшие — только если других нет (и с пометкой).
func bankViewFrom(ctx *AgentContext) BankView {
	bv := BankView{Label: "Позиция Альфа-Банка (по аналитическим материалам банка)", Forecasts: ctx.Forecasts}
	if bv.Forecasts == nil {
		bv.Forecasts = []ForecastTrack{}
	}
	bv.Revisions = []string{}
	if len(ctx.AlfaHits) == 0 {
		bv.Summary = "В аналитических материалах банка не найдено позиции по этой теме. Вывод ниже сформирован без опоры на экспертизу банка."
		return bv
	}
	bv.Available = true
	hits := append([]ChunkHit{}, ctx.AlfaHits...)
	freshExists := false
	for _, h := range hits {
		if !h.Stale {
			freshExists = true
		}
	}
	keys := ctx.Event.FactorKeys()
	// Главный фактор — самый весомый для компании (первое сопоставление), а не первый по таксономии.
	if len(ctx.Matches) > 0 {
		main := ctx.Matches[0].Factor
		rest := []string{main}
		for _, k := range keys {
			if k != main {
				rest = append(rest, k)
			}
		}
		keys = rest
	}
	var terms, mainTerms []string
	for i, k := range keys {
		if f := factorIndex[k]; f != nil {
			terms = append(terms, f.Keywords...)
			if i == 0 {
				mainTerms = append(append(mainTerms, f.Keywords...), queryKeywords[k]...)
			}
		}
	}
	// Слова, связывающие позицию банка с профилем клиента: выбираем предложения,
	// которые говорят о таких компаниях (заёмщики, экспортёры, отрасль клиента…).
	var clientTerms []string
	c := ctx.Profile.Company
	if c.Products["LOA"] || c.Products["LEA"] {
		clientTerms = append(clientTerms, "заёмщик", "заемщик", "кредит", "заимствован", "долг", "рефинансир", "плавающ")
	}
	if c.Products["DEP"] {
		clientTerms = append(clientTerms, "депозит", "ликвидност", "размещени")
	}
	if c.FTSFlag {
		clientTerms = append(clientTerms, "экспорт", "импорт", "валют", "хеджир")
	}
	if s, ok := sectorIndex[c.Sector]; ok {
		clientTerms = append(clientTerms, s.Keywords...)
	}
	superseded := map[string]string{}
	if ctx.KB != nil {
		for _, h := range hits {
			if notes := ctx.KB.StaleClaims([]string{h.ID}, keys...); len(notes) > 0 {
				superseded[h.ID] = notes[0]
				bv.Revisions = append(bv.Revisions, notes...)
			}
		}
		bv.Revisions = uniqueStrings(bv.Revisions)
	}
	// Сначала свежие и не пересмотренные материалы, затем — с пересмотренным прогнозом, в конце — устаревшие.
	rank := func(h ChunkHit) int {
		switch {
		case h.Stale:
			return 2
		case superseded[h.ID] != "":
			return 1
		}
		return 0
	}
	sort.SliceStable(hits, func(i, j int) bool { return rank(hits[i]) < rank(hits[j]) })
	var summary []string
	// Честность: если ни один найденный фрагмент не говорит о главном факторе события, так и пишем.
	if len(keys) > 0 {
		direct := false
		for _, h := range hits {
			if contains(h.Factors, keys[0]) && !h.Stale {
				direct = true
			}
		}
		if !direct {
			msg := fmt.Sprintf("Прямой позиции банка по теме «%s» в актуальных материалах нет; ниже — ближайшие по смыслу фрагменты.", strings.ToLower(factorName(keys[0])))
			for _, h := range hits {
				if contains(h.Factors, keys[0]) && h.Stale {
					msg = fmt.Sprintf("Позиция банка по теме «%s» есть только в материале от %s — он мог устареть.", strings.ToLower(factorName(keys[0])), dateShort(h.Date))
					break
				}
			}
			summary = append(summary, msg)
		}
	}
	// Актуальный числовой прогноз банка — первым: это то, что руководителю нужно для бюджета.
	for i, t := range ctx.Forecasts {
		if i >= 2 {
			break
		}
		line := fmt.Sprintf("Актуальный прогноз банка: %s — %s%s (материал от %s) [%s].",
			t.Label, fmtNum(t.Current.Value), unitSuffix(t.Unit), dateShort(t.Current.DocDate), t.Current.ChunkID)
		if t.Current.DocDate > bv.AsOf {
			bv.AsOf = t.Current.DocDate
		}
		if t.Revised {
			dir := map[string]string{"up": "повышен", "down": "понижен"}[t.Direction]
			line = strings.TrimSuffix(line, ".") + fmt.Sprintf("; прогноз %s по сравнению с более ранними материалами.", dir)
		}
		summary = append(summary, line)
	}
	for i, h := range hits {
		if i >= 3 {
			break
		}
		if h.Stale && freshExists && i > 0 {
			continue
		}
		type cand struct {
			text, chunk string
			pos         int
		}
		var cands []cand
		for k, s := range splitSentences(h.Text) {
			cands = append(cands, cand{s, h.ID, k})
		}
		// Для главного материала смотрим весь документ: вывод банка («мы ожидаем…») часто
		// стоит в конце, а поиск находит фрагмент с описанием события.
		if i == 0 && ctx.KB != nil && !h.Synthetic {
			for _, c := range ctx.KB.Chunks {
				if c.DocID == h.DocID && c.ID != h.ID && !c.Table && len(ctx.KB.StaleClaims([]string{c.ID}, keys...)) == 0 {
					for k, s := range splitSentences(c.Text) {
						cands = append(cands, cand{s, c.ID, k + 1})
					}
				}
			}
		}
		sents := make([]string, len(cands))
		for k := range cands {
			sents[k] = cands[k].text
		}
		best, bestScore, bestChunk := "", -1, h.ID
		for k, s := range sents {
			// Предложение из соседнего фрагмента документа берём, только если оно о факторах события.
			if cands[k].chunk != h.ID && countAny(s, terms) == 0 {
				continue
			}
			score := 2*min(countAny(s, terms), 3) + 3*min(countAny(s, mainTerms), 2) + 3*countAny(s, clientTerms) - cands[k].pos/3
			// Позиция банка — это его оценка и прогноз («мы ожидаем», «мы рекомендуем»), а не пересказ фактов.
			if containsAny(s, bankOpinionTerms) {
				score += 5
			}
			// Табличные строки («2025 — 16,0%; 2026П — 14,0%…») плохо читаются как позиция — их ниже.
			if len(numRe.FindAllString(s, -1)) > 6 || strings.Count(s, ";") >= 2 {
				score -= 8
			}
			if score > bestScore {
				best, bestScore, bestChunk = s, score, cands[k].chunk
			}
		}
		if bestChunk != h.ID {
			if c, ok := ctx.KB.ChunkByID(bestChunk); ok {
				h = ChunkHit{Chunk: c, Score: h.Score}
				ctx.AlfaHits = append(ctx.AlfaHits, h) // цитата должна быть в реестре источников
				if notes := ctx.KB.StaleClaims([]string{h.ID}, keys...); len(notes) > 0 {
					superseded[h.ID] = notes[0]
				}
			}
		}
		quote := shorten(best, 300)
		cit := Citation{ID: h.ID, DocTitle: h.DocTitle, Quote: quote, Date: h.Date, File: h.File, Page: h.Page, Synthetic: h.Synthetic, Stale: h.Stale}
		if h.Stale {
			cit.Note = "Материал от " + dateShort(h.Date) + " — старше полугода, может не отражать текущую позицию банка."
		}
		if n, ok := superseded[h.ID]; ok {
			cit.Note = strings.TrimSpace(cit.Note + " " + n)
		}
		// Материал написан задолго до события — позиция могла измениться (если есть более свежие цитаты).
		if evT, dT := ctx.Event.PublishedAt(), parseDate(h.Date); cit.Note == "" && evT != nil && dT != nil && evT.Sub(*dT).Hours() > 24*30 && i > 0 {
			cit.Note = "Материал от " + dateShort(h.Date) + " написан до события — позиция банка могла измениться."
		}
		bv.Citations = append(bv.Citations, cit)
		bv.Points = append(bv.Points, Point{Text: quote, Basis: []string{h.ID}, Origin: "bank"})
		if h.Date > bv.AsOf && !h.Stale {
			bv.AsOf = h.Date
		}
		if i == 0 {
			summary = append(summary, quote+" ["+h.ID+"]")
			// Из главного документа берём ещё одно предложение о факторах события, связанное с клиентом.
			for k, s := range sents {
				if s == best || countAny(s, clientTerms) == 0 || countAny(s, terms) == 0 && cands[k].chunk != h.ID {
					continue
				}
				id := cands[k].chunk
				if id != h.ID && ctx.KB != nil {
					if c, ok := ctx.KB.ChunkByID(id); ok {
						ctx.AlfaHits = append(ctx.AlfaHits, ChunkHit{Chunk: c, Score: h.Score})
					}
				}
				summary = append(summary, shorten(s, 300)+" ["+id+"]")
				break
			}
		}
	}
	bv.Summary = strings.Join(summary, " ")
	return bv
}

// productHintFor — нативная реклама банковского продукта (пункт 7 «Пространства решений»),
// только если она обоснована экспозицией компании, и всегда с пометкой «Реклама».
func productHintFor(ctx *AgentContext) *ProductHint {
	if len(ctx.Matches) == 0 {
		return nil
	}
	c := ctx.Profile.Company
	for _, m := range ctx.Matches {
		switch {
		case m.Factor == "key_rate" && c.Products["LOA"] && m.Impact != "opportunity":
			return &ProductHint{Product: "Управление процентным риском", Label: "Реклама",
				Text:   "Банк может помочь зафиксировать стоимость долга: кредит с фиксированной ставкой или процентный своп.",
				Reason: "у компании есть кредитные продукты, а ставка влияет на стоимость долга"}
		case m.Factor == "key_rate" && (c.Products["DEP"] || m.Impact == "risk"):
			return &ProductHint{Product: "Размещение ликвидности", Label: "Реклама",
				Text:   "Пока ставки высокие, свободную ликвидность можно разместить на депозите или неснижаемом остатке.",
				Reason: "высокая ключевая ставка повышает доходность размещения"}
		case m.Factor == "fx_rub" || m.Factor == "sanctions_trade":
			if c.FTSFlag {
				return &ProductHint{Product: "ВЭД и валютные риски", Label: "Реклама",
					Text:   "Для ВЭД-контрактов банк предлагает валютный контроль, конверсию и инструменты хеджирования курса.",
					Reason: "компания ведёт внешнеэкономическую деятельность"}
			}
		case m.Factor == "labor" && !c.Products["ZPP"]:
			return &ProductHint{Product: "Зарплатный проект", Label: "Реклама",
				Text:   "Зарплатный проект помогает удерживать сотрудников за счёт привилегий по картам.",
				Reason: "рост конкуренции за персонал"}
		case (m.Factor == "consumer_demand" || m.Factor == "logistics") && !c.Products["FACTORING"]:
			return &ProductHint{Product: "Факторинг", Label: "Реклама",
				Text:   "Факторинг ускоряет оборачиваемость дебиторской задолженности, если покупатели начнут затягивать оплату.",
				Reason: "изменение спроса может удлинить сроки оплаты"}
		}
	}
	return nil
}

func confidenceFor(ctx *AgentContext, in *Insight) float64 {
	c := 0.35 + 0.25*ctx.Event.Credibility()
	if len(ctx.Matches) > 0 {
		c += 0.15
	}
	if in.BankView.Available {
		c += 0.15
	}
	if len(ctx.Profile.DataQuality) > 0 {
		c -= 0.05 * float64(len(ctx.Profile.DataQuality))
	}
	if ctx.Event.Suspicious {
		c -= 0.2
	}
	return round(clamp(c, 0.05, 0.95), 2)
}

// ---------- самопроверка ----------

var numRe = regexp.MustCompile(`\d+(?:[.,]\d+)?`)

func normNum(s string) string { return strings.ReplaceAll(s, ",", ".") }

// SelfCheckInsight — проверка полноты, обоснованности и непротиворечивости.
func SelfCheckInsight(in *Insight, ctx *AgentContext) SelfCheck {
	sc := SelfCheck{Checks: map[string]bool{}}
	ids := map[string]bool{}
	for _, s := range in.Sources {
		ids[s.ID] = true
	}
	issue := func(check, msg string) {
		sc.Checks[check] = false
		sc.Issues = append(sc.Issues, msg)
	}
	for _, k := range []string{"completeness", "grounding", "numbers", "consistency", "bank_separation", "freshness", "safety"} {
		sc.Checks[k] = true
	}

	// Полнота: ответ на все 6 вопросов.
	if strings.TrimSpace(in.WhatHappened.Summary) == "" {
		issue("completeness", "нет описания события (что произошло)")
	}
	if strings.TrimSpace(in.WhyItMatters.Text) == "" {
		issue("completeness", "нет объяснения, почему событие важно компании")
	}
	if len(in.Consequences.Risks)+len(in.Consequences.Opportunities) == 0 && in.Impact != "neutral" {
		issue("completeness", "не указаны риски или возможности")
	}
	if len(in.Actions) == 0 {
		issue("completeness", "нет действий / вопросов / сценариев")
	}
	if strings.TrimSpace(in.BankView.Summary) == "" {
		issue("completeness", "нет блока позиции банка")
	}
	if !in.BankView.Available && len(ctx.Event.Factors) > 0 && len(ctx.Notes) == 0 {
		issue("completeness", "позиция банка не найдена — нужен расширенный поиск по аналитике")
	}

	// Обоснованность: каждый пункт ссылается на существующий источник.
	checkPoints := func(name string, pts []Point) {
		for _, p := range pts {
			if len(p.Basis) == 0 {
				issue("grounding", name+": пункт без источника — «"+shorten(p.Text, 60)+"»")
				continue
			}
			for _, b := range p.Basis {
				if !ids[b] {
					issue("grounding", name+": ссылка на несуществующий источник "+b)
				}
			}
		}
	}
	checkPoints("риски", in.Consequences.Risks)
	checkPoints("возможности", in.Consequences.Opportunities)
	checkPoints("выводы модели", in.ModelConclusions)
	checkPoints("позиция банка", in.BankView.Points)
	for _, f := range in.WhatHappened.Facts {
		for _, s := range f.SourceIDs {
			if !ids[s] {
				issue("grounding", "факт ссылается на несуществующий источник "+s)
			}
		}
	}

	// Цифры в фактах должны встречаться в первоисточниках (защита от галлюцинаций).
	var corpus strings.Builder
	for _, p := range ctx.Event.Publications {
		corpus.WriteString(p.Content() + " ")
	}
	for _, h := range ctx.AlfaHits {
		corpus.WriteString(h.Text + " ")
	}
	src := normNum(corpus.String())
	idRefRe := regexp.MustCompile(`\[[^\]]*\]`)
	checkNums := func(where, text string) {
		text = idRefRe.ReplaceAllString(text, " ") // ссылки на источники [A2.7] — не числа
		for _, n := range numRe.FindAllString(text, -1) {
			if len(n) >= 2 && !strings.Contains(src, normNum(n)) {
				issue("numbers", fmt.Sprintf("%s: число %s не найдено в источниках", where, n))
			}
		}
	}
	checkNums("что произошло", in.WhatHappened.Summary)
	for _, f := range in.WhatHappened.Facts {
		checkNums("факт", f.Text)
	}
	checkNums("позиция банка", in.BankView.Summary)

	// Непротиворечивость: итоговая оценка согласуется с рисками/возможностями.
	if in.Impact == "risk" && len(in.Consequences.Risks) == 0 {
		issue("consistency", "оценка «риск», но риски не перечислены")
	}
	if in.Impact == "opportunity" && len(in.Consequences.Opportunities) == 0 {
		issue("consistency", "оценка «возможность», но возможности не перечислены")
	}

	// Позиция банка — только из материалов банка.
	for _, c := range in.BankView.Citations {
		if !strings.HasPrefix(c.ID, "A") {
			issue("bank_separation", "в позиции банка цитата не из материалов банка: "+c.ID)
		}
	}

	// Актуальность позиции банка: главная цитата не из устаревшего материала, если есть свежий,
	// и в позиции нет уже пересмотренного прогноза без пометки.
	if len(in.BankView.Citations) > 0 && in.BankView.Citations[0].Stale {
		for _, h := range ctx.AlfaHits {
			if !h.Stale {
				issue("freshness", "позиция банка опирается на устаревший материал при наличии свежего")
				break
			}
		}
	}
	for _, c := range in.BankView.Citations {
		if ctx.KB != nil && len(ctx.KB.StaleClaims([]string{c.ID}, ctx.Event.FactorKeys()...)) > 0 && c.Note == "" {
			issue("freshness", "цитата "+c.ID+" содержит пересмотренный прогноз без пометки")
		}
	}

	// Безопасность: в тексте нет инструкций-инъекций и призывов к платежам.
	all := in.Headline + in.WhatHappened.Summary + in.WhyItMatters.Text + in.BankView.Summary
	for _, a := range in.Actions {
		all += " " + a.Text
	}
	if looksLikeInjection(all) {
		issue("safety", "в ответе обнаружен текст, похожий на внедрённую инструкцию")
	}
	sc.Passed = len(sc.Issues) == 0
	return sc
}
