package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task10/internal/strategy"
	"github.com/AlexS8332/AITrenning_Task10/internal/tools"
)

// fakeTools — реестр с инструментами тех же имён, что у настоящих, но
// отвечающими заготовками: агенты проверяются без сети.
func fakeTools() *tools.Registry {
	stub := func(name, out string) tools.Tool {
		return tools.Func{
			FuncName: name, FuncDescription: "подставной " + name,
			FuncParameters: json.RawMessage(`{"type":"object","properties":{}}`),
			FuncCall: func(context.Context, json.RawMessage) (string, error) {
				return out, nil
			},
		}
	}
	return tools.NewRegistry(
		stub("search_wikipedia", `{"results":[{"title":"Обыкновенная рысь"}]}`),
		stub("read_wikipedia", `{"title":"Обыкновенная рысь","text":"`+strings.Repeat("текст ", 400)+`"}`),
		stub("match_taxon", `{"found":true,"canonical_name":"Lynx lynx"}`),
		stub("taxon_tree", `{"tree":[]}`),
		stub("vernacular_names", `{"names":[]}`),
	)
}

type recorder struct{ events []agent.Event }

func (r *recorder) Log(ev agent.Event) { r.events = append(r.events, ev) }

func TestCatalog(t *testing.T) {
	keys := []string{"analyst", "tools", "chat"}
	if len(Catalog()) != len(keys) {
		t.Fatalf("каталог: %+v", Infos())
	}
	for i, key := range keys {
		if Catalog()[i].Key != key {
			t.Errorf("на месте %d ожидался %q, а стоит %q", i, key, Catalog()[i].Key)
		}
	}
	if e, ok := Find("analyst"); !ok || e.Title == "" || e.HasTools {
		t.Errorf("Find(analyst): %+v %v", e, ok)
	}
	if e, ok := Find("chat"); !ok || e.Title == "" || e.HasTools {
		t.Errorf("Find(chat): %+v %v", e, ok)
	}
	if e, ok := Find("tools"); !ok || !e.HasTools {
		t.Errorf("Find(tools): %+v %v", e, ok)
	}
	if _, ok := Find("team"); ok {
		t.Errorf("неизвестный тип не должен находиться")
	}
	if len(Infos()) != len(keys) {
		t.Errorf("Infos: %+v", Infos())
	}
}

