package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/tokens"
	"github.com/AlexS8332/AITrenning_Task10/internal/tools"
)

const (
	// Сколько символов результата инструмента и ответа модели показывать в
	// журнале. Полный текст уходит модели, а в ленте нужен обозримый кусок.
	logDetailRunes = 6000

	defaultMaxSteps = 12
)

// ErrStepLimit — агент не уложился в лимит шагов.
var ErrStepLimit = errors.New("исчерпан лимит шагов агента")

// ErrContextOverflow — запрос не влезает в лимит контекста. Свой лимит
// ловит переполнение до отправки; без него то же самое скажет API, но
// после запроса и в своих словах.
var ErrContextOverflow = errors.New("запрос не влезает в контекст")

// Что делать, когда запрос не влезает в лимит контекста.
const (
	// OverflowFail — не отправлять, ход падает с ошибкой.
	OverflowFail = "fail"
	// OverflowTrim — выбрасывать самые старые ходы, пока не влезет.
	OverflowTrim = "trim"
	// OverflowOff — не проверять и отправить как есть: пусть отвечает API.
	OverflowOff = "off"
)

// Spec — описание агента для Runner: роль, инструменты, лимит шагов.
type Spec struct {
	Name     string
	System   string
	Tools    []tools.Tool
	MaxSteps int
}

// Prepared — то, что уходит модели в этом ходе, уже после работы с
// историей. History — сообщения, которые модель получит дословно (у окна
// и фактов это только последние), Facts — карточка фактов или пустая
// строка. Остальные поля Runner не вычисляет, а показывает и сохраняет:
// стратегия — дело агента, а счёт токенов — его.
type Prepared struct {
	History []llm.Message
	Facts   string
	User    string

	// Mode — стратегия работы с историей: full, window, facts.
	Mode string
	// Dropped — сколько сообщений истории не ушло модели.
	Dropped int
	// Full — оценка того же запроса с полным путём ветки и без карточки:
	// столько стоил бы ход без стратегии.
	Full int
	// FactsVersion — версия карточки фактов, с которой идёт ход.
	FactsVersion int
	// Branch — имя ветки, Linear — оценка того же запроса, если бы весь
	// диалог шёл одной лентой без ветвлений.
	Branch string
	Linear int
}

// Runner исполняет цикл одного хода: запрос к модели, вызовы инструментов,
// ответ. Один Runner обслуживает всех агентов приложения.
type Runner struct {
	LLM         llm.Chatter
	Model       string
	Temperature float64

	// ContextLimit — свой лимит контекста в токенах, 0 — не проверять.
	// У модели свой лимит, и его через API не изменить; этот нужен, чтобы
	// ловить переполнение до отправки и чтобы опыт с переполнением можно
	// было поставить на четырёх тысячах токенов, а не на миллионе.
	ContextLimit int
	// OnOverflow — что делать при переполнении: OverflowFail (по
	// умолчанию), OverflowTrim, OverflowOff.
	OnOverflow string
}

