package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task10/internal/tokens"
	"github.com/AlexS8332/AITrenning_Task10/internal/tools"
)

// recorder — эмиттер для тестов: копит события.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) Log(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Kind)
	}
	return out
}

func echoTool() tools.Tool {
	return tools.Func{
		FuncName: "echo", FuncDescription: "повторяет аргументы",
		FuncParameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		FuncCall: func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct{ Text string }
			json.Unmarshal(args, &in)
			if in.Text == "boom" {
				return "", errors.New("инструмент сломался")
			}
			return `{"echo":"` + in.Text + `"}`, nil
		},
	}
}

func roles(ms []llm.Message) string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Role)
	}
	return strings.Join(out, ",")
}

func TestRunnerToolLoopWithHistory(t *testing.T) {
	history := []llm.Message{
		{Role: llm.RoleUser, Content: "меня зовут Алекс"},
		{Role: llm.RoleAssistant, Content: "запомнил"},
	}
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		switch llmtest.ToolReplies(req) {
		case 0:
			if !llmtest.HasTool(req, "echo") {
				t.Errorf("модели не переданы инструменты: %+v", req.Tools)
			}
			// Системный промпт, история, новое сообщение — в этом порядке.
			if got := roles(req.Messages); got != "system,user,assistant,user" {
				t.Errorf("порядок сообщений: %s", got)
			}
			if req.Messages[0].Content != "s" || req.Messages[1].Content != "меня зовут Алекс" || req.Messages[3].Content != "привет" {
				t.Errorf("история: %+v", req.Messages)
			}
			// Модель отвечает на часть вопроса и тут же просит инструмент.
			resp := llmtest.ToolCall("echo", `{"text":"раз"}`)
			resp.Message.Content = "Тебя зовут Алекс. Сейчас проверю."
			return resp, nil
		case 1:
			if !strings.Contains(llmtest.LastToolReply(req), `"echo":"раз"`) {
				t.Errorf("результат инструмента не дошёл: %q", llmtest.LastToolReply(req))
			}
			last := req.Messages[len(req.Messages)-1]
			prev := req.Messages[len(req.Messages)-2]
			if last.Role != llm.RoleTool || last.ToolCallID != prev.ToolCalls[0].ID {
				t.Errorf("связка вызова и ответа: %+v / %+v", prev, last)
			}
			return llmtest.Text("готово"), nil
		}
		return llm.Response{}, errors.New("лишний шаг")
	}}

	rec := &recorder{}
	r := Runner{LLM: fake, Model: "deepseek-v4-flash"}
	reply, err := r.Run(context.Background(), Spec{
		Name: "t", System: "s", Tools: []tools.Tool{echoTool()},
	}, Prepared{History: history, User: "привет"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	// Ответ пользователю — всё, что модель сказала за ход, а не только
	// последнее сообщение: текст рядом с вызовом инструмента тоже ответ.
	if reply.Text != "Тебя зовут Алекс. Сейчас проверю.\n\nготово" {
		t.Errorf("ответ: %q", reply.Text)
	}
	// В Added — только новое: сообщение пользователя, вызов, ответ
	// инструмента, текст. Истории и системного промпта там нет.
	if got := roles(reply.Added); got != "user,assistant,tool,assistant" {
		t.Errorf("добавленные сообщения: %s", got)
	}
	if reply.Added[0].Content != "привет" || reply.Added[3].Content != "готово" {
		t.Errorf("содержимое добавленных: %+v", reply.Added)
	}
	if reply.Stats.Steps != 2 || reply.Stats.ToolCalls != 1 || reply.Stats.Usage.Total != 43 {
		t.Errorf("статистика: %+v", reply.Stats)
	}
	if !reply.Stats.Cost.Known {
		t.Errorf("стоимость известной модели должна считаться")
	}

	kinds := strings.Join(rec.kinds(), " ")
	want := "prompt llm.request llm.response tool.call tool.result llm.request llm.response"
	if kinds != want {
		t.Errorf("события:\n есть  %s\n нужно %s", kinds, want)
	}

	var p Prompt
	if err := json.Unmarshal([]byte(rec.events[0].Detail), &p); err != nil {
		t.Fatalf("событие prompt не JSON: %v", err)
	}
	if p.System != "s" || p.User != "привет" || p.History != 2 || p.HistoryRunes == 0 {
		t.Errorf("промпт хода: %+v", p)
	}
	if len(p.Tools) != 1 || p.Tools[0].Name != "echo" {
		t.Errorf("инструменты в промпте: %+v", p.Tools)
	}
}

func TestRunnerTextWithoutTools(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if len(req.Tools) != 0 {
			t.Errorf("инструментов быть не должно: %+v", req.Tools)
		}
		return llmtest.Text("ответ"), nil
	}}
	r := Runner{LLM: fake, Model: "m"}
	reply, err := r.Run(context.Background(), Spec{Name: "chat", System: "s"}, Prepared{User: "вопрос"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "ответ" || roles(reply.Added) != "user,assistant" {
		t.Errorf("ответ: %+v", reply)
	}
	if reply.Stats.Cost.Known {
		t.Errorf("стоимость неизвестной модели не должна считаться")
	}
}

func TestRunnerStepLimitAndToolErrors(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		switch llmtest.ToolReplies(req) {
		case 0:
			return llmtest.ToolCall("echo", `{"text":"boom"}`), nil
		case 1:
			if !strings.Contains(llmtest.LastToolReply(req), "инструмент сломался") {
				t.Errorf("ошибка инструмента не дошла до модели: %q", llmtest.LastToolReply(req))
			}
			return llmtest.ToolCall("nope", `{}`), nil
		case 2:
			if !strings.Contains(llmtest.LastToolReply(req), "инструмента nope нет") {
				t.Errorf("неизвестный инструмент не объяснён: %q", llmtest.LastToolReply(req))
			}
			return llmtest.ToolCall("echo", `{"text":"ещё"}`), nil
		}
		return llmtest.ToolCall("echo", `{"text":"ещё"}`), nil
	}}

	rec := &recorder{}
	r := Runner{LLM: fake, Model: "m"}
	_, err := r.Run(context.Background(), Spec{
		Name: "t", System: "s", Tools: []tools.Tool{echoTool()}, MaxSteps: 3,
	}, Prepared{User: "q"}, rec)
	if !errors.Is(err, ErrStepLimit) {
		t.Fatalf("ожидался лимит шагов, получено: %v", err)
	}
	if fake.Calls() != 3 {
		t.Errorf("запросов к модели: %d", fake.Calls())
	}
	errs := 0
	for _, k := range rec.kinds() {
		if k == EventToolError {
			errs++
		}
	}
	if errs != 2 {
		t.Errorf("ошибок инструментов в журнале: %d", errs)
	}
}

