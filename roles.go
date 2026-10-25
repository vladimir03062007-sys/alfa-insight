// Роли пользователей. Сервис персонализируется не только под компанию,
// но и под человека: у финдиректора и главбуха разные информационные потребности.
package main

type Role struct {
	Key         string             `json:"key"`
	Name        string             `json:"name"`
	Focus       string             `json:"focus"`        // подсказка для LLM: на что смотреть
	FactorBoost map[string]float64 `json:"factor_boost"` // множители важности факторов
}

var roles = []Role{
	{Key: "owner", Name: "Собственник бизнеса",
		Focus:       "стоимость бизнеса, стратегические риски, отрасль и конкуренты, дивиденды, сделки M&A",
		FactorBoost: map[string]float64{"gdp_growth": 1.3, "sanctions_trade": 1.2, "regulation": 1.1, "key_rate": 1.1}},
	{Key: "ceo", Name: "Генеральный директор",
		Focus:       "спрос, операционная деятельность, цепочки поставок, конкуренты, стратегия",
		FactorBoost: map[string]float64{"consumer_demand": 1.3, "logistics": 1.2, "sanctions_trade": 1.2, "gdp_growth": 1.2, "labor": 1.1}},
	{Key: "cfo", Name: "Финансовый директор",
		Focus:       "стоимость долга, ликвидность, валютные риски, бюджет и ковенанты",
		FactorBoost: map[string]float64{"key_rate": 1.4, "credit_market": 1.4, "fx_rub": 1.3, "inflation": 1.1, "taxes": 1.1}},
	{Key: "chief_accountant", Name: "Главный бухгалтер",
		Focus:       "налоги, отчётность, изменения законодательства и сроки их вступления в силу",
		FactorBoost: map[string]float64{"taxes": 1.6, "regulation": 1.5, "labor": 1.1}},
	{Key: "treasury", Name: "Руководитель казначейства",
		Focus:       "ликвидность, размещение средств, валютные позиции, процентные ставки, платёжные ограничения",
		FactorBoost: map[string]float64{"key_rate": 1.5, "fx_rub": 1.5, "credit_market": 1.3, "sanctions_trade": 1.2}},
	{Key: "clevel", Name: "Другой C-level руководитель",
		Focus:       "влияние событий на своё направление и компанию в целом",
		FactorBoost: map[string]float64{}},
	{Key: "employee", Name: "Сотрудник, влияющий на решения",
		Focus:       "понятные выводы и действия, которые нужно передать руководству",
		FactorBoost: map[string]float64{}},
}

func getRole(key string) Role {
	for _, r := range roles {
		if r.Key == key {
			return r
		}
	}
	return roles[2] // по умолчанию — финансовый директор
}

func (r Role) Boost(factor string) float64 {
	if v, ok := r.FactorBoost[factor]; ok {
		return v
	}
	return 1.0
}