// Run ведёт один ход диалога. Модель получает системный промпт, историю
// прошлых ходов и новое сообщение; цикл крутится, пока модель просит
// вызвать инструменты, и заканчивается её ответом текстом. В Reply.Added
// возвращается всё, что добавилось после истории: сообщение пользователя,
// ответы модели, ответы инструментов и итоговый текст.
func (r Runner) Run(ctx context.Context, spec Spec, in Prepared, em Emitter) (Reply, error) {
	if em == nil {
		em = Nop{}
	}
	history, user := in.History, in.User
	memoryBlock := in.Facts
	if spec.MaxSteps <= 0 {
		spec.MaxSteps = defaultMaxSteps
	}

	byName := make(map[string]tools.Tool, len(spec.Tools))
	for _, t := range spec.Tools {
		byName[t.Name()] = t
	}
	defs := tools.Defs(spec.Tools)

	// Сколько токенов уйдёт модели, считаем до отправки: из чего сложился
	// контекст и влезает ли он. Своего лимита может не быть — тогда
	// переполнение поймает только API, и это тоже часть опыта.
	est := tokens.Of(spec.System, memoryBlock, defs, history, user)
	var trimmed int
	if r.ContextLimit > 0 && est.Total > r.ContextLimit {
		switch r.OnOverflow {
		case OverflowOff:
			// Отправляем как есть: интересно, что скажет сам API.
		case OverflowTrim:
			history, trimmed = trimHistory(spec.System, memoryBlock, defs, history, user, r.ContextLimit)
			est = tokens.Of(spec.System, memoryBlock, defs, history, user)
			em.Log(Event{Agent: spec.Name, Kind: EventNote,
				Title: fmt.Sprintf("история обрезана: выброшено сообщений %d, контекст ≈ %d токенов при лимите %d",
					trimmed, est.Total, r.ContextLimit),
				Detail: "Выброшены самые старые ходы целиком — от сообщения пользователя до следующего: " +
					"ответ инструмента без вызова, который его породил, API отвергает. " +
					"Файл диалога не меняется, режется только то, что уходит модели."})
			if est.Total > r.ContextLimit {
				return Reply{}, overflowError(est.Total, r.ContextLimit)
			}
		default:
			return Reply{}, overflowError(est.Total, r.ContextLimit)
		}
	}

	messages := make([]llm.Message, 0, len(history)+3)
	messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: spec.System})
	// Карточка фактов идёт отдельным системным сообщением сразу за
	// промптом: она не реплика в диалоге, а знание о том, чего в диалоге
	// больше не видно. Отдельным сообщением её видно и в файле запроса, и
	// в разбивке токенов.
	if memoryBlock != "" {
		messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: memoryBlock})
	}
	messages = append(messages, history...)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: user})
	// Всё после этого индекса — новое, оно и попадёт в Added.
	base := len(messages) - 1

	em.Log(Event{Agent: spec.Name, Kind: EventPrompt,
		Title:  "промпт хода",
		Detail: promptDetail(spec, in, history, est, r.ContextLimit)})

	stats := Stats{Context: Context{
		Estimate:     est,
		Limit:        r.ContextLimit,
		Trimmed:      trimmed,
		Mode:         in.Mode,
		Full:         in.Full,
		Dropped:      in.Dropped,
		FactsVersion: in.FactsVersion,
		Branch:       in.Branch,
		Linear:       in.Linear,
	}}
	if in.Full > est.Total {
		stats.Context.Saved = in.Full - est.Total
	}
	if in.Linear > in.Full {
		stats.Context.BranchSaved = in.Linear - in.Full
	}

	for step := 1; step <= spec.MaxSteps; step++ {
		stats.Steps = step

		// Контекст растёт и внутри хода: каждый ответ инструмента уходит
		// модели в следующем запросе. Поэтому оценка считается на каждом
		// шаге, а не один раз на ход.
		stepEst := tokens.OfMessages(defs, messages)
		if stepEst > stats.Context.Peak {
			stats.Context.Peak = stepEst
		}
		if r.ContextLimit > 0 && r.OnOverflow != OverflowOff && stepEst > r.ContextLimit {
			return Reply{Stats: stats}, fmt.Errorf("шаг %d: %w", step, overflowError(stepEst, r.ContextLimit))
		}

		em.Log(Event{Agent: spec.Name, Kind: EventLLMRequest, Step: step,
			Title: fmt.Sprintf("запрос к модели, сообщений: %d (из них истории: %d), контекст ≈ %d токенов",
				len(messages), len(history), stepEst),
			Detail: lastMessageDetail(messages),
			Tokens: &tokens.Tokens{Estimated: stepEst}})

		started := time.Now()
		resp, err := r.LLM.Chat(ctx, llm.Request{
			Model:       r.Model,
			Messages:    messages,
			Tools:       defs,
			Temperature: r.Temperature,
		})
		elapsed := time.Since(started)
		if err != nil {
			return Reply{Stats: stats}, fmt.Errorf("шаг %d: %w", step, err)
		}

		cost := llm.PriceOf(r.Model, resp.Usage, started)
		stats.Usage = stats.Usage.Add(resp.Usage)
		stats.Cost = stats.Cost.Add(cost)
		if step == 1 {
			stats.Context.FirstPrompt = resp.Usage.Prompt
		}
		usage := resp.Usage
		// Оценка против факта: факт всегда из usage, оценка — прикидка,
		// которую он и проверяет.
		tk := tokens.Compare(stepEst, resp.Usage.Prompt)
		em.Log(Event{Agent: spec.Name, Kind: EventLLMReply, Step: step,
			Title:   replyTitle(resp),
			Detail:  replyDetail(resp),
			Usage:   &usage,
			Cost:    &cost,
			Seconds: elapsed.Seconds(),
			Tokens:  &tk})

		messages = append(messages, resp.Message)

		if !resp.HasToolCalls() {
			added := append([]llm.Message(nil), messages[base:]...)
			return Reply{
				Text:  replyText(added),
				Added: added,
				Stats: stats,
			}, nil
		}

		for _, call := range resp.Message.ToolCalls {
			stats.ToolCalls++
			args := json.RawMessage(call.Function.Arguments)

			em.Log(Event{Agent: spec.Name, Kind: EventToolCall, Step: step,
				Title:  "вызов " + call.Function.Name,
				Detail: prettyJSON(call.Function.Arguments)})

			t, ok := byName[call.Function.Name]
			if !ok {
				err := fmt.Errorf("инструмента %s нет; доступны: %s", call.Function.Name,
					strings.Join(toolNames(spec), ", "))
				em.Log(Event{Agent: spec.Name, Kind: EventToolError, Step: step,
					Title: "неизвестный инструмент " + call.Function.Name, Detail: err.Error()})
				messages = append(messages, toolReply(call.ID, errorPayload(err)))
				continue
			}

			toolStarted := time.Now()
			out, err := t.Call(ctx, args)
			toolElapsed := time.Since(toolStarted)
			if err != nil {
				em.Log(Event{Agent: spec.Name, Kind: EventToolError, Step: step,
					Title: call.Function.Name + ": ошибка", Detail: err.Error(),
					Seconds: toolElapsed.Seconds()})
				messages = append(messages, toolReply(call.ID, errorPayload(err)))
				continue
			}
			em.Log(Event{Agent: spec.Name, Kind: EventToolResult, Step: step,
				Title:   fmt.Sprintf("%s: %s", call.Function.Name, sizeLabel(out)),
				Detail:  tools.Truncate(prettyJSON(out), logDetailRunes),
				Seconds: toolElapsed.Seconds()})
			messages = append(messages, toolReply(call.ID, out))
		}
	}

	return Reply{Stats: stats}, fmt.Errorf("%w (%d)", ErrStepLimit, spec.MaxSteps)
}

