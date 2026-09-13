// Package agent — контракты агента: ход диалога, события журнала и сам
// интерфейс Agent. Пакет не знает о конкретных агентах, о хранилище и об
// HTTP: его импортируют и агенты, и сервер, и история.
package agent

import (
	"context"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/facts"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/tokens"
)

// Input — с чем агент входит в ход: путь текущей ветки целиком, карточка
// фактов этой ветки и новое сообщение. История передаётся целиком при
// любой стратегии: решать, что из неё уйдёт модели, — дело агента, а не
// того, кто его вызвал.
type Input struct {
	History []llm.Message
	Facts   facts.State
	User    string
	// Turn — номер хода в ветке, считая с единицы. Им помечаются записи
	// карточки фактов: по нему видно, когда факт появился.
	Turn int
	// Linear — весь диалог одной лентой, как если бы ветвиться было
	// нельзя. Нужен только для счёта: сравнив его с путём ветки, видно,
	// сколько контекста ветвление сняло с этого хода. Пусто у диалога без
	// ветвлений.
	Linear []llm.Message
	// Branch — имя текущей ветки, для журнала.
	Branch string
}

// Reply — итог одного хода: ответ пользователю и всё, что добавилось к
// истории за этот ход. Added хранится как есть: сообщение пользователя,
// ответы модели с вызовами инструментов, ответы инструментов и итоговый
// текст. Именно из этих сообщений складывается контекст следующего хода.
type Reply struct {
	Text  string
	Added []llm.Message
	// Facts — карточка фактов ветки после хода: та же, что была, или
	// поправленная, если в этом ходе нашлись новые факты.
	Facts facts.State
	Stats Stats
}

// Stats — счётчики одного хода.
type Stats struct {
	Steps     int
	ToolCalls int
	Usage     llm.Usage
	Cost      llm.Cost
	Context   Context
}

// Context — что занимало контекст в этом ходе. Estimate — оценка на
// старте хода с разбивкой по частям; Peak — оценка перед последним
// запросом хода: внутри хода контекст растёт с каждым ответом
// инструмента. FirstPrompt — фактические токены запроса на первом шаге,
// именно с ними сравнима оценка старта. Limit и Trimmed заполнены, когда
// работал свой лимит контекста.
type Context struct {
	Estimate    tokens.Estimate `json:"estimate"`
	Peak        int             `json:"peak"`
	FirstPrompt int             `json:"firstPrompt"`
	Limit       int             `json:"limit,omitempty"`
	// Trimmed — сколько сообщений истории выброшено, чтобы ход влез.
	Trimmed int `json:"trimmed,omitempty"`
	// Mode — стратегия работы с историей в этом ходе: full, window, facts.
	Mode string `json:"mode,omitempty"`
	// Full — во сколько токенов обошёлся бы тот же ход с полным путём
	// ветки и без карточки фактов. Считается тем же оценщиком и ничего не
	// стоит, зато без него экономию не с чем сравнить.
	Full int `json:"full,omitempty"`
	// Saved — Full минус оценка отправленного запроса: сколько токенов
	// стратегия сняла с этого хода.
	Saved int `json:"saved,omitempty"`
	// Dropped — сколько сообщений истории не ушло модели: они заменены
	// карточкой фактов или просто выброшены окном.
	Dropped int `json:"dropped,omitempty"`
	// FactsVersion — версия карточки фактов, с которой шёл ход.
	FactsVersion int `json:"factsVersion,omitempty"`
	// Branch — имя ветки, в которой сделан ход.
	Branch string `json:"branch,omitempty"`
	// Linear — во сколько токенов обошёлся бы тот же ход, если бы весь
	// диалог шёл одной лентой без ветвлений, BranchSaved — разница с
	// полным путём ветки. Так видно, сколько снимает само ветвление,
	// отдельно от окна и фактов.
	Linear      int `json:"linear,omitempty"`
	BranchSaved int `json:"branchSaved,omitempty"`
}