func TestRunnerLLMError(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llm.Response{}, errors.New("сеть упала")
	}}
	r := Runner{LLM: fake, Model: "m"}
	_, err := r.Run(context.Background(), Spec{Name: "t", System: "s"}, Prepared{User: "q"}, nil)
	if err == nil || !strings.Contains(err.Error(), "сеть упала") {
		t.Errorf("ошибка модели должна дойти: %v", err)
	}
}

func TestParallelToolCallsInOneReply(t *testing.T) {
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCalls(llmtest.Call{Name: "echo", Args: `{"text":"a"}`}, llmtest.Call{Name: "echo", Args: `{"text":"b"}`}), nil
		}
		// Оба вызова должны получить ответ, каждый со своим id.
		if llmtest.ToolReplies(req) != 2 {
			t.Errorf("ответов инструментов: %d", llmtest.ToolReplies(req))
		}
		n := len(req.Messages)
		if req.Messages[n-1].ToolCallID == req.Messages[n-2].ToolCallID {
			t.Errorf("ответы инструментов ссылаются на один вызов")
		}
		return llmtest.Text("ок"), nil
	}}
	r := Runner{LLM: fake, Model: "m"}
	reply, err := r.Run(context.Background(), Spec{Name: "t", System: "s", Tools: []tools.Tool{echoTool()}}, Prepared{User: "q"}, nil)
	if err != nil || reply.Stats.ToolCalls != 2 {
		t.Errorf("ошибка %v, статистика %+v", err, reply.Stats)
	}
}