// replyText собирает ответ пользователю из всех сообщений модели за ход.
// Модель нередко отвечает на часть вопроса в том же сообщении, в котором
// просит вызвать инструмент («Тебя зовут Алекс. Сейчас посмотрю, где она
// обитает» и вызов read_wikipedia); показывать только последнее сообщение
// значило бы потерять этот текст, хотя в истории он есть.
func replyText(added []llm.Message) string {
	var parts []string
	for _, m := range added {
		if m.Role == llm.RoleAssistant && strings.TrimSpace(m.Content) != "" {
			parts = append(parts, strings.TrimSpace(m.Content))
		}
	}
	return strings.Join(parts, "\n\n")
}

// overflowError — переполнение с числами: сколько насчитали и при каком
// лимите. Без чисел сообщение бесполезно: непонятно, насколько промах.
func overflowError(estimated, limit int) error {
	return fmt.Errorf("%w: ≈%d токенов при лимите %d (лишних ≈%d)",
		ErrContextOverflow, estimated, limit, estimated-limit)
}

// trimHistory выбрасывает самые старые ходы, пока запрос не влезет в
// лимит. Ход выбрасывается целиком, от сообщения пользователя до
// следующего: ответ инструмента без вызова, который его породил, API
// отвергает. Возвращает укороченную историю и число выброшенных
// сообщений. Файл диалога при этом не трогается: режется только то, что
// уходит модели, — как и сокращение старых ответов инструментов.
func trimHistory(system, memory string, defs []llm.ToolDef, history []llm.Message, user string, limit int) ([]llm.Message, int) {
	dropped := 0
	for len(history) > 0 && tokens.Of(system, memory, defs, history, user).Total > limit {
		next := 1
		for next < len(history) && history[next].Role != llm.RoleUser {
			next++
		}
		dropped += next
		history = history[next:]
	}
	return history, dropped
}

// promptDetail собирает, что уходит модели на старте хода: промпты, размер
// истории и описания инструментов в том же виде, в каком их получает модель,
// плюс оценку запроса в токенах.
func promptDetail(spec Spec, in Prepared, history []llm.Message, est tokens.Estimate, limit int) string {
	p := Prompt{System: spec.System, Facts: in.Facts, User: in.User, Tools: []PromptTool{},
		History: len(history), Estimate: est, Limit: limit,
		Mode: in.Mode, Dropped: in.Dropped, Full: in.Full,
		Branch: in.Branch, Linear: in.Linear}
	for _, m := range history {
		p.HistoryRunes += len([]rune(m.Content))
		for _, c := range m.ToolCalls {
			p.HistoryRunes += len([]rune(c.Function.Arguments))
		}
	}
	for _, t := range spec.Tools {
		p.Tools = append(p.Tools, PromptTool{Name: t.Name(), Description: t.Description()})
	}
	data, err := json.Marshal(p)
	if err != nil {
		return spec.System + "\n\n" + in.User
	}
	return string(data)
}

func toolReply(callID, content string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: callID, Content: content}
}

// errorPayload — ошибка в виде JSON: модели проще отличить её от результата.
func errorPayload(err error) string {
	data, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(data)
}

func toolNames(spec Spec) []string {
	names := make([]string, 0, len(spec.Tools))
	for _, t := range spec.Tools {
		names = append(names, t.Name())
	}
	return names
}

func replyTitle(resp llm.Response) string {
	if !resp.HasToolCalls() {
		return "ответ текстом"
	}
	names := make([]string, 0, len(resp.Message.ToolCalls))
	for _, c := range resp.Message.ToolCalls {
		names = append(names, c.Function.Name)
	}
	return "модель просит вызвать: " + strings.Join(names, ", ")
}

func replyDetail(resp llm.Response) string {
	if resp.Message.Content == "" {
		return ""
	}
	return tools.Truncate(resp.Message.Content, logDetailRunes)
}

// lastMessageDetail показывает, что именно ушло модели последним: на первом
// шаге это сообщение пользователя, дальше — результаты инструментов.
func lastMessageDetail(messages []llm.Message) string {
	if len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	if last.Role == llm.RoleUser {
		return tools.Truncate(last.Content, logDetailRunes)
	}
	return ""
}

func sizeLabel(s string) string {
	n := len([]rune(s))
	switch {
	case n < 1000:
		return fmt.Sprintf("%d символов", n)
	default:
		return fmt.Sprintf("%.1f тыс. символов", float64(n)/1000)
	}
}

// prettyJSON форматирует JSON для журнала; не JSON возвращается как есть.
func prettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(data)
}
