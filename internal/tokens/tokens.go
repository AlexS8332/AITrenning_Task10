// Package tokens — оценка числа токенов до отправки запроса.
//
// Точное число знает только токенизатор модели, а он у DeepSeek лежит
// сборкой под Python. Поэтому здесь оценка по классам символов: латиница,
// кириллица, цифры, иероглифы, знаки. Документация DeepSeek даёт две
// опорные точки — английский символ ≈ 0.3 токена, китайский ≈ 0.6, — а
// вес кириллицы подобран по фактическому расходу из ответов API.
//
// Оценка нужна до запроса: чтобы показать, из чего складывается контекст,
// и чтобы поймать переполнение раньше, чем его поймает API. Факт всегда
// берётся из usage ответа: оценка — прикидка, а не истина.
package tokens

import (
	"unicode"

	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
)

// Weights — сколько токенов приходится на символ каждого класса и какие
// накладные даёт структура запроса.
type Weights struct {
	// Latin — латиница, Cyrillic — кириллица, CJK — иероглифы,
	// Digit — цифры, Space — пробелы и переводы строк, Other — всё
	// остальное: знаки препинания, скобки, кавычки.
	Latin    float64 `json:"latin"`
	Cyrillic float64 `json:"cyrillic"`
	CJK      float64 `json:"cjk"`
	Digit    float64 `json:"digit"`
	Space    float64 `json:"space"`
	Other    float64 `json:"other"`

	// PerMessage — служебная обвязка одного сообщения: роль, разделители,
	// идентификатор вызова инструмента.
	PerMessage float64 `json:"perMessage"`
	// PerToolDef — обвязка одного описания инструмента поверх его текста.
	PerToolDef float64 `json:"perToolDef"`
	// PerRequest — обвязка всего запроса.
	PerRequest float64 `json:"perRequest"`
}

// Default — веса по умолчанию. Латиница и иероглифы взяты из
// документации DeepSeek, вес кириллицы и знаков подобран по фактическому
// расходу: первый прогон с весом кириллицы 0.50 завышал оценку на 19–36 %
// (тем сильнее, чем короче запрос), после подгонки ошибка укладывается в
// единицы процентов. Числа замеров — в README, раздел «Насколько точна
// оценка».
var Default = Weights{
	Latin:      0.30,
	Cyrillic:   0.39,
	CJK:        0.60,
	Digit:      0.40,
	Space:      0.08,
	Other:      0.36,
	PerMessage: 3,
	PerToolDef: 6,
	PerRequest: 3,
}

// Estimate — оценка запроса по частям. Каждое поле — токены, а не символы.
// History — та часть истории, которая уходит модели дословно; Facts —
// карточка фактов, заменившая всё, что старше; User — новое сообщение. У
// стратегий full и window Facts равен нулю, а у full History — это весь
// путь ветки.
type Estimate struct {
	System  int `json:"system"`
	Facts   int `json:"facts"`
	Tools   int `json:"tools"`
	History int `json:"history"`
	User    int `json:"user"`
	Total   int `json:"total"`
}

// Tokens — оценка до отправки против факта из ответа модели.
// Actual = 0 означает, что ответа ещё нет.
type Tokens struct {
	Estimated int `json:"estimated"`
	Actual    int `json:"actual,omitempty"`
	// ErrorPct — на сколько процентов оценка разошлась с фактом;
	// отрицательное значение — оценка занизила.
	ErrorPct float64 `json:"errorPct,omitempty"`
}

// Text — оценка одной строки.
func (w Weights) Text(s string) float64 {
	var sum float64
	for _, r := range s {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF, r >= 0x3040 && r <= 0x30FF:
			sum += w.CJK
		case unicode.IsSpace(r):
			sum += w.Space
		case unicode.IsDigit(r):
			sum += w.Digit
		case unicode.Is(unicode.Cyrillic, r):
			sum += w.Cyrillic
		case unicode.Is(unicode.Latin, r):
			sum += w.Latin
		default:
			sum += w.Other
		}
	}
	return sum
}

// Message — оценка одного сообщения вместе с обвязкой. Аргументы вызовов
// инструментов считаются как текст: модель платит и за них.
func (w Weights) Message(m llm.Message) float64 {
	sum := w.PerMessage + w.Text(m.Content)
	for _, c := range m.ToolCalls {
		sum += w.Text(c.Function.Name) + w.Text(c.Function.Arguments) + w.PerMessage
	}
	return sum
}

// Messages — оценка списка сообщений.
func (w Weights) Messages(ms []llm.Message) float64 {
	var sum float64
	for _, m := range ms {
		sum += w.Message(m)
	}
	return sum
}

// ToolDefs — оценка описаний инструментов. Они уходят модели в каждом
// запросе целиком, вместе со схемой аргументов.
func (w Weights) ToolDefs(defs []llm.ToolDef) float64 {
	var sum float64
	for _, d := range defs {
		sum += w.PerToolDef + w.Text(d.Function.Name) + w.Text(d.Function.Description) + w.Text(string(d.Function.Parameters))
	}
	return sum
}

// Of — разбивка запроса: системный промпт, блок памяти, описания
// инструментов, история дословно, новое сообщение. Total считается по
// сумме долей, чтобы части складывались в целое и в интерфейсе не
// расходились на единицу. Пустой memory даёт нулевую долю: карточки нет —
// платить не за что.
func (w Weights) Of(system, memory string, defs []llm.ToolDef, history []llm.Message, user string) Estimate {
	e := Estimate{
		System:  round(w.PerMessage + w.Text(system)),
		Tools:   round(w.ToolDefs(defs)),
		History: round(w.Messages(history)),
		User:    round(w.PerMessage + w.Text(user)),
	}
	if memory != "" {
		e.Facts = round(w.PerMessage + w.Text(memory))
	}
	e.Total = e.System + e.Facts + e.Tools + e.History + e.User + round(w.PerRequest)
	return e
}

// OfMessages — оценка готового списка сообщений: им пользуется цикл хода,
// где история, ответы модели и результаты инструментов уже перемешаны.
func (w Weights) OfMessages(defs []llm.ToolDef, ms []llm.Message) int {
	return round(w.Messages(ms) + w.ToolDefs(defs) + w.PerRequest)
}

// Of — разбивка с весами по умолчанию.
func Of(system, memory string, defs []llm.ToolDef, history []llm.Message, user string) Estimate {
	return Default.Of(system, memory, defs, history, user)
}

// OfMessages — оценка списка сообщений с весами по умолчанию.
func OfMessages(defs []llm.ToolDef, ms []llm.Message) int {
	return Default.OfMessages(defs, ms)
}

// Compare сводит оценку с фактом. Факт 0 (ответа нет) оставляет одну
// оценку: делить на ноль и рисовать стопроцентную ошибку нечестно.
func Compare(estimated, actual int) Tokens {
	t := Tokens{Estimated: estimated, Actual: actual}
	if actual > 0 {
		t.ErrorPct = float64(estimated-actual) / float64(actual) * 100
	}
	return t
}

func round(f float64) int {
	if f <= 0 {
		return 0
	}
	return int(f + 0.5)
}
