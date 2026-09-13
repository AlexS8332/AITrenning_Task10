package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatSendsToolsAndParsesToolCalls(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("путь %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer key-1" {
			t.Errorf("заголовок Authorization: %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("тело: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
  "model": "deepseek-v4-flash",
  "choices": [{"message": {"role": "assistant", "content": null,
    "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "search", "arguments": "{\"q\":\"рысь\"}"}}]},
    "finish_reason": "tool_calls"}],
  "usage": {"prompt_tokens": 100, "completion_tokens": 7, "total_tokens": 107,
    "prompt_cache_hit_tokens": 60, "prompt_cache_miss_tokens": 40,
    "completion_tokens_details": {"reasoning_tokens": 0}}
}`))
	}))
	defer srv.Close()

	c := NewClient("key-1", srv.URL+"/")
	resp, err := c.Chat(context.Background(), Request{
		Model:       "deepseek-v4-flash",
		Messages:    []Message{{Role: RoleSystem, Content: "s"}, {Role: RoleUser, Content: "u"}},
		Tools:       []ToolDef{NewToolDef("search", "поиск", json.RawMessage(`{"type":"object"}`))},
		Temperature: 0,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if got["model"] != "deepseek-v4-flash" {
		t.Errorf("model: %v", got["model"])
	}
	if got["stream"] != false {
		t.Errorf("stream должен быть false")
	}
	if _, ok := got["temperature"]; !ok {
		t.Errorf("temperature должна отправляться всегда, даже нулевая")
	}
	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Errorf("thinking: %v", got["thinking"])
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %v", got["tools"])
	}
	if _, ok := got["max_tokens"]; ok {
		t.Errorf("max_tokens не должен отправляться, если не задан")
	}

	if !resp.HasToolCalls() {
		t.Fatalf("ожидался вызов инструмента")
	}
	call := resp.Message.ToolCalls[0]
	if call.ID != "c1" || call.Function.Name != "search" || !strings.Contains(call.Function.Arguments, "рысь") {
		t.Errorf("вызов: %+v", call)
	}
	if resp.Message.Role != RoleAssistant {
		t.Errorf("роль ответа: %q", resp.Message.Role)
	}
	if resp.FinishReason != FinishToolCalls {
		t.Errorf("finish_reason: %q", resp.FinishReason)
	}
	if resp.Usage.Prompt != 100 || resp.Usage.CacheHit != 60 || resp.Usage.CacheMiss != 40 {
		t.Errorf("usage: %+v", resp.Usage)
	}
}

func TestChatThinkingEnabled(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	c := NewClient("k", srv.URL)
	c.Thinking = true
	if _, err := c.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}}}); err != nil {
		t.Fatal(err)
	}
	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Errorf("thinking: %v", got["thinking"])
	}
}

func TestChatAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Authentication Fails","type":"authentication_error"}}`))
	}))
	defer srv.Close()

	_, err := NewClient("bad", srv.URL).Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}}})
	if err == nil || !strings.Contains(err.Error(), "Authentication Fails") {
		t.Fatalf("ожидалась ошибка API, получено: %v", err)
	}
}

func TestChatEmptyAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"   "},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	_, err := NewClient("k", srv.URL).Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "u"}}})
	if err == nil || !strings.Contains(err.Error(), "пустой ответ") {
		t.Fatalf("ожидалась ошибка пустого ответа, получено: %v", err)
	}
}

func TestChatNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[],"usage":{}}`))
	}))
	defer srv.Close()

	_, err := NewClient("k", srv.URL).Chat(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
}

func TestNewToolDefDefaultsParameters(t *testing.T) {
	def := NewToolDef("x", "y", nil)
	if def.Type != "function" || string(def.Function.Parameters) == "" {
		t.Errorf("описание: %+v", def)
	}
}

func TestUsageAdd(t *testing.T) {
	sum := Usage{Prompt: 1, Completion: 2, Total: 3, CacheHit: 1}.Add(Usage{Prompt: 10, Completion: 20, Total: 30, CacheMiss: 9})
	if sum != (Usage{Prompt: 11, Completion: 22, Total: 33, CacheHit: 1, CacheMiss: 9}) {
		t.Errorf("сумма: %+v", sum)
	}
}

func TestPriceOf(t *testing.T) {
	offPeak := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) // суббота
	peak := time.Date(2026, 9, 7, 2, 0, 0, 0, time.UTC)     // понедельник, 02:00 UTC

	u := Usage{Prompt: 1_000_000, Completion: 1_000_000, CacheHit: 500_000, CacheMiss: 500_000}
	c := PriceOf("deepseek-v4-flash", u, offPeak)
	if !c.Known || c.Tariff != TariffOffPeak {
		t.Fatalf("тариф: %+v", c)
	}
	want := 0.5*0.007 + 0.5*0.22 + 0.66
	if diff := c.USD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("стоимость %.6f, ожидалось %.6f", c.USD, want)
	}

	p := PriceOf("deepseek-v4-flash", u, peak)
	if p.Tariff != TariffPeak || p.USD <= c.USD {
		t.Errorf("пиковый тариф должен быть дороже: %+v против %+v", p, c)
	}

	// Без разбивки на кэш весь запрос считается по полной цене.
	noCache := PriceOf("deepseek-v4-flash", Usage{Prompt: 1_000_000}, offPeak)
	if diff := noCache.USD - 0.22; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("без разбивки: %.6f", noCache.USD)
	}

	if PriceOf("unknown-model", u, offPeak).Known {
		t.Errorf("у незнакомой модели стоимость не должна быть известна")
	}
}

func TestCostAdd(t *testing.T) {
	a := Cost{USD: 0.1, Tariff: TariffOffPeak, Known: true}
	b := Cost{USD: 0.2, Tariff: TariffPeak, Known: true}
	if sum := a.Add(b); !sum.Known || sum.Tariff != "смешанный" || sum.USD < 0.3-1e-9 {
		t.Errorf("сумма тарифов: %+v", sum)
	}
	if sum := (Cost{}).Add(a); sum != a {
		t.Errorf("ноль плюс a должен быть a: %+v", sum)
	}
	if sum := a.Add(Cost{Tariff: "x"}); sum.Known {
		t.Errorf("неизвестная стоимость заражает сумму: %+v", sum)
	}
}