// Нулевая стратегия — это полная история: Deps без Strategy не должен
// молча начинать резать контекст и звать вторую модель за фактами.
func TestZeroStrategyKeepsWholeHistory(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llmtest.Text("ответ"), nil }}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools()}
	reply, err := NewChat(deps).Reply(context.Background(),
		agent.Input{History: dialogHistory(8), User: "и что дальше?"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls() != 1 {
		t.Errorf("лишние запросы к модели: %d", fake.Calls())
	}
	if reply.Stats.Context.Dropped != 0 || reply.Stats.Context.Mode != strategy.ModeFull {
		t.Errorf("счётчики: %+v", reply.Stats.Context)
	}
}

func TestToolsAgentUsesToolsAndHistory(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		for _, name := range []string{"search_wikipedia", "read_wikipedia", "match_taxon", "taxon_tree", "vernacular_names"} {
			if !llmtest.HasTool(req, name) {
				t.Errorf("агенту не передан %s", name)
			}
		}
		switch llmtest.ToolReplies(req) {
		case 0:
			if !strings.Contains(req.Messages[0].Content, "Проверка названия") {
				t.Errorf("системный промпт: %q", req.Messages[0].Content[:60])
			}
			return llmtest.ToolCall("read_wikipedia", `{"title":"Обыкновенная рысь","section":"Питание"}`), nil
		default:
			return llmtest.Text("Рысь питается зайцами."), nil
		}
	}}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools()}

	rec := &recorder{}
	a := NewToolsAgent(deps)
	if a.Name() != "tools" {
		t.Errorf("имя: %s", a.Name())
	}
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "расскажи про рысь"},
		{Role: llm.RoleAssistant, Content: "Рысь — хищник семейства кошачьих."},
	}
	reply, err := a.Reply(context.Background(), agent.Input{History: hist, User: "а чем она питается?"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "Рысь питается зайцами." || len(reply.Added) != 4 {
		t.Errorf("ответ: %+v", reply)
	}
	// История ушла модели перед новым сообщением.
	first := fake.Requests[0].Messages
	if len(first) != 4 || first[1].Content != "расскажи про рысь" || first[3].Content != "а чем она питается?" {
		t.Errorf("история в запросе: %+v", first)
	}
	kinds := make([]string, 0, len(rec.events))
	for _, ev := range rec.events {
		kinds = append(kinds, ev.Kind)
	}
	got := strings.Join(kinds, " ")
	if !strings.HasPrefix(got, "agent.start prompt llm.request") || !strings.HasSuffix(got, "agent.done") {
		t.Errorf("события: %s", got)
	}
	if !strings.Contains(rec.events[0].Title, "сообщений в истории 2") {
		t.Errorf("событие старта: %q", rec.events[0].Title)
	}
}

func TestToolsAgentCompactsOldToolReplies(t *testing.T) {
	long := strings.Repeat("статья ", 1000)
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "расскажи про рысь"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "read_wikipedia", Arguments: `{"title":"Рысь"}`}}}},
		{Role: llm.RoleTool, ToolCallID: "c1", Content: long},
		{Role: llm.RoleAssistant, Content: "Рысь — хищник."},
	}
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("ок"), nil
	}}

	// С ограничением: старый ответ инструмента ушёл модели сокращённым.
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools(), KeepToolRunes: 200}
	if _, err := NewToolsAgent(deps).Reply(context.Background(), agent.Input{History: hist, User: "ещё?"}, nil); err != nil {
		t.Fatal(err)
	}
	sent := fake.Requests[0].Messages[3]
	if sent.Role != llm.RoleTool || len([]rune(sent.Content)) > 400 || !strings.Contains(sent.Content, "[сокращено") {
		t.Errorf("старый ответ инструмента не сокращён: %d символов", len([]rune(sent.Content)))
	}
	if hist[2].Content != long {
		t.Errorf("исходная история изменена")
	}

	// Без ограничения: как есть.
	deps.KeepToolRunes = 0
	if _, err := NewToolsAgent(deps).Reply(context.Background(), agent.Input{History: hist, User: "ещё?"}, nil); err != nil {
		t.Fatal(err)
	}
	if fake.Requests[1].Messages[3].Content != long {
		t.Errorf("при KeepToolRunes = 0 история должна уходить целиком")
	}
}

func TestChatHasNoToolsButKeepsHistory(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if len(req.Tools) != 0 {
			t.Errorf("у чата не должно быть инструментов: %+v", req.Tools)
		}
		if len(req.Messages) != 4 || req.Messages[1].Content != "меня зовут Алекс" {
			t.Errorf("история: %+v", req.Messages)
		}
		return llmtest.Text("Тебя зовут Алекс."), nil
	}}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools()}
	a := NewChat(deps)
	if a.Name() != "chat" {
		t.Errorf("имя: %s", a.Name())
	}
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "меня зовут Алекс"},
		{Role: llm.RoleAssistant, Content: "Приятно познакомиться."},
	}
	reply, err := a.Reply(context.Background(), agent.Input{History: hist, User: "как меня зовут?"}, nil)
	if err != nil || reply.Text != "Тебя зовут Алекс." || len(reply.Added) != 2 {
		t.Errorf("ответ: %+v, %v", reply, err)
	}
}

func TestAgentErrorIsLogged(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.ToolCall("search_wikipedia", `{}`), nil
	}}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools()}
	rec := &recorder{}
	_, err := NewChat(deps).Reply(context.Background(), agent.Input{User: "q"}, rec)
	if err == nil {
		t.Fatal("чат с лимитом в один шаг и вызовом инструмента должен упасть")
	}
	last := rec.events[len(rec.events)-1]
	if last.Kind != agent.EventAgentError || !strings.Contains(last.Title, "лимит") {
		t.Errorf("последнее событие: %+v", last)
	}
}

