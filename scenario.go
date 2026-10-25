// Калькулятор «что если»: перевод прогнозов банка в рубли для конкретной компании.
//
// Руководителю мало знать, что «ставка останется высокой». Ему нужно понять, сколько это
// стоит его компании. Калькулятор берёт:
//   - данные клиента (выручка REVENUE_COMPANY, кредитные продукты, ВЭД) — поле-источник C:*;
//   - прогнозы и факты из аналитики банка (ставка и курс по годам, доля плавающих ставок
//     в корпоративных кредитах) — фрагмент-источник A*;
//   - допущения, которых нет в данных (размер долга, доля валютной выручки). Они явно
//     помечены как допущения, и пользователь меняет их ползунками.
//
// Формулы простые и прозрачные (показываются в интерфейсе), считаются кодом, а не LLM.
package main

import (
	"fmt"
	"math"
	"strings"
)

type Assumption struct {
	Key    string  `json:"key"`
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Step   float64 `json:"step"`
	Kind   string  `json:"kind"`   // client (данные клиента) | bank (аналитика банка) | assumption (допущение)
	Source string  `json:"source"` // пояснение происхождения значения
	Basis  string  `json:"basis,omitempty"`
}

type ScenarioDef struct {
	ID      string   `json:"id"`
	Factor  string   `json:"factor"`
	Title   string   `json:"title"`
	Base    float64  `json:"base"`
	Alt     float64  `json:"alt"`
	Unit    string   `json:"unit"`
	Formula string   `json:"formula"`
	Basis   []string `json:"basis"`
}

type ScenarioResult struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	DeltaRub   float64 `json:"delta_rub"`   // эффект на прибыль до налогов за год, ₽ (+ хорошо, − плохо)
	PctRevenue float64 `json:"pct_revenue"` // в % выручки
	Text       string  `json:"text"`
}

type ScenarioPack struct {
	CompanyID   string           `json:"company_id"`
	Assumptions []Assumption     `json:"assumptions"`
	Scenarios   []ScenarioDef    `json:"scenarios"`
	Results     []ScenarioResult `json:"results"`
	Notes       []string         `json:"notes"`
}

// findFactChunk — первый свежий фрагмент аналитики, где встречаются все термины.
func (kb *KnowledgeBase) findFactChunk(terms ...string) (Chunk, bool) {
	for _, c := range kb.Chunks {
		if c.Synthetic || c.Stale {
			continue
		}
		low := strings.ToLower(c.Text)
		ok := true
		for _, t := range terms {
			if !strings.Contains(low, strings.ToLower(t)) {
				ok = false
				break
			}
		}
		if ok {
			return c, true
		}
	}
	return Chunk{}, false
}

