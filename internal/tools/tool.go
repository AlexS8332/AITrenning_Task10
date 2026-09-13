// Package tools — инструменты, которые агент может вызывать через модель.
// Инструмент описывает себя для модели (имя, назначение, схема аргументов)
// и исполняет вызов. Результат всегда текст с JSON: так его удобно и
// отдать модели, и показать в логе.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
)

// Tool — один инструмент.
type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// Func — инструмент из замыкания. Достаточно для всех инструментов
// проекта: состояние живёт в замыкании, а не в отдельном типе.
type Func struct {
	FuncName        string
	FuncDescription string
	FuncParameters  json.RawMessage
	FuncCall        func(ctx context.Context, args json.RawMessage) (string, error)
}

func (f Func) Name() string                { return f.FuncName }
func (f Func) Description() string         { return f.FuncDescription }
func (f Func) Parameters() json.RawMessage { return f.FuncParameters }
func (f Func) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return f.FuncCall(ctx, args)
}

// Defs собирает описания инструментов для запроса к модели.
func Defs(ts []Tool) []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(ts))
	for _, t := range ts {
		defs = append(defs, llm.NewToolDef(t.Name(), t.Description(), t.Parameters()))
	}
	return defs
}

// Registry — набор инструментов по имени. Агент собирает из него своё
// подмножество: у каждого типа агента свои инструменты.
type Registry struct {
	byName map[string]Tool
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		r.byName[t.Name()] = t
	}
	return r
}

// Get возвращает инструмент по имени.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Pick выбирает инструменты по именам. Неизвестное имя — ошибка программиста,
// а не данных, поэтому паника: агент с несуществующим инструментом не должен
// собираться вовсе.
func (r *Registry) Pick(names ...string) []Tool {
	ts := make([]Tool, 0, len(names))
	for _, name := range names {
		t, ok := r.byName[name]
		if !ok {
			panic(fmt.Sprintf("инструмент %q не зарегистрирован", name))
		}
		ts = append(ts, t)
	}
	return ts
}

// Names — имена всех инструментов реестра по алфавиту.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ParseArgs разбирает аргументы вызова. Пустая строка — допустимый вызов
// без аргументов: модели иногда так и присылают.
func ParseArgs(args json.RawMessage, target any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, target); err != nil {
		return fmt.Errorf("аргументы не разобрались: %w", err)
	}
	return nil
}

// Result сериализует результат инструмента для модели. Ошибка сериализации
// здесь невозможна для наших типов, но молча вернуть пустоту нельзя.
func Result(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("сериализация результата: %w", err)
	}
	return string(data), nil
}

// Truncate обрезает длинный текст по рунам, чтобы результат инструмента не
// раздувал контекст модели. Обрыв помечается, иначе модель не узнает, что
// текста было больше.
func Truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + " …[обрезано]"
}