func TestPrettyJSONAndHelpers(t *testing.T) {
	if got := prettyJSON(`{"a":1}`); !strings.Contains(got, "\n  \"a\": 1") {
		t.Errorf("prettyJSON: %q", got)
	}
	if got := prettyJSON("не json"); got != "не json" {
		t.Errorf("prettyJSON должен вернуть не-JSON как есть: %q", got)
	}
	if got := sizeLabel(strings.Repeat("x", 1500)); got != "1.5 тыс. символов" {
		t.Errorf("sizeLabel: %q", got)
	}
	if got := sizeLabel("abc"); got != "3 символов" {
		t.Errorf("sizeLabel: %q", got)
	}
	if got := replyTitle(llmtest.Text("x")); got != "ответ текстом" {
		t.Errorf("replyTitle: %q", got)
	}
	if got := lastMessageDetail(nil); got != "" {
		t.Errorf("lastMessageDetail(nil): %q", got)
	}
	if got := lastMessageDetail([]llm.Message{{Role: llm.RoleTool, Content: "x"}}); got != "" {
		t.Errorf("ответ инструмента не должен показываться как запрос: %q", got)
	}
}

// longHistory — история из n ходов: сообщение пользователя и ответ модели.
// Нужна там, где важен объём контекста, а не его содержание.
func longHistory(n int) []llm.Message {
	var ms []llm.Message
	for i := 0; i < n; i++ {
		ms = append(ms,
			llm.Message{Role: llm.RoleUser, Content: strings.Repeat("расскажи ещё про рысь ", 20)},
			llm.Message{Role: llm.RoleAssistant, Content: strings.Repeat("Рысь — хищник семейства кошачьих. ", 20)},
		)
	}
	return ms
}

func TestRunnerCountsTokens(t *testing.T) {
	// Модель сообщает свой расход, агент — свою оценку до отправки.
	// В журнале должны быть оба числа и расхождение между ними.
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		resp := llmtest.Text("ответ")
		resp.Usage = llm.Usage{Prompt: 1000, Completion: 7, Total: 1007}
		return resp, nil
	}}
	rec := &recorder{}
	r := Runner{LLM: fake, Model: "m"}
	reply, err := r.Run(context.Background(), Spec{Name: "chat", System: "системный промпт"},
		Prepared{History: longHistory(3), User: "а чем она питается?"}, rec)
	if err != nil {
		t.Fatal(err)
	}

	c := reply.Stats.Context
	if c.Estimate.History == 0 || c.Estimate.User == 0 || c.Estimate.System == 0 {
		t.Errorf("разбивка оценки пустая: %+v", c.Estimate)
	}
	if c.Estimate.History <= c.Estimate.User {
		t.Errorf("история должна весить больше нового сообщения: %+v", c.Estimate)
	}
	if c.FirstPrompt != 1000 {
		t.Errorf("факт из ответа модели: %d", c.FirstPrompt)
	}
	if c.Peak == 0 {
		t.Errorf("пик контекста не посчитан")
	}

	var request, response *tokens.Tokens
	for _, ev := range rec.events {
		switch ev.Kind {
		case EventLLMRequest:
			request = ev.Tokens
		case EventLLMReply:
			response = ev.Tokens
		}
	}
	if request == nil || request.Estimated == 0 {
		t.Fatalf("в событии запроса нет оценки: %+v", request)
	}
	if response == nil || response.Actual != 1000 || response.Estimated != request.Estimated {
		t.Fatalf("в событии ответа нет сверки оценки с фактом: %+v", response)
	}
	if response.ErrorPct == 0 {
		t.Errorf("расхождение оценки с фактом не посчитано")
	}
}

func TestRunnerContextGrowsWithinTurn(t *testing.T) {
	// Внутри одного хода контекст растёт: ответ инструмента уходит модели
	// следующим запросом. Оценка должна это видеть, иначе переполнение
	// посреди хода останется незамеченным.
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		if llmtest.ToolReplies(req) == 0 {
			return llmtest.ToolCall("echo", `{"text":"`+strings.Repeat("длинный ответ ", 100)+`"}`), nil
		}
		return llmtest.Text("готово"), nil
	}}
	rec := &recorder{}
	r := Runner{LLM: fake, Model: "m"}
	if _, err := r.Run(context.Background(), Spec{
		Name: "t", System: "s", Tools: []tools.Tool{echoTool()}, MaxSteps: 3,
	}, Prepared{User: "вопрос"}, rec); err != nil {
		t.Fatal(err)
	}

	var estimates []int
	for _, ev := range rec.events {
		if ev.Kind == EventLLMRequest && ev.Tokens != nil {
			estimates = append(estimates, ev.Tokens.Estimated)
		}
	}
	if len(estimates) != 2 {
		t.Fatalf("ожидались два запроса к модели, оценок: %d", len(estimates))
	}
	if estimates[1] <= estimates[0] {
		t.Errorf("контекст внутри хода не вырос: %d → %d", estimates[0], estimates[1])
	}
}

