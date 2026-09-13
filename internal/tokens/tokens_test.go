package tokens

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
)

func TestTextByCharClass(t *testing.T) {
	// Латиница дешевле кириллицы при той же длине: у модели больше
	// английских слов в словаре, русский рассыпается на куски.
	lat := Default.Text(strings.Repeat("a", 100))
	cyr := Default.Text(strings.Repeat("я", 100))
	if !(lat < cyr) {
		t.Errorf("латиница %.1f должна быть дешевле кириллицы %.1f", lat, cyr)
	}
	if got := Default.Text(""); got != 0 {
		t.Errorf("пустая строка: %.1f", got)
	}
	// Иероглифы — самые дорогие из букв.
	if Default.Text("中") <= Default.Text("a") {
		t.Errorf("иероглиф должен стоить дороже латинской буквы")
	}
	// Пробелы почти бесплатны: они склеиваются со словами.
	if Default.Text(" ") >= Default.Text("a") {
		t.Errorf("пробел должен стоить меньше буквы")
	}
}

func TestMessageCountsToolCalls(t *testing.T) {
	plain := llm.Message{Role: llm.RoleAssistant, Content: "Рысь — хищник"}
	withCall := plain
	withCall.ToolCalls = []llm.ToolCall{{
		ID: "call_1", Type: "function",
		Function: llm.FunctionCall{Name: "read_wikipedia", Arguments: `{"title":"Рысь","section":"Питание"}`},
	}}
	if Default.Message(withCall) <= Default.Message(plain) {
		t.Errorf("аргументы вызова инструмента должны считаться: модель платит и за них")
	}

	// Пустое сообщение всё равно стоит обвязки: роль и разделители.
	if got := Default.Message(llm.Message{Role: llm.RoleUser}); got != Default.PerMessage {
		t.Errorf("обвязка пустого сообщения: %.1f, ждали %.1f", got, Default.PerMessage)
	}
}

func TestOfSplitsRequest(t *testing.T) {
	defs := []llm.ToolDef{llm.NewToolDef("read_wikipedia", "читает раздел статьи", json.RawMessage(`{"type":"object"}`))}
	history := []llm.Message{
		{Role: llm.RoleUser, Content: "Расскажи про рысь"},
		{Role: llm.RoleAssistant, Content: strings.Repeat("Рысь — хищник семейства кошачьих. ", 20)},
	}
	e := Of("Ты справочник по животным.", "", defs, history, "А чем она питается?")

	if e.System == 0 || e.Tools == 0 || e.History == 0 || e.User == 0 {
		t.Fatalf("пустая доля в разбивке: %+v", e)
	}
	// Части должны складываться в целое: интерфейс показывает и то и другое.
	if sum := e.System + e.Tools + e.History + e.User; e.Total < sum {
		t.Errorf("итог %d меньше суммы частей %d", e.Total, sum)
	}
	// История длиннее нового сообщения — на ней и растёт стоимость хода.
	if e.History <= e.User {
		t.Errorf("история %d должна быть больше нового сообщения %d", e.History, e.User)
	}
}

func TestOfMessagesGrowsWithHistory(t *testing.T) {
	defs := []llm.ToolDef{llm.NewToolDef("t", "описание", nil)}
	var ms []llm.Message
	prev := OfMessages(defs, ms)
	for i := 0; i < 5; i++ {
		ms = append(ms, llm.Message{Role: llm.RoleUser, Content: "ещё один ход диалога"})
		got := OfMessages(defs, ms)
		if got <= prev {
			t.Fatalf("после %d-го сообщения оценка не выросла: %d → %d", i+1, prev, got)
		}
		prev = got
	}
}

func TestCompare(t *testing.T) {
	// Оценка завысила на десятую часть.
	if got := Compare(110, 100); math.Abs(got.ErrorPct-10) > 0.001 {
		t.Errorf("ошибка оценки: %.3f, ждали 10", got.ErrorPct)
	}
	// Занизила — ошибка отрицательная.
	if got := Compare(90, 100); math.Abs(got.ErrorPct+10) > 0.001 {
		t.Errorf("ошибка оценки: %.3f, ждали -10", got.ErrorPct)
	}
	// Факта ещё нет: ошибку не считаем, иначе выйдет ложные -100 %.
	if got := Compare(50, 0); got.ErrorPct != 0 || got.Actual != 0 {
		t.Errorf("без факта ошибки быть не должно: %+v", got)
	}
}