// dialogHistory — история из n ходов чата: вопрос и ответ на каждый.
func dialogHistory(n int) []llm.Message {
	var ms []llm.Message
	for i := 1; i <= n; i++ {
		ms = append(ms,
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("вопрос %d", i)},
			// Ответы длинные не для красоты: на коротких репликах конспект
			// вместе с пояснением к нему весит больше, чем свёрнутая
			// история, и сжатие уходит в минус. Опыт это подтверждает, а
			// тест должен проверять обычный случай.
			llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("ответ %d: ", i) +
				strings.Repeat("рысь — хищник семейства кошачьих, обитающий в тайге. ", 20)})
	}
	return ms
}

// isExtract — запрос к извлекателю фактов узнаётся по его системному
// промпту: у него другая роль, чем у агента диалога.
func isExtract(req llm.Request) bool {
	return len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "карточку фактов диалога")
}

// factsFake — подставная модель, которая на запрос извлекателя отвечает
// готовой правкой карточки, а на запрос диалога — коротким текстом.
func factsFake(patch string, extracts *int) *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isExtract(req) {
			if extracts != nil {
				*extracts++
			}
			return llmtest.Text(patch), nil
		}
		return llmtest.Text("ответ"), nil
	}}
}

func TestFactsReplaceOldHistoryInRequest(t *testing.T) {
	// Главное свойство стратегии: модель получает карточку фактов вместо
	// начала истории, а не вдобавок к нему.
	var extracts int
	patch := `{"set":[{"key":"имя","value":"Алекс"},{"key":"город","value":"Иркутск"}],"delete":[]}`
	fake := factsFake(patch, &extracts)
	deps := Deps{
		Runner:   agent.Runner{LLM: fake, Model: "m"},
		Tools:    fakeTools(),
		Strategy: strategy.Policy{Mode: strategy.ModeFacts, Keep: 4},
	}
	rec := &recorder{}
	hist := dialogHistory(8)

	reply, err := NewChat(deps).Reply(context.Background(),
		agent.Input{History: hist, User: "как меня зовут?", Turn: 9}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if extracts != 1 {
		t.Fatalf("запросов к извлекателю: %d", extracts)
	}
	if reply.Facts.Version != 1 || len(reply.Facts.Entries) != 2 {
		t.Fatalf("карточка после хода: %+v", reply.Facts)
	}
	if v, ok := reply.Facts.Get("имя"); !ok || v != "Алекс" {
		t.Errorf("факт «имя»: %q %v", v, ok)
	}
	if reply.Facts.Entries[0].Since != 9 {
		t.Errorf("факт должен помнить ход появления: %+v", reply.Facts.Entries[0])
	}

	sent := fake.Requests[1].Messages
	if sent[0].Role != llm.RoleSystem || !strings.Contains(sent[0].Content, "справочник") {
		t.Errorf("первым идёт системный промпт: %+v", sent[0])
	}
	if sent[1].Role != llm.RoleSystem || !strings.Contains(sent[1].Content, "имя: Алекс") {
		t.Errorf("вторым идёт карточка отдельным сообщением: %+v", sent[1])
	}
	// Отрезанная часть истории до модели не дошла, хвост дошёл.
	for _, m := range sent {
		if m.Content == "вопрос 1" {
			t.Errorf("отрезанное сообщение всё равно ушло модели")
		}
	}
	if !strings.HasPrefix(sent[len(sent)-2].Content, "ответ 8: ") {
		t.Errorf("окно последних сообщений потерялось: %+v", sent[len(sent)-2])
	}

	// Экономия считается и попадает в счётчики хода.
	c := reply.Stats.Context
	if c.Mode != strategy.ModeFacts || c.Dropped != 12 || c.Full <= c.Estimate.Total || c.Saved == 0 {
		t.Errorf("счётчики стратегии: %+v", c)
	}
	if c.Estimate.Facts == 0 {
		t.Errorf("карточка должна занимать место в разбивке: %+v", c.Estimate)
	}
	if c.FactsVersion != 1 {
		t.Errorf("версия карточки в счётчиках: %+v", c)
	}

	var factEvents int
	for _, ev := range rec.events {
		if ev.Kind == agent.EventFacts {
			factEvents++
			if !strings.Contains(ev.Detail, "Алекс") || ev.Usage == nil {
				t.Errorf("событие карточки без записей или расхода: %+v", ev)
			}
			if !strings.Contains(ev.Title, "«имя»") {
				t.Errorf("в заголовке события должны быть ключи: %q", ev.Title)
			}
		}
	}
	if factEvents != 1 {
		t.Errorf("событий карточки в журнале: %d", factEvents)
	}
}

// Карточка обновляется после каждой реплики пользователя, но версия
// растёт только когда что-то действительно поправлено: иначе кэш
// префикса ломался бы на каждом ходе впустую.
func TestFactsUpdateEveryTurnButVersionOnlyOnChange(t *testing.T) {
	var extracts int
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isExtract(req) {
			extracts++
			if extracts == 1 {
				return llmtest.Text(`{"set":[{"key":"цель","value":"учёт списаний"}]}`), nil
			}
			return llmtest.Text(`{"set":[],"delete":[]}`), nil
		}
		return llmtest.Text("ответ"), nil
	}}
	deps := Deps{
		Runner:   agent.Runner{LLM: fake, Model: "m"},
		Tools:    fakeTools(),
		Strategy: strategy.Policy{Mode: strategy.ModeFacts, Keep: 4},
	}
	a := NewChat(deps)

	first, err := a.Reply(context.Background(), agent.Input{History: dialogHistory(4), User: "раз", Turn: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	second, err := a.Reply(context.Background(),
		agent.Input{History: dialogHistory(4), Facts: first.Facts, User: "два", Turn: 6}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if extracts != 2 {
		t.Errorf("извлекатель должен зваться на каждом ходе: %d", extracts)
	}
	if second.Facts.Version != 1 {
		t.Errorf("ход без правок не должен поднимать версию: %+v", second.Facts)
	}
	if second.Facts.Calls != 2 {
		t.Errorf("запросы извлекателя должны считаться и без правок: %+v", second.Facts)
	}
	var noChange bool
	for _, ev := range rec.events {
		if ev.Kind == agent.EventFacts && strings.Contains(ev.Title, "новых фактов нет") {
			noChange = true
		}
	}
	if !noChange {
		t.Error("о ходе без новых фактов тоже нужно сказать в журнале")
	}
}

func TestWindowModeDropsHistoryWithoutFacts(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isExtract(req) {
			t.Error("в режиме окна извлекатель фактов зваться не должен")
		}
		return llmtest.Text("ответ"), nil
	}}
	deps := Deps{
		Runner:   agent.Runner{LLM: fake, Model: "m"},
		Tools:    fakeTools(),
		Strategy: strategy.Policy{Mode: strategy.ModeWindow, Keep: 4},
	}
	reply, err := NewChat(deps).Reply(context.Background(),
		agent.Input{History: dialogHistory(8), User: "как меня зовут?", Turn: 9}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls() != 1 {
		t.Fatalf("в режиме окна модель зовут только за ответом: %d запросов", fake.Calls())
	}
	sent := fake.Requests[0].Messages
	if len(sent) != 6 {
		t.Errorf("модели ушло %d сообщений вместо системного промпта, окна и вопроса", len(sent))
	}
	if reply.Facts.Version != 0 || reply.Stats.Context.Estimate.Facts != 0 {
		t.Errorf("в режиме окна карточки быть не должно: %+v", reply.Facts)
	}
	if reply.Stats.Context.Dropped != 12 {
		t.Errorf("выброшено сообщений: %d", reply.Stats.Context.Dropped)
	}
}

func TestFailedFactsExtractionDoesNotBreakTurn(t *testing.T) {
	// Карточка — не главное дело хода: если извлекатель не ответил, ход
	// всё равно обязан состояться.
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isExtract(req) {
			return llm.Response{}, errors.New("сеть недоступна")
		}
		return llmtest.Text("ответ"), nil
	}}
	deps := Deps{
		Runner:   agent.Runner{LLM: fake, Model: "m"},
		Tools:    fakeTools(),
		Strategy: strategy.Policy{Mode: strategy.ModeFacts, Keep: 4},
	}
	rec := &recorder{}
	reply, err := NewChat(deps).Reply(context.Background(),
		agent.Input{History: dialogHistory(8), User: "и что дальше?", Turn: 9}, rec)
	if err != nil {
		t.Fatalf("ход должен пройти без карточки: %v", err)
	}
	if reply.Text != "ответ" || reply.Facts.Version != 0 {
		t.Errorf("ответ: %+v", reply)
	}
	var noted bool
	for _, ev := range rec.events {
		if ev.Kind == agent.EventNote && strings.Contains(ev.Title, "карточка фактов не обновлена") {
			noted = true
		}
	}
	if !noted {
		t.Error("о несобранной карточке нужно сказать в журнале")
	}
}

