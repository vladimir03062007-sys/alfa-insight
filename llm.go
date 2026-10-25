// Клиент языковых моделей (OpenAI-совместимый Chat Completions API).
//
// Организаторы выдают общий ключ к разрешённым моделям (Приложение 3).
// Клиент:
//   - не даёт вызвать модель вне списка разрешённых (LLM_ALLOWED_MODELS);
//   - маршрутизирует задачи по моделям: fast / reasoning / general / research;
//   - кэширует ответы на диске и ведёт дневной учёт токенов (лимит бюджета);
//   - журналирует каждый вызов в audit.log.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type LLMResult struct {
	Text             string  `json:"text"`
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostRub          float64 `json:"cost_rub"`
	FallbackFrom     string  `json:"fallback_from,omitempty"`
	Cached           bool    `json:"cached"`
	DurationMs       int64   `json:"duration_ms"`
}

var ErrBudget = errors.New("дневной лимит токенов исчерпан")
var ErrNoLLM = errors.New("LLM не настроена (нет LLM_BASE_URL / LLM_API_KEY)")

// Роли моделей (по описанию из Приложения 3 кейса):
//
//	fast      — Gemma 4 26B A4B: невысокая сложность (план агента, классификация, извлечение);
//	reasoning — GLM-5.3-Flash: глубокий анализ и рассуждения (интерпретация события);
//	general   — Qwen3.6 35B A3B: универсальная (ответы на вопросы, резерв);
//	research  — DeepSeek-V4.1-Flash: поиск взаимосвязей, большие объёмы (вопросы по всей ленте, критик-проверяющий);
//	text      — Kimi-K2.6: обработка и анализ текста (резерв писателя; критиком не ставим — на шлюзе дорого рассуждает);
//	vision    — Qwen3-VL-30B-A3B-Instruct: изображения (графики/сканы в PDF аналитики банка).
func modelForRole(role string) string {
	switch role {
	case "fast":
		return cfg.LLMModelFast
	case "general":
		return cfg.LLMModelGeneral
	case "research":
		return cfg.LLMModelResearch
	case "text":
		return cfg.LLMModelText
	case "vision":
		return cfg.LLMModelVision
	}
	return cfg.LLMModelReasoning
}

var modelRoles = []string{"fast", "reasoning", "general", "research", "text", "vision"}

// taskChains — какая модель решает задачу и какие подключаются при её недоступности.
// Ключ один на все модели, поэтому модель выбирается в каждом запросе.
// Критик намеренно — другая модель, чем писатель: вывод не проверяет сам себя.
var taskChains = map[string][]string{
	"plan":     {"fast", "general"},
	"write":    {"reasoning", "text", "general"},
	"critic":   {"research", "fast"},
	"answer":   {"general", "reasoning"},
	"research": {"research", "general"},
	"vision":   {"vision"},
}

// modelFor — основная модель задачи (или роли).
func modelFor(task string) string {
	if chain, ok := taskChains[task]; ok {
		return modelForRole(chain[0])
	}
	return modelForRole(task)
}

