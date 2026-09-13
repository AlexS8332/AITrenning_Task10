package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/agents"
	"github.com/AlexS8332/AITrenning_Task10/internal/history"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task10/internal/strategy"
	"github.com/AlexS8332/AITrenning_Task10/internal/tools"
)

func stubTools() *tools.Registry {
	stub := func(name string) tools.Tool {
		return tools.Func{
			FuncName: name, FuncDescription: name,
			FuncParameters: json.RawMessage(`{"type":"object","properties":{}}`),
			FuncCall: func(context.Context, json.RawMessage) (string, error) {
				return `{"title":"Обыкновенная рысь","text":"` + strings.Repeat("рысь ", 300) + `"}`, nil
			},
		}
	}
	return tools.NewRegistry(stub("search_wikipedia"), stub("read_wikipedia"), stub("match_taxon"), stub("taxon_tree"), stub("vernacular_names"))
}

// echoModel — модель, которая на первом шаге хода зовёт инструмент, а на
// втором отвечает текстом, повторяя, сколько сообщений истории видит.
func echoModel() *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == llm.RoleUser && llmtest.HasTool(req, "read_wikipedia") {
			return llmtest.ToolCall("read_wikipedia", `{"title":"Рысь"}`), nil
		}
		var users []string
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser {
				users = append(users, m.Content)
			}
		}
		return llmtest.Text(fmt.Sprintf("сообщений: %02d", len(req.Messages)) + "; вопросы: " + strings.Join(users, " | ")), nil
	}}
}

func newManager(t *testing.T, dir string, fake *llmtest.Fake, keep int) *Manager {
	t.Helper()
	deps := agents.Deps{
		Runner:        agent.Runner{LLM: fake, Model: "m"},
		Tools:         stubTools(),
		KeepToolRunes: keep,
	}
	return NewManager(deps, history.NewStore(dir), 5*time.Second)
}

// newFactsManager — менеджер со стратегией фактов: окно в четыре
// сообщения и карточка ключ-значение, которая обновляется на каждом ходе.
func newFactsManager(t *testing.T, dir string, fake *llmtest.Fake) *Manager {
	t.Helper()
	deps := agents.Deps{
		Runner:   agent.Runner{LLM: fake, Model: "m"},
		Tools:    stubTools(),
		Strategy: strategy.Policy{Mode: strategy.ModeFacts, Keep: 4},
	}
	return NewManager(deps, history.NewStore(dir), 5*time.Second)
}

// isExtract — запрос к извлекателю фактов узнаётся по его системному
// промпту.
func isExtract(req llm.Request) bool {
	return len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "карточку фактов диалога")
}

// wait дожидается конца хода через подписку и возвращает итоговый вид.
func wait(t *testing.T, s *Session) View {
	t.Helper()
	_, updates, unsubscribe := s.Subscribe()
	defer unsubscribe()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				return s.View()
			}
		case <-deadline:
			t.Fatal("ход не завершился")
		}
	}
}

func TestConversationSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	// Первый запуск: диалог из двух ходов.
	fake := echoModel()
	m1 := newManager(t, dir, fake, 0)
	if n, errs := m1.Load(); n != 0 || len(errs) != 0 {
		t.Fatalf("пустое хранилище: %d %v", n, errs)
	}
	s1, err := m1.Start("tools", "  меня зовут Алекс, расскажи про рысь ")
	if err != nil {
		t.Fatal(err)
	}
	v1 := wait(t, s1)
	if v1.Status != StatusDone || v1.History != 0 || v1.SaveError != "" {
		t.Fatalf("первый ход: %+v", v1)
	}
	if !strings.HasPrefix(v1.Reply, "сообщений: 04") {
		t.Errorf("первый ответ: %q", v1.Reply)
	}
	if v1.Totals.LLMCalls != 2 || v1.Totals.ToolCalls != 1 || v1.Events == 0 {
		t.Errorf("итоги первого хода: %+v", v1)
	}

	convID := v1.ConversationID
	s2, err := m1.Send(convID, "а чем она питается?")
	if err != nil {
		t.Fatal(err)
	}
	v2 := wait(t, s2)
	// История: user, assistant(tool_calls), tool, assistant = 4 сообщения.
	if v2.History != 4 || !strings.Contains(v2.Reply, "вопросы: меня зовут Алекс, расскажи про рысь | а чем она питается?") {
		t.Errorf("второй ход не видит историю: %+v", v2)
	}

	list := m1.List()
	if len(list) != 1 || list[0].Turns != 2 || list[0].Messages != 8 || list[0].Title != "меня зовут Алекс, расскажи про рысь" || list[0].Running {
		t.Errorf("список: %+v", list)
	}
	if list[0].Totals.LLMCalls != 4 || list[0].Runes == 0 || list[0].AgentTitle == "" {
		t.Errorf("сводка: %+v", list[0])
	}
	path, raw, err := m1.Raw(convID)
	if err != nil || !strings.HasSuffix(path, convID+".json") || !strings.Contains(string(raw), `"role": "tool"`) {
		t.Errorf("файл диалога: %v %s", err, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл диалога не записан: %v", err)
	}

	// «Перезапуск»: новый менеджер и новая модель на том же каталоге.
	fake2 := echoModel()
	m2 := newManager(t, dir, fake2, 0)
	n, errs := m2.Load()
	if n != 1 || len(errs) != 0 {
		t.Fatalf("после перезапуска: диалогов %d, ошибки %v", n, errs)
	}
	d, ok := m2.Get(convID)
	if !ok || len(d.Messages) != 8 || len(d.Turns) != 2 || d.Turns[1].User != "а чем она питается?" || d.Active != nil {
		t.Fatalf("диалог после перезапуска: %+v", d.Summary)
	}
	if len(d.Turns[0].Events) == 0 || d.Turns[0].Events[0].Kind != agent.EventAgentStart {
		t.Errorf("журнал хода не восстановлен: %+v", d.Turns[0].Events)
	}

	s3, err := m2.Send(convID, "как меня зовут?")
	if err != nil {
		t.Fatal(err)
	}
	v3 := wait(t, s3)
	if v3.History != 8 || !strings.Contains(v3.Reply, "меня зовут Алекс, расскажи про рысь | а чем она питается? | как меня зовут?") {
		t.Errorf("после перезапуска агент не помнит диалог: %+v", v3)
	}
	// Модель второго запуска получила всю историю первого, включая вызовы
	// инструментов с их идентификаторами.
	first := fake2.Requests[0].Messages
	if len(first) != 10 || first[0].Role != llm.RoleSystem || first[2].ToolCalls == nil || first[3].ToolCallID != first[2].ToolCalls[0].ID {
		t.Errorf("история в запросе после перезапуска: %d сообщений", len(first))
	}

	d, _ = m2.Get(convID)
	if len(d.Messages) != 12 || len(d.Turns) != 3 || d.Updated.Before(d.Created) {
		t.Errorf("диалог после третьего хода: %+v", d.Summary)
	}

	// И третий запуск видит все три хода.
	m3 := newManager(t, dir, echoModel(), 0)
	m3.Load()
	if d, ok := m3.Get(convID); !ok || len(d.Turns) != 3 {
		t.Errorf("третий запуск: %+v", d.Summary)
	}
}

func TestCompactionAppliesToOldTurnsOnly(t *testing.T) {
	fake := echoModel()
	m := newManager(t, t.TempDir(), fake, 100)
	s, _ := m.Start("tools", "рысь")
	wait(t, s)
	s2, _ := m.Send(s.View().ConversationID, "ещё")
	wait(t, s2)

	// На втором ходе старый ответ инструмента ушёл сокращённым, а в файле
	// он целый.
	req := fake.Requests[2].Messages
	if req[3].Role != llm.RoleTool || !strings.Contains(req[3].Content, "[сокращено") {
		t.Errorf("старый ответ инструмента не сокращён: %q", req[3].Content[:50])
	}
	d, _ := m.Get(s.View().ConversationID)
	if strings.Contains(d.Messages[2].Content, "[сокращено") || len([]rune(d.Messages[2].Content)) < 1000 {
		t.Errorf("в истории должен лежать полный текст")
	}
}

