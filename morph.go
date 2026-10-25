// Лёгкая морфология русского языка без внешних библиотек.
//
// Ключевые слова таксономии записаны основами («курс рубл», «ключевую ставк»).
// Раньше они искались как точная подстрока, поэтому «о курсе рубля» не находилось
// по «курс рубл», а «ключевой ставки» — по «ключевую ставк». Теперь многословный
// термин сравнивается пословно: каждое слово термина приводится к основе
// (отбрасывается окончание), и слово текста должно начинаться с этой основы.
// Между словами термина допускается одно «лишнее» слово
// («курс российского рубля» находится по «курс рубл»).
//
// Те же основы используются в TF-IDF (embeddings.go), поэтому RAG-поиск по
// аналитике банка находит «ставки» по запросу «ставка».
package main

import (
	"strings"
	"unicode"
)

// Окончания, от длинных к коротким. Это не полный стеммер Портера,
// а безопасный минимум: основа должна остаться не короче 3 букв.
var ruEndings = []string{
	"иями", "ями", "ами", "ого", "его", "ому", "ему", "ыми", "ими", "ией", "ьей",
	"ой", "ей", "ий", "ый", "ая", "яя", "ое", "ее", "ую", "юю", "ом", "ем", "ах", "ях",
	"ов", "ев", "ам", "ям", "ия", "ие", "ию", "ии", "ью", "ым", "им", "их", "ых",
	"а", "я", "о", "е", "у", "ю", "ы", "и", "ь", "й",
}

// stemRu отбрасывает окончание у русского слова (латиница и числа не меняются).
func stemRu(w string) string {
	r := []rune(w)
	if len(r) < 4 || !isCyrillic(r[0]) {
		return w
	}
	for _, end := range ruEndings {
		er := []rune(end)
		if len(r)-len(er) >= 3 && strings.HasSuffix(w, end) {
			return string(r[:len(r)-len(er)])
		}
	}
	return w
}

func isCyrillic(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }

// wordTokens — слова текста в нижнем регистре (буквы и цифры; дефис разделяет слова).
func wordTokens(text string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if r == 'ё' {
				r = 'е'
			}
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// compiledTerm — термин, разобранный на основы слов.
type compiledTerm struct {
	raw    string   // исходный термин в нижнем регистре
	stems  []string // основы слов термина
	exact  bool     // термин с пробелом в конце («стали ») — последнее слово целиком
	single bool     // однословный термин — ищется как раньше, с начала слова
}

func compileTerm(term string) compiledTerm {
	low := strings.ToLower(term)
	ct := compiledTerm{raw: low, exact: strings.HasSuffix(low, " ")}
	words := wordTokens(low)
	if len(words) <= 1 && !ct.exact {
		ct.single = true
		return ct
	}
	for i, w := range words {
		if ct.exact && i == len(words)-1 {
			ct.stems = append(ct.stems, w)
		} else {
			ct.stems = append(ct.stems, stemRu(w))
		}
	}
	return ct
}

// matchTokens — встречается ли термин в последовательности слов.
func (ct compiledTerm) matchTokens(toks []string) bool {
	n := len(ct.stems)
	if n == 0 {
		return false
	}
	wordOK := func(tok string, j int) bool {
		if ct.exact && j == n-1 {
			return tok == ct.stems[j]
		}
		return strings.HasPrefix(tok, ct.stems[j])
	}
	for i := range toks {
		if !wordOK(toks[i], 0) {
			continue
		}
		pos, ok := i, true
		for j := 1; j < n && ok; j++ {
			switch {
			case pos+1 < len(toks) && wordOK(toks[pos+1], j):
				pos++
			case pos+2 < len(toks) && wordOK(toks[pos+2], j): // одно слово между частями термина
				pos += 2
			default:
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// termIn — термин в тексте (low — текст в нижнем регистре, toks — его слова).
func termIn(low string, toks []string, term string) bool {
	ct := compileTerm(term)
	if ct.single {
		return hasStem(low, ct.raw)
	}
	return ct.matchTokens(toks)
}