// thinkingModel — модели с режимом рассуждений: GLM, Kimi, Qwen3.6, DeepSeek.
func thinkingModel(model string) bool {
	m := strings.ToLower(model)
	for _, p := range []string{"glm", "kimi", "qwen3.6", "deepseek"} {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

func modelAllowed(model string) bool {
	m := strings.ToLower(model)
	for _, a := range cfg.LLMAllowedModels {
		if strings.Contains(m, a) {
			return true
		}
	}
	return false
}

var llmHTTP = &http.Client{}

// Chat — вызов модели для задачи с переходом на резервную модель при сбое
// (кроме исчерпания бюджета и ошибок авторизации — их резерв не исправит).
func Chat(task string, messages []ChatMessage, maxTokens int, actor string) (*LLMResult, error) {
	chain, ok := taskChains[task]
	if !ok {
		chain = []string{task}
	}
	var lastErr error
	for i, role := range chain {
		res, err := chatModel(modelForRole(role), messages, estimateTokens(messages), maxTokens, actor)
		if err == nil {
			if i > 0 {
				res.FallbackFrom = modelForRole(chain[0])
			}
			return res, nil
		}
		lastErr = err
		if errors.Is(err, ErrBudget) || errors.Is(err, ErrNoLLM) || errors.Is(err, errAuth) {
			break
		}
		logWarn("модель %s недоступна (%v), пробую резервную", modelForRole(role), err)
	}
	return nil, lastErr
}

var errAuth = errors.New("ключ не принят API (401/403)")

// reasoningMaxTokens — лимит для повтора, когда рассуждения модели не оставили места для ответа.
const reasoningMaxTokens = 12000

// chatModel — один запрос к конкретной модели. messages — []ChatMessage или
// мультимодальные сообщения (для vision-модели), estTokens — оценка входа для бюджета.
func chatModel(model string, messages any, estTokens, maxTokens int, actor string) (*LLMResult, error) {
	if !cfg.LLMEnabled() {
		return nil, ErrNoLLM
	}
	if !modelAllowed(model) {
		audit(AuditRecord{Kind: "denied", Actor: actor, Name: "llm:" + model, Result: "модель не входит в список разрешённых"})
		return nil, fmt.Errorf("модель %q не входит в список разрешённых кейсом", model)
	}
	payload := map[string]any{
		"model": model, "messages": messages, "temperature": cfg.LLMTemperature,
		"max_tokens": maxTokens, "stream": false,
	}
	if !cfg.LLMThinking && thinkingModel(model) {
		// Рассуждения оплачиваются как выход и съедают max_tokens: на шлюзе организаторов они
		// в 4–7 раз удорожают вызов. Gemma и Qwen3-VL не трогаем: reasoning_effort у Gemma, наоборот, включает рассуждения.
		payload["thinking"] = map[string]string{"type": "disabled"}
		payload["reasoning_effort"] = "low"
	}
	body, _ := json.Marshal(payload)
	sum := sha256.Sum256(body)
	key := hex.EncodeToString(sum[:])[:32]
	cachePath := cfg.StateDir + "/llm_cache/" + key + ".json"

	if cfg.LLMCache {
		var cached LLMResult
		if err := readJSON(cachePath, &cached); err == nil && cached.Text != "" {
			cached.Cached = true
			cached.CostRub = 0
			recordUsage(model, 0, 0, true, 0)
			return &cached, nil
		}
	}
	if !budgetAllows(model, estTokens, maxTokens) {
		audit(AuditRecord{Kind: "budget", Actor: actor, Name: "llm:" + model, Result: "лимит токенов"})
		return nil, ErrBudget
	}

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, cfg.LLMBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.LLMAPIKey)
	llmHTTP.Timeout = time.Duration(cfg.LLMTimeoutSec) * time.Second
	resp, err := llmHTTP.Do(req)
	if err != nil {
		audit(AuditRecord{Kind: "llm", Actor: actor, Name: model, Result: "ошибка: " + err.Error()})
		return nil, fmt.Errorf("LLM недоступна: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, fmt.Errorf("%w: %s", errAuth, truncRunes(string(data), 200))
	}
	if resp.StatusCode >= 400 {
		audit(AuditRecord{Kind: "llm", Actor: actor, Name: model, Result: fmt.Sprintf("HTTP %d", resp.StatusCode)})
		return nil, fmt.Errorf("LLM вернула HTTP %d: %s", resp.StatusCode, truncRunes(string(data), 400))
	}
	var r struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			Cost             float64 `json:"cost"` // стоимость вызова от шлюза, ₽
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("некорректный ответ LLM: %w", err)
	}
	if len(r.Choices) == 0 {
		return nil, fmt.Errorf("LLM вернула пустой ответ")
	}
	text := stripThinking(r.Choices[0].Message.Content)
	if strings.TrimSpace(text) == "" {
		// Рассуждающие модели (GLM, Kimi, Qwen3.6, DeepSeek) тратят max_tokens и на reasoning:
		// если лимит кончился до ответа, повторяем один раз с запасом, иначе — ошибка, и Chat
		// переходит к резервной модели. Пустой ответ не кэшируется.
		if r.Choices[0].FinishReason == "length" && maxTokens < reasoningMaxTokens {
			logWarn("модель %s израсходовала лимит %d токенов на рассуждения, повтор с %d", model, maxTokens, reasoningMaxTokens)
			return chatModel(model, messages, estTokens, reasoningMaxTokens, actor)
		}
		audit(AuditRecord{Kind: "llm", Actor: actor, Name: model, Result: "пустой ответ, finish_reason=" + r.Choices[0].FinishReason})
		return nil, fmt.Errorf("LLM вернула пустой ответ (finish_reason=%s)", r.Choices[0].FinishReason)
	}
	res := &LLMResult{Text: text, Model: model, PromptTokens: r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens, DurationMs: time.Since(start).Milliseconds()}
	if res.PromptTokens == 0 {
		res.PromptTokens = estTokens
		res.CompletionTokens = runeLen(text) / 3
	}
	res.CostRub = recordUsage(model, res.PromptTokens, res.CompletionTokens, false, r.Usage.Cost)
	audit(AuditRecord{Kind: "llm", Actor: actor, Name: model,
		Result: fmt.Sprintf("ok, tokens=%d+%d, %.3f ₽, %dms", res.PromptTokens, res.CompletionTokens, res.CostRub, res.DurationMs)})
	if cfg.LLMCache {
		_ = os.MkdirAll(cfg.StateDir+"/llm_cache", 0o755)
		_ = writeJSON(cachePath, res)
	}
	return res, nil
}

func estimateTokens(msgs []ChatMessage) int {
	n := 0
	for _, m := range msgs {
		n += runeLen(m.Content)/3 + 4
	}
	return n
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThinking убирает блоки рассуждений, которые возвращают некоторые модели.
func stripThinking(s string) string {
	return strings.TrimSpace(thinkRe.ReplaceAllString(s, ""))
}

// extractJSON достаёт первый JSON-объект из ответа модели (в т.ч. из ```json ... ```).
func extractJSON(raw string, out any) error {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		s = strings.TrimPrefix(s, "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return fmt.Errorf("в ответе модели нет JSON")
	}
	open, close := s[start], byte('}')
	if open == '[' {
		close = ']'
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && c == open:
			depth++
		case !inStr && c == close:
			depth--
			if depth == 0 {
				return json.Unmarshal([]byte(s[start:i+1]), out)
			}
		}
	}
	return fmt.Errorf("JSON в ответе модели обрезан")
}

// ListModels — список моделей, доступных по ключу (GET {base}/models).
func ListModels() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, cfg.LLMBaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.LLMAPIKey)
	llmHTTP.Timeout = 30 * time.Second
	resp, err := llmHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncRunes(string(data), 300))
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range r.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// VisionTranscribe — расшифровка страницы-изображения (график, таблица, скан) моделью Qwen3-VL.
func VisionTranscribe(png []byte, actor string) (*LLMResult, error) {
	msgs := []map[string]any{
		{"role": "system", "content": "Ты расшифровываешь страницы аналитических отчётов банка. Перепиши весь текст страницы и кратко опиши графики и таблицы: что показано, ключевые значения и выводы. Ничего не добавляй от себя. Ответ — обычный текст на русском."},
		{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "Расшифруй страницу."},
			{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)}},
		}},
	}
	return chatModel(modelForRole("vision"), msgs, 1500, 1500, actor)
}