// BuildScenarios — допущения, сценарии и результаты для компании (overrides — значения ползунков).
func BuildScenarios(p *CompanyProfile, kb *KnowledgeBase, overrides map[string]float64) *ScenarioPack {
	c := p.Company
	pack := &ScenarioPack{CompanyID: c.ID}
	set := func(a Assumption) {
		if v, ok := overrides[a.Key]; ok && v >= a.Min && v <= a.Max {
			a.Value = v
			if a.Kind == "assumption" {
				a.Source = "введено пользователем"
			}
		}
		pack.Assumptions = append(pack.Assumptions, a)
	}
	revBn := c.Revenue / 1e9
	if c.RevenueKnown && revBn > 0 {
		set(Assumption{Key: "revenue", Name: "Выручка", Value: round(revBn, 1), Unit: "млрд ₽", Min: 0.1, Max: math.Max(1000, revBn*2), Step: 0.1,
			Kind: "client", Source: "данные клиента", Basis: "C:REVENUE_COMPANY"})
	} else {
		set(Assumption{Key: "revenue", Name: "Выручка", Value: 20, Unit: "млрд ₽", Min: 0.1, Max: 1000, Step: 0.1,
			Kind: "assumption", Source: "выручки нет в данных — введите фактическое значение"})
		pack.Notes = append(pack.Notes, "Выручки нет в данных клиента: расчёт в рублях условный, введите выручку.")
	}
	rev := pack.Assumptions[0].Value

	borrower := c.Products["LOA"] || c.Products["LEA"] || c.Products["FACTORING"]
	debtDefault := 0.0
	if borrower {
		debtDefault = round(rev*0.3, 1)
	}
	set(Assumption{Key: "debt", Name: "Долг компании", Value: debtDefault, Unit: "млрд ₽", Min: 0, Max: math.Max(rev*3, 10), Step: 0.1,
		Kind: "assumption", Source: map[bool]string{true: "допущение: 30% выручки (у компании есть кредитные продукты [C:CL_PRODUCT_LOA_MARK]) — уточните", false: "кредитных продуктов в данных нет — при наличии долга введите сумму"}[borrower]})

	floating := Assumption{Key: "floating", Name: "Доля долга с плавающей ставкой", Value: 67, Unit: "%", Min: 0, Max: 100, Step: 1,
		Kind: "assumption", Source: "допущение"}
	if ch, ok := kb.findFactChunk("плавающей ставке", "67%"); ok {
		floating.Kind, floating.Basis = "bank", ch.ID
		floating.Source = "в среднем по рынку: 67% корпоративных кредитов — по плавающей ставке (аналитика банка)"
	}
	set(floating)

	exportDefault, importDefault := 0.0, 0.0
	if c.FTSFlag {
		// Экспортёр или импортёр — по знаку чувствительности компании к курсу (отрасль + специализация):
		// если укрепление рубля компании выгодно, она скорее импортёр.
		exportDefault = 30
		if ex := p.Exposure("fx_rub"); ex != nil && ex.Sign > 0.1 {
			exportDefault, importDefault = 5, 30
		}
	}
	set(Assumption{Key: "export", Name: "Доля валютной выручки", Value: exportDefault, Unit: "% выручки", Min: 0, Max: 100, Step: 1,
		Kind: "assumption", Source: map[bool]string{true: "допущение по данным о ВЭД [C:FTS_FLAG] и отрасли — уточните", false: "ВЭД в данных нет"}[c.FTSFlag]})
	set(Assumption{Key: "import", Name: "Доля валютных затрат", Value: importDefault, Unit: "% выручки", Min: 0, Max: 100, Step: 1,
		Kind: "assumption", Source: map[bool]string{true: "допущение по данным о ВЭД [C:FTS_FLAG] и отрасли — уточните", false: "ВЭД в данных нет"}[c.FTSFlag]})

	val := func(k string) float64 {
		for _, a := range pack.Assumptions {
			if a.Key == k {
				return a.Value
			}
		}
		return 0
	}

	// Сценарии из прогнозов банка.
	byInd := map[string][]ForecastTrack{}
	for _, t := range kb.Tracks("") {
		byInd[t.Indicator] = append(byInd[t.Indicator], t)
	}
	if kr := byInd["key_rate"]; len(kr) >= 2 {
		a, b := kr[0], kr[len(kr)-1]
		pack.Scenarios = append(pack.Scenarios,
			ScenarioDef{ID: "rate_forecast", Factor: "key_rate", Unit: "%", Base: a.Current.Value, Alt: b.Current.Value,
				Title:   fmt.Sprintf("Ставка снижается по прогнозу банка: %s%% → %s%% (конец %s → конец %s г.); если снижения не будет — эта сумма ляжет сверх бюджета", fmtNum(a.Current.Value), fmtNum(b.Current.Value), a.Period, b.Period),
				Formula: "долг × доля плавающих × (ставка_было − ставка_стало)", Basis: []string{a.Current.ChunkID, b.Current.ChunkID}},
		)
	}
	pack.Scenarios = append(pack.Scenarios, ScenarioDef{ID: "rate_shock", Factor: "key_rate", Unit: "п.п.", Base: 0, Alt: 1,
		Title: "Стресс: ставка выше на 1 п.п.", Formula: "долг × доля плавающих × 1 п.п."})
	if fx := byInd["usd_rub"]; len(fx) >= 2 {
		a, b := fx[0], fx[len(fx)-1]
		pack.Scenarios = append(pack.Scenarios, ScenarioDef{ID: "fx_forecast", Factor: "fx_rub", Unit: "руб./$", Base: a.Current.Value, Alt: b.Current.Value,
			Title:   fmt.Sprintf("Курс по прогнозу банка: %s → %s руб./$ (конец %s → конец %s г.)", fmtNum(a.Current.Value), fmtNum(b.Current.Value), a.Period, b.Period),
			Formula: "выручка × (доля валютной выручки − доля валютных затрат) × (курс_стало / курс_было − 1)", Basis: []string{a.Current.ChunkID, b.Current.ChunkID}})
	}
	pack.Scenarios = append(pack.Scenarios, ScenarioDef{ID: "fx_shock", Factor: "fx_rub", Unit: "%", Base: 0, Alt: -10,
		Title: "Стресс: рубль укрепляется на 10%", Formula: "выручка × (доля валютной выручки − доля валютных затрат) × (−10%)"})

	debt, fl, ex, im := val("debt"), val("floating")/100, val("export")/100, val("import")/100
	for _, s := range pack.Scenarios {
		var d float64
		switch s.ID {
		case "rate_forecast":
			d = debt * fl * (s.Base - s.Alt) / 100
		case "rate_shock":
			d = -debt * fl * s.Alt / 100
		case "fx_forecast":
			d = rev * (ex - im) * (s.Alt/s.Base - 1)
		case "fx_shock":
			d = rev * (ex - im) * s.Alt / 100
		}
		r := ScenarioResult{ID: s.ID, Title: s.Title, DeltaRub: round(d*1e9, 0)}
		if rev > 0 {
			r.PctRevenue = round(d/rev*100, 2)
		}
		switch {
		case math.Abs(d) < 0.005:
			r.Text = "эффект несущественный при текущих допущениях"
		case d > 0:
			r.Text = fmt.Sprintf("+%s в год (%s%% выручки)", formatRub(d*1e9), pctRu(r.PctRevenue))
		default:
			r.Text = fmt.Sprintf("−%s в год (%s%% выручки)", formatRub(-d*1e9), pctRu(r.PctRevenue))
		}
		pack.Results = append(pack.Results, r)
	}
	pack.Notes = append(pack.Notes, "Оценка ориентировочная: эффект на прибыль до налогов за год без учёта хеджирования, графика погашений и пересмотра цен. Допущения меняются ползунками.")
	return pack
}