func TestRunnerOverflowFails(t *testing.T) {
	// Лимит меньше истории: ход не отправляется вовсе, модель не
	// вызывается, а в ошибке видно, насколько промахнулись.
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("не должно случиться"), nil
	}}
	r := Runner{LLM: fake, Model: "m", ContextLimit: 200, OnOverflow: OverflowFail}
	_, err := r.Run(context.Background(), Spec{Name: "t", System: "s"}, Prepared{History: longHistory(5), User: "вопрос"}, nil)
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("ожидалось переполнение, получено: %v", err)
	}
	if !strings.Contains(err.Error(), "200") {
		t.Errorf("в ошибке должен быть лимит: %v", err)
	}
	if fake.Calls() != 0 {
		t.Errorf("при переполнении запрос уходить не должен, запросов: %d", fake.Calls())
	}
}

func TestRunnerOverflowTrims(t *testing.T) {
	// В режиме trim ход всё-таки идёт, но модель получает историю короче:
	// самые старые ходы выброшены целиком.
	var got llm.Request
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		got = req
		return llmtest.Text("ответ"), nil
	}}
	rec := &recorder{}
	full := longHistory(6)
	r := Runner{LLM: fake, Model: "m", ContextLimit: 900, OnOverflow: OverflowTrim}
	reply, err := r.Run(context.Background(), Spec{Name: "t", System: "s"}, Prepared{History: full, User: "вопрос"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Stats.Context.Trimmed == 0 {
		t.Errorf("история должна была обрезаться")
	}
	// Системный промпт, остаток истории и новое сообщение.
	if len(got.Messages) >= len(full)+2 {
		t.Errorf("модель получила всю историю: сообщений %d из %d", len(got.Messages), len(full)+2)
	}
	// Обрезка идёт ходами: первым сообщением истории остаётся вопрос
	// пользователя, иначе ответ инструмента остался бы без своего вызова.
	if got.Messages[1].Role != llm.RoleUser {
		t.Errorf("история после обрезки начинается с %q", got.Messages[1].Role)
	}
	if reply.Stats.Context.Estimate.Total > 900 {
		t.Errorf("после обрезки всё ещё не влезает: %+v", reply.Stats.Context.Estimate)
	}
	found := false
	for _, ev := range rec.events {
		if ev.Kind == EventNote && strings.Contains(ev.Title, "обрезана") {
			found = true
		}
	}
	if !found {
		t.Errorf("обрезка должна попасть в журнал: %v", rec.kinds())
	}
}

func TestRunnerOverflowOffLetsAPIAnswer(t *testing.T) {
	// В режиме off свой лимит не мешает: запрос уходит целиком, и отвечает
	// на переполнение сам API. Так ставится опыт с настоящим лимитом модели.
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llm.Response{}, errors.New("API вернул 400 Bad Request: input exceeds context length")
	}}
	r := Runner{LLM: fake, Model: "m", ContextLimit: 100, OnOverflow: OverflowOff}
	_, err := r.Run(context.Background(), Spec{Name: "t", System: "s"}, Prepared{History: longHistory(5), User: "вопрос"}, nil)
	if err == nil || !strings.Contains(err.Error(), "context length") {
		t.Fatalf("ошибка API должна дойти как есть: %v", err)
	}
	if fake.Calls() != 1 {
		t.Errorf("запросов к модели: %d, ждали один", fake.Calls())
	}
}

func TestRunnerNoLimitDoesNotCheck(t *testing.T) {
	// Лимит 0 — проверки нет вовсе: оценка считается, но ничего не решает.
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		return llmtest.Text("ответ"), nil
	}}
	r := Runner{LLM: fake, Model: "m"}
	reply, err := r.Run(context.Background(), Spec{Name: "t", System: "s"}, Prepared{History: longHistory(10), User: "вопрос"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Stats.Context.Limit != 0 || reply.Stats.Context.Estimate.Total == 0 {
		t.Errorf("без лимита оценка всё равно нужна: %+v", reply.Stats.Context)
	}
}