// Извлекатель ответил не тем — ход всё равно идёт, но оплаченный запрос
// попадает в счёт карточки: платить за него пришлось.
func TestBrokenPatchIsPaidAndReported(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isExtract(req) {
			return llmtest.Text("извините, не понял задачу"), nil
		}
		return llmtest.Text("ответ"), nil
	}}
	deps := Deps{
		Runner:   agent.Runner{LLM: fake, Model: "m"},
		Tools:    fakeTools(),
		Strategy: strategy.Policy{Mode: strategy.ModeFacts, Keep: 4},
	}
	rec := &recorder{}
	reply, err := NewChat(deps).Reply(context.Background(),
		agent.Input{History: dialogHistory(4), User: "дальше", Turn: 5}, rec)
	if err != nil {
		t.Fatalf("ход должен пройти: %v", err)
	}
	if reply.Facts.Calls != 1 || reply.Facts.Usage.Total == 0 {
		t.Errorf("неудачное извлечение всё равно оплачено: %+v", reply.Facts)
	}
	if len(reply.Facts.Entries) != 0 {
		t.Errorf("из мусора не должно родиться фактов: %+v", reply.Facts.Entries)
	}
	var noted bool
	for _, ev := range rec.events {
		if ev.Kind == agent.EventNote && strings.Contains(ev.Title, "карточка фактов не обновлена") {
			noted = true
		}
	}
	if !noted {
		t.Error("о разборе ответа извлекателя нужно сказать в журнале")
	}
}

