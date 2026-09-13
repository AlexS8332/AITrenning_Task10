// Package agents — конкретные агенты приложения и их каталог. Все они
// ведут диалог: получают путь текущей ветки и новое сообщение, а
// возвращают ответ и сообщения, которые добавились к ветке.
package agents

import (
	"context"
	"fmt"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/facts"
	"github.com/AlexS8332/AITrenning_Task10/internal/history"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/strategy"
	"github.com/AlexS8332/AITrenning_Task10/internal/tokens"
	"github.com/AlexS8332/AITrenning_Task10/internal/tools"
)

// Deps — то, что нужно любому агенту: цикл с моделью, инструменты и
// стратегия работы с историей.
type Deps struct {
	Runner agent.Runner
	Tools  *tools.Registry
	// KeepToolRunes — до скольких символов сокращать ответы инструментов
	// прошлых ходов перед отправкой модели; 0 — отправлять целиком.
	KeepToolRunes int
	// Strategy — что уходит модели вместо истории: вся история, окно или
	// карточка фактов с окном.
	Strategy strategy.Policy
	// Extractor — модель, которая ведёт карточку фактов; nil означает ту
	// же, что ведёт диалог. Отдельная модель имеет смысл, когда факты
	// дешевле выбирать слабой моделью, а отвечать — сильной.
	Extractor llm.Chatter
}

// extractor собирает извлекатель фактов по зависимостям: модель по
// умолчанию та же, что ведёт диалог.
func (d Deps) extractor() facts.Extractor {
	client := d.Extractor
	if client == nil {
		client = d.Runner.LLM
	}
	return facts.Extractor{LLM: client, Model: d.Runner.Model, Policy: d.Strategy.Facts}
}

// Info — описание типа агента для интерфейса.
type Info struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	HasTools    bool   `json:"hasTools"`
}

// Entry — тип агента в каталоге: описание и сборка.
type Entry struct {
	Info
	Build func(Deps) agent.Agent
}

// Catalog — все типы агентов в порядке показа. Ключ типа хранится в
// диалоге: при продолжении диалог ведёт тот же тип агента.
func Catalog() []Entry {
	return []Entry{
		{
			Info: Info{
				Key:   "analyst",
				Title: "Аналитик: сбор ТЗ",
				Description: "Ведёт разбор задачи: задаёт по одному уточняющему вопросу, фиксирует решения " +
					"и по просьбе собирает из них техническое задание. Инструментов нет — вся его работа " +
					"держится на том, что он помнит из разговора.",
				HasTools: false,
			},
			Build: NewAnalyst,
		},
		{
			Info: Info{
				Key:   "tools",
				Title: "Агент с инструментами",
				Description: "Справочник по животным: ищет и читает статьи русской Википедии по разделам, " +
					"сверяет латинское название и строит дерево классификации по GBIF. " +
					"Факты берёт только из инструментов, а контекст диалога — из истории.",
				HasTools: true,
			},
			Build: NewToolsAgent,
		},
		{
			Info: Info{
				Key:   "chat",
				Title: "Чат без инструментов",
				Description: "Та же история диалога, но ответы из памяти модели, без проверок. " +
					"Точка отсчёта для сравнения.",
				HasTools: false,
			},
			Build: NewChat,
		},
	}
}

// Find ищет тип агента по ключу.
func Find(key string) (Entry, bool) {
	for _, e := range Catalog() {
		if e.Key == key {
			return e, true
		}
	}
	return Entry{}, false
}

// Infos — описания всех типов для интерфейса.
func Infos() []Info {
	entries := Catalog()
	infos := make([]Info, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, e.Info)
	}
	return infos
}

// Общий для всех агентов блок о работе с историей диалога.
const dialogRules = `Ты ведёшь диалог, и его история у тебя перед глазами.
- Всё, что пользователь говорил раньше (как его зовут, о чём шла речь, что он просил и о чём вы договорились), — часть контекста. Короткие реплики вроде «да, второй вариант» относятся к предыдущим ходам.
- Не пересказывай каждый раз всё сначала: отвечай на текущую реплику, опираясь на уже сказанное.
- Если пользователь спрашивает, что было раньше в диалоге, отвечай по истории, а не по догадкам. Если чего-то в контексте нет — так и скажи, не придумывай.
Отвечай по-русски, в markdown, по существу.`