// scenarioConclusion — вывод AI с рублёвой оценкой для инсайта (только для факторов события).
func scenarioConclusion(pack *ScenarioPack, factor string) *Point {
	if pack == nil {
		return nil
	}
	var parts []string
	var basis []string
	for i, s := range pack.Scenarios {
		if s.Factor != factor || pack.Results[i].Text == "" || strings.HasPrefix(pack.Results[i].Text, "эффект несущественный") {
			continue
		}
		parts = append(parts, s.Title+": "+pack.Results[i].Text)
		basis = append(basis, s.Basis...)
	}
	if len(parts) == 0 {
		return nil
	}
	for _, a := range pack.Assumptions {
		if a.Basis != "" && (factor == "key_rate" && (a.Key == "floating" || a.Key == "revenue") || factor == "fx_rub" && a.Key == "revenue") {
			basis = append(basis, a.Basis)
		}
	}
	txt := "Оценка в рублях (калькулятор, на допущениях — уточните их ниже): " + strings.Join(parts, "; ") + "."
	return &Point{Text: txt, Basis: uniqueStrings(basis), Origin: "model", Factor: factor}
}

func pctRu(v float64) string {
	return strings.ReplaceAll(fmt.Sprintf("%.2f", math.Abs(v)), ".", ",")
}
