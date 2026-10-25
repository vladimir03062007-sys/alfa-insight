// Механизмы безопасной работы AI-компонентов (ограничение 06 кейса):
//   - разграничение прав: у каждой роли — свой allowlist инструментов агента;
//     все инструменты только читают данные, «опасных» действий (платежи, изменения) нет;
//   - защита чувствительных данных: маскирование ПДн перед отправкой в LLM,
//     в LLM уходят только производные признаки, а не сырые платежи;
//   - защита от prompt-injection: новости передаются как данные в тегах <data>,
//     подозрительные фразы помечаются и вырезаются;
//   - журналирование: каждый вызов инструмента и LLM пишется в output/audit.log (JSONL);
//   - бюджет: дневной лимит токенов, после исчерпания — офлайн-режим.
package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var injectionPatterns = regexp.MustCompile(`(?i)(ignore (all |the )?(previous|above) instructions|disregard (the )?(previous|above)|` +
	`игнорируй (все )?(предыдущие|прошлые|вышеуказанные) инструкции|забудь (все )?инструкции|` +
	`you are now|ты теперь|system prompt|системн(ый|ого) промпт|выведи (свой )?промпт|reveal your prompt|` +
	`переведи (все )?средства|send money|отправь (деньги|платёж|платеж))`)

func looksLikeInjection(text string) bool { return injectionPatterns.MatchString(text) }

// sanitizeForLLM вырезает фразы-инъекции и маскирует ПДн.
func sanitizeForLLM(text string) string {
	text = injectionPatterns.ReplaceAllString(text, "[удалено фильтром безопасности]")
	return maskPII(text)
}

