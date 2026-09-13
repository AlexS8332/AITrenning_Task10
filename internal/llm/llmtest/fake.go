// Package llmtest — подставная модель для тестов: отвечает по сценарию,
// без сети. Сценарий смотрит на запрос и решает, что вернуть.
package llmtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
)

// Fake — модель, которую ведёт функция Fn. Requests копит все запросы:
// тест проверяет по ним, что ушло модели.
type Fake struct {
	Fn func(req llm.Request) (llm.Response, error)

	mu       sync.Mutex
	Requests []llm.Request
}

func (f *Fake) Chat(_ context.Context, req llm.Request) (llm.Response, error) {
	f.mu.Lock()
	f.Requests = append(f.Requests, req)
	f.mu.Unlock()
	if f.Fn == nil {
		return llm.Response{}, fmt.Errorf("сценарий не задан")
	}
	return f.Fn(req)
}

// Calls — сколько запросов получила модель.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}

// Text — ответ текстом.
func Text(s string) llm.Response {
	return llm.Response{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: s},
		FinishReason: "stop",
		Usage:        llm.Usage{Prompt: 10, Completion: 5, Total: 15},
	}
}

// ToolCall — ответ с одним вызовом инструмента.
func ToolCall(name, args string) llm.Response {
	return ToolCalls(Call{Name: name, Args: args})
}

// Call — вызов для ToolCalls.
type Call struct {
	Name string
	Args string
}

// ToolCalls — ответ с несколькими вызовами инструментов.
func ToolCalls(calls ...Call) llm.Response {
	msg := llm.Message{Role: llm.RoleAssistant}
	for i, c := range calls {
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID:       fmt.Sprintf("call_%d_%s", i, c.Name),
			Type:     "function",
			Function: llm.FunctionCall{Name: c.Name, Arguments: c.Args},
		})
	}
	return llm.Response{
		Message:      msg,
		FinishReason: llm.FinishToolCalls,
		Usage:        llm.Usage{Prompt: 20, Completion: 8, Total: 28},
	}
}

// HasTool — есть ли у запроса инструмент с таким именем.
func HasTool(req llm.Request, name string) bool {
	for _, t := range req.Tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

// ToolReplies — сколько ответов инструментов уже в истории: по ним сценарий
// понимает, на каком шаге агент.
func ToolReplies(req llm.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			n++
		}
	}
	return n
}

// LastToolReply — содержимое последнего ответа инструмента.
func LastToolReply(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleTool {
			return req.Messages[i].Content
		}
	}
	return ""
}