// Agent — то, что ведёт диалог. В Input вся история прошлых ходов без
// системного промпта: системный промпт агент добавляет сам, чтобы правки
// в коде действовали и на старые диалоги.
type Agent interface {
	Name() string
	Reply(ctx context.Context, in Input, em Emitter) (Reply, error)
}

// Виды событий журнала.
const (
	EventAgentStart = "agent.start"
	EventAgentDone  = "agent.done"
	EventAgentError = "agent.error"
	EventLLMRequest = "llm.request"
	EventLLMReply   = "llm.response"
	EventToolCall   = "tool.call"
	EventToolResult = "tool.result"
	EventToolError  = "tool.error"
	EventNote       = "note"
	// EventFacts — карточка фактов поправлена: в Detail лежит она целиком,
	// в Usage и Cost — во что обошёлся сам извлекатель.
	EventFacts = "facts.update"
	// EventBranch — ход идёт в ветке, отпочкованной от другой: в Title
	// сказано, от какой и с какого места.
	EventBranch = "branch"
	// EventPrompt — что именно получает модель на старте хода: системный
	// промпт, новое сообщение, размер истории и описания инструментов. В
	// Detail лежит JSON вида Prompt; интерфейс показывает его отдельной
	// панелью, а не в ленте.
	EventPrompt = "prompt"
)

// Prompt — содержимое события EventPrompt.
type Prompt struct {
	System string `json:"system"`
	// Facts — карточка фактов в том виде, в каком её получила модель;
	// пустая, если стратегия её не подставляет.
	Facts string       `json:"facts,omitempty"`
	User  string       `json:"user"`
	Tools []PromptTool `json:"tools"`
	// History — сколько сообщений прошлых ходов ушло модели перед новым
	// сообщением, и сколько в них символов.
	History      int `json:"history"`
	HistoryRunes int `json:"historyRunes"`
	// Estimate — оценка запроса в токенах с разбивкой по частям, Limit —
	// свой лимит контекста, если он задан.
	Estimate tokens.Estimate `json:"estimate"`
	Limit    int             `json:"limit,omitempty"`
	// Mode — стратегия работы с историей, Dropped — сколько сообщений
	// истории не ушло модели, Full — оценка того же запроса с полным
	// путём ветки.
	Mode    string `json:"mode,omitempty"`
	Dropped int    `json:"dropped,omitempty"`
	Full    int    `json:"full,omitempty"`
	// Branch — ветка хода, Linear — оценка того же запроса без ветвлений.
	Branch string `json:"branch,omitempty"`
	Linear int    `json:"linear,omitempty"`
}

// PromptTool — инструмент, доступный агенту, как он описан модели.
type PromptTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Event — запись журнала. Title — одна строка для ленты, Detail — текст
// под ней (аргументы, результат инструмента, ответ модели), Usage и Cost
// заполнены только у ответов модели.
type Event struct {
	Seq     int        `json:"seq"`
	Time    time.Time  `json:"time"`
	Agent   string     `json:"agent"`
	Kind    string     `json:"kind"`
	Step    int        `json:"step,omitempty"`
	Title   string     `json:"title"`
	Detail  string     `json:"detail,omitempty"`
	Usage   *llm.Usage `json:"usage,omitempty"`
	Cost    *llm.Cost  `json:"cost,omitempty"`
	Seconds float64    `json:"seconds,omitempty"`
	// Tokens — сколько токенов насчитала оценка перед запросом и сколько
	// их оказалось по ответу модели. У запроса заполнена только оценка, у
	// ответа — оба числа и расхождение.
	Tokens *tokens.Tokens `json:"tokens,omitempty"`
}

// Emitter принимает события журнала. Реализация обязана быть безопасной
// для вызова из нескольких горутин.
type Emitter interface {
	Log(Event)
}

// Nop — эмиттер, который всё отбрасывает. Для тестов и вызовов, где
// журнал не нужен.
type Nop struct{}

func (Nop) Log(Event) {}