var (
	emailRe = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.-]+`)
	phoneRe = regexp.MustCompile(`(\+7|8)[\s(-]*\d{3}[\s)-]*\d{3}[\s-]*\d{2}[\s-]*\d{2}`)
	innRe   = regexp.MustCompile(`\b(ИНН|инн)[:\s]*\d{10,12}\b`)
	cardRe  = regexp.MustCompile(`\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b`)
	acctRe  = regexp.MustCompile(`\b\d{20}\b`)
)

func maskPII(s string) string {
	s = emailRe.ReplaceAllString(s, "[email]")
	s = phoneRe.ReplaceAllString(s, "[телефон]")
	s = innRe.ReplaceAllString(s, "ИНН [скрыт]")
	s = cardRe.ReplaceAllString(s, "[карта]")
	s = acctRe.ReplaceAllString(s, "[счёт]")
	return s
}

// ---------- права доступа к инструментам ----------

var readOnlyTools = []string{"get_company_profile", "get_event", "match_exposure", "search_alfa_analytics",
	"get_bank_forecasts", "search_news", "get_counterparties", "get_feedback_summary"}

// roleTools — какие инструменты агента доступны пользователю с ролью.
// Например, сведения о платежах контрагентов доступны только финансовым ролям и руководителям.
var roleTools = map[string][]string{
	"owner":            readOnlyTools,
	"ceo":              readOnlyTools,
	"cfo":              readOnlyTools,
	"treasury":         readOnlyTools,
	"chief_accountant": readOnlyTools,
	"clevel":           {"get_company_profile", "get_event", "match_exposure", "search_alfa_analytics", "get_bank_forecasts", "search_news"},
	"employee":         {"get_company_profile", "get_event", "match_exposure", "search_alfa_analytics", "get_bank_forecasts", "search_news"},
}

func toolAllowed(role, tool string) bool {
	list, ok := roleTools[role]
	if !ok {
		list = roleTools["employee"]
	}
	return contains(list, tool)
}

// ---------- аудит ----------

type AuditRecord struct {
	Time    string `json:"time"`
	Kind    string `json:"kind"` // tool | llm | api | denied | budget
	Actor   string `json:"actor"`
	Company string `json:"company,omitempty"`
	Name    string `json:"name"`
	Args    string `json:"args,omitempty"`
	Result  string `json:"result,omitempty"`
}

var auditMu sync.Mutex

func audit(rec AuditRecord) {
	rec.Time = time.Now().UTC().Format(time.RFC3339)
	rec.Args = maskPII(truncRunes(rec.Args, 500))
	rec.Result = truncRunes(rec.Result, 300)
	data, _ := json.Marshal(rec)
	auditMu.Lock()
	defer auditMu.Unlock()
	f, err := os.OpenFile(cfg.AuditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

// ---------- бюджет токенов ----------

type usageState struct {
	Date             string             `json:"date"`
	PromptTokens     int                `json:"prompt_tokens"`
	CompletionTokens int                `json:"completion_tokens"`
	Calls            int                `json:"calls"`
	CacheHits        int                `json:"cache_hits"`
	ByModel          map[string]int     `json:"by_model"`
	CostRub          float64            `json:"cost_rub"` // по usage.cost шлюза, иначе по ценам LLM_PRICE_*
	CostByModel      map[string]float64 `json:"cost_by_model"`
	CostFromGateway  bool               `json:"cost_from_gateway"` // стоимость пришла от шлюза организаторов
}

// costRub — стоимость вызова по ценам из настроек (0, если цена модели не задана).
func costRub(model string, prompt, completion int) float64 {
	p, ok := cfg.LLMPrices[model]
	if !ok {
		return 0
	}
	return (float64(prompt)*p[0] + float64(completion)*p[1]) / 1e6
}

// budgetAllows — проверка лимитов до вызова: по токенам и, если известны цены, по рублям.
// Лимит 500 ₽/день общий для всей команды и всех моделей, а этот счётчик видит только
// свой экземпляр сервиса, поэтому LLM_DAILY_BUDGET_RUB — доля бюджета на экземпляр.
func budgetAllows(model string, prompt, maxCompletion int) bool {
	usageMu.Lock()
	defer usageMu.Unlock()
	u := currentUsage()
	if u.PromptTokens+u.CompletionTokens+prompt+maxCompletion > cfg.LLMDailyTokenLimit {
		return false
	}
	if _, ok := cfg.LLMPrices[model]; ok && u.CostRub+costRub(model, prompt, maxCompletion) > cfg.LLMDailyBudgetRub {
		return false
	}
	// Фактический расход по usage.cost шлюза: лимит работает и без заданных цен.
	if cfg.LLMDailyBudgetRub > 0 && u.CostRub >= cfg.LLMDailyBudgetRub {
		return false
	}
	return true
}

var (
	usageMu  sync.Mutex
	llmUsage *usageState
)

func usagePath() string { return cfg.StateDir + "/llm_usage.json" }

func currentUsage() *usageState {
	today := time.Now().Format("2006-01-02")
	if llmUsage == nil {
		llmUsage = &usageState{}
		_ = readJSON(usagePath(), llmUsage)
	}
	if llmUsage.Date != today {
		llmUsage = &usageState{Date: today, ByModel: map[string]int{}}
	}
	if llmUsage.ByModel == nil {
		llmUsage.ByModel = map[string]int{}
	}
	return llmUsage
}

func budgetLeft() int {
	usageMu.Lock()
	defer usageMu.Unlock()
	u := currentUsage()
	return cfg.LLMDailyTokenLimit - u.PromptTokens - u.CompletionTokens
}

// recordUsage учитывает вызов. apiCost — стоимость из ответа шлюза организаторов (usage.cost, ₽);
// если шлюз её не вернул, стоимость считается по ценам LLM_PRICE_*.
func recordUsage(model string, prompt, completion int, cacheHit bool, apiCost float64) float64 {
	usageMu.Lock()
	defer usageMu.Unlock()
	u := currentUsage()
	cost := 0.0
	if cacheHit {
		u.CacheHits++
	} else {
		cost = apiCost
		if cost > 0 {
			u.CostFromGateway = true
		} else {
			cost = costRub(model, prompt, completion)
		}
		if u.CostByModel == nil {
			u.CostByModel = map[string]float64{}
		}
		u.CostByModel[model] += cost
		u.PromptTokens += prompt
		u.CompletionTokens += completion
		u.Calls++
		u.ByModel[model] += prompt + completion
		u.CostRub += cost
	}
	_ = writeJSON(usagePath(), u)
	return cost
}

// llmCostSource — откуда берётся расход в рублях: gateway (usage.cost шлюза), prices (LLM_PRICE_*) или none.
func llmCostSource() string {
	u := usageSnapshot()
	switch {
	case u.CostFromGateway, u.CostRub > 0 && len(cfg.LLMPrices) == 0:
		return "gateway"
	case len(cfg.LLMPrices) > 0:
		return "prices"
	case cfg.LLMEnabled() && u.Calls == 0:
		return "gateway" // шлюз организаторов сообщает стоимость в каждом ответе; вызовов сегодня ещё не было
	}
	return "none"
}

func usageSnapshot() usageState {
	usageMu.Lock()
	defer usageMu.Unlock()
	return *currentUsage()
}

// wrapData — оборачивает недоверенный текст (новости, материалы) в теги данных.
func wrapData(label, text string) string {
	text = strings.ReplaceAll(text, "</data>", "")
	return "<data source=\"" + label + "\">\n" + sanitizeForLLM(text) + "\n</data>"
}