// run — общая обвязка хода: события старта и конца, работа с историей.
// Здесь же решается, что именно уйдёт модели: весь путь ветки, окно
// последних сообщений или окно вместе с карточкой фактов. Карточка
// обновляется до запроса — иначе новое решение пользователя попало бы в
// память только со следующего хода.
func run(ctx context.Context, deps Deps, spec agent.Spec, in agent.Input, em agent.Emitter, startTitle string) (agent.Reply, error) {
	if em == nil {
		em = agent.Nop{}
	}
	started := time.Now()
	policy := deps.Strategy.Normalized()
	em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventAgentStart,
		Title: fmt.Sprintf("%s: сообщений в истории %d, стратегия «%s» — %s",
			startTitle, len(in.History), strategy.Title(policy.Mode), policy.Label())})
	if in.Branch != "" && len(in.Linear) > len(in.History) {
		em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventBranch,
			Title: fmt.Sprintf("ветка «%s»: в пути %d сообщений из %d в диалоге",
				in.Branch, len(in.History), len(in.Linear)),
			Detail: "Сообщения соседних веток в контекст не попадают: ветка видит только путь от начала диалога до места ветвления и дальше свой."})
	}

	// Ответы инструментов прошлых ходов сокращаются и здесь, и при
	// извлечении фактов: карточке нужна суть, а окну — обрезанный текст.
	full := history.Compact(in.History, deps.KeepToolRunes)

	state := in.Facts
	if policy.Mode == strategy.ModeFacts {
		upd, err := deps.extractor().Run(ctx, state, full, in.User, in.Turn)
		switch {
		case err != nil:
			// Извлечение не удалось — ход не обязан падать: модель
			// получит окно со старой карточкой, а человек увидит, что
			// памяти не хватает. Оплаченный, но бесполезный запрос всё
			// равно попадает в счёт карточки.
			if upd.State.Calls > state.Calls {
				state = upd.State
			}
			em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventNote,
				Title:  "карточка фактов не обновлена: " + err.Error(),
				Detail: "Ход идёт с прежней карточкой; следующий ход попробует снова."})
		case upd.Changed:
			state = upd.State
			em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventFacts,
				Title:   factsTitle(upd),
				Detail:  state.Render(),
				Usage:   &upd.Usage,
				Cost:    &upd.Cost,
				Seconds: upd.Seconds})
		default:
			state = upd.State
			em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventFacts,
				Title:   fmt.Sprintf("новых фактов нет, в карточке %s", strategy.Plural(len(state.Entries), "запись", "записи", "записей")),
				Usage:   &upd.Usage,
				Cost:    &upd.Cost,
				Seconds: upd.Seconds})
		}
	}

	block, window := strategy.Apply(state, full, policy)
	defs := tools.Defs(spec.Tools)
	prepared := agent.Prepared{
		History:      window,
		Facts:        block,
		User:         in.User,
		Mode:         policy.Mode,
		Dropped:      len(full) - len(window),
		FactsVersion: state.Version,
		Branch:       in.Branch,
	}
	// Оценка того же хода с полным путём ветки: она ничего не стоит и
	// показывает, сколько сняла стратегия. Без неё экономия — слово, а не
	// число.
	prepared.Full = tokens.Of(spec.System, "", defs, full, in.User).Total
	// И вторая оценка — сколько ушло бы модели, если бы веток не было и
	// весь диалог шёл одной лентой.
	if len(in.Linear) > len(full) {
		linear := history.Compact(in.Linear, deps.KeepToolRunes)
		prepared.Linear = tokens.Of(spec.System, "", defs, linear, in.User).Total
	}

	reply, err := deps.Runner.Run(ctx, spec, prepared, em)
	reply.Facts = state
	if err != nil {
		em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventAgentError, Title: err.Error(),
			Seconds: time.Since(started).Seconds()})
		return reply, err
	}
	em.Log(agent.Event{Agent: spec.Name, Kind: agent.EventAgentDone,
		Title: fmt.Sprintf("готово: шагов %d, вызовов инструментов %d, к ветке добавлено сообщений %d%s",
			reply.Stats.Steps, reply.Stats.ToolCalls, len(reply.Added), savedNote(reply.Stats.Context)),
		Seconds: time.Since(started).Seconds()})
	return reply, nil
}

// factsTitle — одна строка о том, что изменилось в карточке. Список
// ключей важнее числа: по нему видно, что именно агент решил запомнить.
func factsTitle(upd facts.Update) string {
	parts := make([]string, 0, 3)
	if n := len(upd.Set); n > 0 {
		parts = append(parts, "записано: "+join(upd.Set))
	}
	if n := len(upd.Deleted); n > 0 {
		parts = append(parts, "убрано: "+join(upd.Deleted))
	}
	if upd.Dropped > 0 {
		parts = append(parts, fmt.Sprintf("вытеснено потолком: %d", upd.Dropped))
	}
	head := fmt.Sprintf("карточка фактов, версия %d — ", upd.State.Version)
	return head + join2(parts) + fmt.Sprintf(" (всего %s)",
		strategy.Plural(len(upd.State.Entries), "запись", "записи", "записей"))
}

func join(keys []string) string {
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += "«" + k + "»"
	}
	return out
}

func join2(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

func savedNote(c agent.Context) string {
	note := ""
	if c.Saved > 0 {
		note += fmt.Sprintf(", стратегия сняла ≈%d токенов из ≈%d", c.Saved, c.Full)
	}
	if c.BranchSaved > 0 {
		note += fmt.Sprintf(", ветвление — ещё ≈%d", c.BranchSaved)
	}
	return note
}