func TestBusyNotFoundAndDelete(t *testing.T) {
	release := make(chan struct{})
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("ок"), nil
	}}
	m := newManager(t, t.TempDir(), fake, 0)

	if _, err := m.Start("nope", "x"); err == nil {
		t.Errorf("неизвестный агент должен отвергаться")
	}
	if _, err := m.Start("chat", "  "); err == nil {
		t.Errorf("пустое сообщение должно отвергаться")
	}
	if _, err := m.Send("0123456789abcdef", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Send в несуществующий диалог: %v", err)
	}

	s, err := m.Start("chat", "привет")
	if err != nil {
		t.Fatal(err)
	}
	id := s.View().ConversationID
	if _, err := m.Send(id, "ещё"); !errors.Is(err, ErrBusy) {
		t.Errorf("второй ход во время первого: %v", err)
	}
	if err := m.Delete(id); !errors.Is(err, ErrBusy) {
		t.Errorf("удаление во время хода: %v", err)
	}
	d, _ := m.Get(id)
	if d.Active == nil || d.Active.Status != StatusRunning || !d.Running {
		t.Errorf("идущий ход должен быть виден: %+v", d.Summary)
	}
	if got, ok := m.Turn(s.View().ID); !ok || got != s {
		t.Errorf("ход по идентификатору не находится")
	}

	close(release)
	wait(t, s)

	if _, err := m.Send(id, "ещё"); err != nil {
		t.Fatal(err)
	}
	wait(t, mustActive(t, m, id))
	if d, _ = m.Get(id); d.Active != nil || len(d.Turns) != 2 {
		t.Errorf("после второго хода: %+v", d.Summary)
	}

	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(id); ok {
		t.Errorf("диалог должен исчезнуть из памяти")
	}
	if _, _, err := m.Raw(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("файл удалённого диалога: %v", err)
	}
	if err := m.Delete(id); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторное удаление: %v", err)
	}
	if len(m.List()) != 0 {
		t.Errorf("список после удаления: %+v", m.List())
	}
}

func mustActive(t *testing.T, m *Manager, id string) *Session {
	t.Helper()
	d, ok := m.Get(id)
	if !ok || d.Active == nil {
		t.Fatalf("ход в диалоге %s не идёт", id)
	}
	s, _ := m.Turn(d.Active.ID)
	return s
}

func TestFailedTurnKeepsHistoryIntact(t *testing.T) {
	calls := 0
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		calls++
		if calls == 2 {
			return llm.Response{}, errors.New("сеть упала")
		}
		return llmtest.Text("ок"), nil
	}}
	m := newManager(t, t.TempDir(), fake, 0)
	s, _ := m.Start("chat", "раз")
	wait(t, s)
	s2, _ := m.Send(s.View().ConversationID, "два")
	v := wait(t, s2)
	if v.Status != StatusFailed || !strings.Contains(v.Error, "сеть упала") {
		t.Fatalf("неудачный ход: %+v", v)
	}
	d, _ := m.Get(s.View().ConversationID)
	if len(d.Messages) != 2 || len(d.Turns) != 2 || d.Turns[1].Status != history.TurnFailed || d.Turns[1].Messages != 0 {
		t.Errorf("после неудачного хода: сообщений %d, ходов %+v", len(d.Messages), d.Turns)
	}
	// Следующий ход идёт по чистой истории: неудачное сообщение в неё не
	// попало.
	s3, _ := m.Send(s.View().ConversationID, "три")
	wait(t, s3)
	last := fake.Requests[2].Messages
	if len(last) != 4 || last[3].Content != "три" {
		t.Errorf("история после неудачного хода: %+v", last)
	}
}