// Ветвление живёт в пакете history, но агент должен заметить, что путь
// ветки короче всего диалога, и посчитать, сколько это сняло.
func TestBranchSavingIsCounted(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llmtest.Text("ответ"), nil }}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools(),
		Strategy: strategy.Policy{Mode: strategy.ModeFull}}
	rec := &recorder{}
	path := dialogHistory(4)
	linear := dialogHistory(10)

	reply, err := NewChat(deps).Reply(context.Background(),
		agent.Input{History: path, Linear: linear, Branch: "вариант Б", User: "и что?", Turn: 5}, rec)
	if err != nil {
		t.Fatal(err)
	}
	c := reply.Stats.Context
	if c.Linear <= c.Full || c.BranchSaved != c.Linear-c.Full {
		t.Errorf("экономия ветвления: %+v", c)
	}
	if c.Branch != "вариант Б" {
		t.Errorf("ветка хода: %q", c.Branch)
	}
	var branched bool
	for _, ev := range rec.events {
		if ev.Kind == agent.EventBranch && strings.Contains(ev.Title, "вариант Б") {
			branched = true
		}
	}
	if !branched {
		t.Error("о работе в ветке нужно сказать в журнале")
	}
}

// Аналитик — агент опыта: без инструментов, с промптом про сбор ТЗ.
func TestAnalystHasNoToolsAndCollectsRequirements(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if len(req.Tools) != 0 {
			t.Errorf("у аналитика не должно быть инструментов: %+v", req.Tools)
		}
		if !strings.Contains(req.Messages[0].Content, "техническое задание") {
			t.Errorf("системный промпт: %q", req.Messages[0].Content[:60])
		}
		return llmtest.Text("Зафиксировал срок. Какой бюджет?"), nil
	}}
	deps := Deps{Runner: agent.Runner{LLM: fake, Model: "m"}, Tools: fakeTools()}
	a := NewAnalyst(deps)
	if a.Name() != "analyst" {
		t.Errorf("имя: %s", a.Name())
	}
	reply, err := a.Reply(context.Background(), agent.Input{User: "срок — 1 марта", Turn: 1}, nil)
	if err != nil || reply.Text == "" || len(reply.Added) != 2 {
		t.Errorf("ответ: %+v, %v", reply, err)
	}
}