func TestSubscribeAfterCloseAndEviction(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("ок"), nil
	}}
	m := newManager(t, t.TempDir(), fake, 0)
	s, _ := m.Start("chat", "раз")
	wait(t, s)

	snap, ch, unsubscribe := s.Subscribe()
	unsubscribe()
	if _, ok := <-ch; ok {
		t.Errorf("подписка на завершённый ход должна закрываться сразу")
	}
	if snap.View.Status != StatusDone || len(snap.Events) == 0 {
		t.Errorf("снимок завершённого хода: %+v", snap.View)
	}

	// Вытеснение: больше maxTurnsInMemory ходов, старые уходят из памяти.
	id := s.View().ConversationID
	for i := 0; i < maxTurnsInMemory+5; i++ {
		s2, err := m.Send(id, "ещё")
		if err != nil {
			t.Fatal(err)
		}
		wait(t, s2)
	}
	if _, ok := m.Turn(s.View().ID); ok {
		t.Errorf("первый ход должен быть вытеснен из памяти")
	}
	if d, _ := m.Get(id); len(d.Turns) != maxTurnsInMemory+6 {
		t.Errorf("в истории должны остаться все ходы: %d", len(d.Turns))
	}
}

// Диалог, в котором идёт первый ход, интерфейс запрашивает сразу после
// создания: сообщений в нём ещё нет. В JSON должен уйти пустой список, а
// не null — на null лента падала на messages.length.
func TestGetFreshConversation(t *testing.T) {
	release := make(chan struct{})
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("готово"), nil
	}}
	m := newManager(t, t.TempDir(), fake, 0)

	session, err := m.Start("chat", "привет")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := m.Get(session.View().ConversationID)
	if !ok {
		t.Fatal("диалог не найден сразу после создания")
	}
	if d.Messages == nil || d.Turns == nil {
		t.Errorf("сообщения %v, ходы %v", d.Messages, d.Turns)
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"messages":[]`) {
		t.Errorf("в JSON идущего диалога должен быть пустой список сообщений: %s", data)
	}

	close(release)
	wait(t, session)
}

func TestFactsAreStoredWithConversation(t *testing.T) {
	// Карточка фактов — часть диалога, а не переменная в памяти процесса:
	// после перезапуска она должна подниматься из файла вместе с
	// сообщениями, иначе агент забудет начало разговора, за извлечение
	// которого уже заплачено.
	dir := t.TempDir()
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isExtract(req) {
			return llmtest.Text(`{"set":[{"key":"имя","value":"Алекс"},{"key":"город","value":"Иркутск"}]}`), nil
		}
		return llmtest.Text("ответ про рысь: " + strings.Repeat("хищник семейства кошачьих. ", 20)), nil
	}}
	m := newFactsManager(t, dir, fake)

	s, err := m.Start("chat", "меня зовут Алекс, я из Иркутска, расскажи про рысь")
	if err != nil {
		t.Fatal(err)
	}
	convID := wait(t, s).ConversationID
	for _, q := range []string{"чем она питается?", "где обитает?", "опасна ли она?"} {
		next, err := m.Send(convID, q)
		if err != nil {
			t.Fatal(err)
		}
		if v := wait(t, next); v.Status != StatusDone {
			t.Fatalf("ход %q: %+v", q, v)
		}
	}

	d, ok := m.Get(convID)
	if !ok || d.FactsCard.Version == 0 {
		t.Fatalf("карточка не собралась: %+v", d.FactsCard)
	}
	if v, ok := d.FactsCard.Get("имя"); !ok || v != "Алекс" {
		t.Errorf("карточка: %+v", d.FactsCard.Entries)
	}
	if d.Facts.Version != d.FactsCard.Version || d.Facts.Runes == 0 || d.Facts.Count != 2 {
		t.Errorf("сводка о карточке: %+v", d.Facts)
	}
	// Извлекатель зовётся на каждом ходе, и за это платят.
	if d.Facts.Calls != 4 || d.Facts.Usage.Total == 0 {
		t.Errorf("расход карточки: %+v", d.Facts)
	}
	// Сообщения из файла никуда не делись: режется только то, что уходит
	// модели.
	if len(d.Messages) != 8 {
		t.Errorf("сообщений в диалоге: %d", len(d.Messages))
	}

	_, raw, err := m.Raw(convID)
	if err != nil || !strings.Contains(string(raw), `"key": "имя"`) {
		t.Errorf("карточки нет в файле диалога: %v", err)
	}

	restored := newFactsManager(t, dir, fake)
	if n, errs := restored.Load(); n != 1 || len(errs) != 0 {
		t.Fatalf("после перезапуска: %d %v", n, errs)
	}
	after, _ := restored.Get(convID)
	if after.Facts.Count != d.Facts.Count || after.Facts.Version != d.Facts.Version {
		t.Errorf("карточка не пережила перезапуск: %+v", after.FactsCard)
	}
}

// Ветвление в менеджере: точка, две ветки от неё, переключение между
// ними. Ветки должны жить в одном файле и не видеть сообщений друг друга.
func TestBranchesAreIndependentAndSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		var users []string
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser {
				users = append(users, m.Content)
			}
		}
		return llmtest.Text("вопросы: " + strings.Join(users, " | ")), nil
	}}
	m := newManager(t, dir, fake, 0)

	s, err := m.Start("analyst", "собираем ТЗ")
	if err != nil {
		t.Fatal(err)
	}
	convID := wait(t, s).ConversationID
	next, _ := m.Send(convID, "срок 1 марта")
	wait(t, next)

	d, cp, err := m.Mark(convID, "перед выбором")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Checkpoints) != 1 || cp.At != 4 || cp.Turn != 2 {
		t.Fatalf("точка сохранения: %+v", cp)
	}

	// Первая ветка от точки.
	d, first, err := m.Fork(convID, cp.ID, "Kotlin")
	if err != nil {
		t.Fatal(err)
	}
	if d.BranchID != first.ID {
		t.Errorf("после ветвления разговор должен идти в новой ветке: %+v", d.BranchID)
	}
	one, _ := m.Send(convID, "делаем нативное приложение")
	wait(t, one)

	// Вторая ветка от той же точки.
	_, second, err := m.Fork(convID, cp.ID, "PWA")
	if err != nil {
		t.Fatal(err)
	}
	two, _ := m.Send(convID, "делаем PWA")
	v := wait(t, two)
	if strings.Contains(v.Reply, "нативное") {
		t.Errorf("во вторую ветку попал ход первой: %q", v.Reply)
	}
	if !strings.Contains(v.Reply, "собираем ТЗ") || !strings.Contains(v.Reply, "срок 1 марта") {
		t.Errorf("вторая ветка потеряла общее начало: %q", v.Reply)
	}
	if v.BranchName != "PWA" {
		t.Errorf("ход должен помнить свою ветку: %q", v.BranchName)
	}

	d, _ = m.Get(convID)
	if len(d.Tree) != 3 || d.Branches != 3 {
		t.Fatalf("дерево: %+v", d.Tree)
	}
	if d.Tree[0].PathTurns != 2 || d.Tree[1].PathTurns != 3 || d.Tree[2].PathTurns != 3 {
		t.Errorf("ходы в путях веток: %+v", d.Tree)
	}
	if !d.Tree[2].Active || d.Tree[0].Active {
		t.Errorf("активной должна быть последняя ветка: %+v", d.Tree)
	}

	// Переключение обратно: модель снова видит первую ветку.
	d, err = m.Switch(convID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.BranchName != "Kotlin" || len(d.Messages) != 6 {
		t.Errorf("после переключения: %q, сообщений %d", d.BranchName, len(d.Messages))
	}
	back, _ := m.Send(convID, "что мы выбрали?")
	vb := wait(t, back)
	if strings.Contains(vb.Reply, "PWA") {
		t.Errorf("в первую ветку попал ход второй: %q", vb.Reply)
	}

	if _, err := m.Switch(convID, "нет такой"); err == nil {
		t.Errorf("переключение в несуществующую ветку должно быть ошибкой")
	}
	if _, _, err := m.Fork(convID, "нет такой точки", "х"); err == nil {
		t.Errorf("ветвление от несуществующей точки должно быть ошибкой")
	}
	if _, _, err := m.Mark("0123456789abcdef", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("точка в несуществующем диалоге: %v", err)
	}

	// Всё дерево в одном файле и переживает перезапуск.
	restored := newManager(t, dir, fake, 0)
	if n, errs := restored.Load(); n != 1 || len(errs) != 0 {
		t.Fatalf("после перезапуска: %d %v", n, errs)
	}
	after, ok := restored.Get(convID)
	if !ok || after.Branches != 3 || after.CheckpointCount != 1 {
		t.Fatalf("дерево не пережило перезапуск: %+v", after.Summary)
	}
	if after.BranchID != first.ID || len(after.Messages) != 8 {
		t.Errorf("текущая ветка после перезапуска: %q, сообщений %d", after.BranchName, len(after.Messages))
	}
	_ = second
}

// Ветвление без точки — обычный случай: пользователь жмёт «ветвиться»
// прямо на текущем конце разговора, и точка ставится сама.
func TestForkFromTipCreatesCheckpoint(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) { return llmtest.Text("ок"), nil }}
	m := newManager(t, t.TempDir(), fake, 0)
	s, _ := m.Start("analyst", "собираем ТЗ")
	convID := wait(t, s).ConversationID

	d, b, err := m.Fork(convID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Checkpoints) != 1 || d.Checkpoints[0].At != 2 {
		t.Errorf("точка должна ставиться сама: %+v", d.Checkpoints)
	}
	if b.Name == "" || d.BranchName != b.Name {
		t.Errorf("ветке нужно имя по умолчанию: %q", b.Name)
	}
}

// Переключатель стратегий: диалог продолжается тем же деревом, но модели
// уходит другое. Сообщения при этом не трогаются.
func TestSetStrategySwitchesMidDialog(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if isExtract(req) {
			return llmtest.Text(`{"set":[{"key":"цель","value":"учёт списаний"}]}`), nil
		}
		return llmtest.Text("ок"), nil
	}}
	m := newFactsManager(t, t.TempDir(), fake)
	s, _ := m.Start("analyst", "цель — учёт списаний")
	convID := wait(t, s).ConversationID

	d, err := m.SetStrategy(convID, strategy.ModeWindow)
	if err != nil {
		t.Fatal(err)
	}
	if d.Strategy != strategy.ModeWindow {
		t.Fatalf("стратегия не переключилась: %q", d.Strategy)
	}
	before := fake.Calls()
	next, _ := m.Send(convID, "и ещё")
	wait(t, next)
	if fake.Calls() != before+1 {
		t.Errorf("после перехода на окно извлекатель зваться не должен: %d запросов", fake.Calls()-before)
	}

	// Карточка осталась в файле и снова работает, если вернуться.
	d, err = m.SetStrategy(convID, strategy.ModeFacts)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.FactsCard.Entries) == 0 {
		t.Errorf("карточка должна была сохраниться: %+v", d.FactsCard)
	}
	if _, err := m.SetStrategy(convID, "summary"); err == nil {
		t.Errorf("неизвестная стратегия должна отвергаться")
	}
	if _, err := m.SetStrategy("0123456789abcdef", strategy.ModeFull); !errors.Is(err, ErrNotFound) {
		t.Errorf("стратегия несуществующего диалога: %v", err)
	}
}

func TestComparisonRunsEveryModeOnTheSameQuestion(t *testing.T) {
	// Стенд сравнения: один вопрос уходит трём дорожкам, отличающимся
	// только тем, что они помнят. Разница в ответах должна объясняться
	// памятью, а не разными разговорами.
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("ответ на " + req.Messages[len(req.Messages)-1].Content), nil
	}}
	m := newFactsManager(t, t.TempDir(), fake)

	group, sessions, err := m.StartComparison("chat", "  расскажи про рысь  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != len(ComparisonModes) {
		t.Fatalf("дорожек: %d", len(sessions))
	}
	for _, s := range sessions {
		if v := wait(t, s); v.Status != StatusDone {
			t.Fatalf("ход дорожки: %+v", v)
		}
	}

	cmp, ok := m.Comparison(group)
	if !ok || len(cmp.Lanes) != len(ComparisonModes) {
		t.Fatalf("сравнение: %+v", cmp)
	}
	for i, lane := range cmp.Lanes {
		if lane.Strategy != ComparisonModes[i] {
			t.Errorf("дорожка %d: режим %q вместо %q", i, lane.Strategy, ComparisonModes[i])
		}
		if lane.Group != group || len(lane.Turns) != 1 || lane.Turns[0].User != "расскажи про рысь" {
			t.Errorf("дорожка %d: %+v", i, lane.Summary)
		}
	}
	if cmp.Title == "" || cmp.Updated.Before(cmp.Created) {
		t.Errorf("сводка сравнения: %+v", cmp)
	}

	next, err := m.SendComparison(group, "а чем она питается?")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range next {
		wait(t, s)
	}
	cmp, _ = m.Comparison(group)
	for i, lane := range cmp.Lanes {
		if len(lane.Turns) != 2 || lane.Turns[1].User != "а чем она питается?" {
			t.Errorf("дорожка %d не получила второй вопрос: %d ходов", i, len(lane.Turns))
		}
	}

	if _, ok := m.Comparison("нет такого"); ok {
		t.Error("несуществующее сравнение не должно находиться")
	}
	if _, err := m.SendComparison("нет такого", "вопрос"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ожидался ErrNotFound, получено: %v", err)
	}
}

func TestComparisonKeepsLanesInStep(t *testing.T) {
	// Пока хоть одна дорожка отвечает, следующий вопрос не уходит никому:
	// иначе ходы разъедутся и сравнивать станет нечего.
	release := make(chan struct{})
	fake := &llmtest.Fake{Fn: func(llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("ок"), nil
	}}
	m := newFactsManager(t, t.TempDir(), fake)

	group, sessions, err := m.StartComparison("chat", "первый вопрос")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.SendComparison(group, "второй вопрос"); !errors.Is(err, ErrBusy) {
		t.Errorf("ожидался ErrBusy, получено: %v", err)
	}
	close(release)
	for _, s := range sessions {
		wait(t, s)
	}
	later, err := m.SendComparison(group, "второй вопрос")
	if err != nil {
		t.Errorf("после ответа вопрос должен уходить: %v", err)
	}
	// Дождаться надо всех: иначе ход допишет файл уже после того, как
	// тест убрал за собой каталог.
	for _, s := range later {
		wait(t, s)
	}
}

func TestStrategySurvivesRestart(t *testing.T) {
	// Режим работы с историей — свойство диалога, а не настроек сервера:
	// дорожка сравнения обязана продолжаться по своим правилам даже после
	// перезапуска, когда сервер поднят с другим режимом по умолчанию.
	dir := t.TempDir()
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("ответ"), nil
	}}
	m := newFactsManager(t, dir, fake)
	group, sessions, err := m.StartComparison("chat", "расскажи про рысь")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		wait(t, s)
	}

	restored := newManager(t, dir, fake, 0) // сервер с умолчанием без сравнения
	if n, errs := restored.Load(); n != len(ComparisonModes) || len(errs) != 0 {
		t.Fatalf("после перезапуска: %d %v", n, errs)
	}
	cmp, ok := restored.Comparison(group)
	if !ok {
		t.Fatal("сравнение не поднялось из файлов")
	}
	for i, lane := range cmp.Lanes {
		if lane.Strategy != ComparisonModes[i] {
			t.Errorf("дорожка %d после перезапуска: %q", i, lane.Strategy)
		}
	}
}
