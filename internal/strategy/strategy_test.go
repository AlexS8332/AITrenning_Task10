package strategy

import (
	"strings"
	"testing"

	"github.com/AlexS8332/AITrenning_Task10/internal/facts"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
)

// dialog — история из n ходов: вопрос и ответ на каждый.
func dialog(n int) []llm.Message {
	var ms []llm.Message
	for i := 1; i <= n; i++ {
		ms = append(ms,
			llm.Message{Role: llm.RoleUser, Content: "вопрос"},
			llm.Message{Role: llm.RoleAssistant, Content: "ответ"})
	}
	return ms
}

func TestModes(t *testing.T) {
	if got := Modes(); len(got) != 3 || got[0] != ModeFull || got[2] != ModeFacts {
		t.Errorf("стратегии: %v", got)
	}
	for _, m := range Modes() {
		if !Valid(m) {
			t.Errorf("%q должна быть известной стратегией", m)
		}
		if Title(m) == m {
			t.Errorf("у стратегии %q нет названия для человека", m)
		}
	}
	if Valid("summary") {
		t.Errorf("конспекта в этом задании нет")
	}
	if Title("что-то") != "что-то" {
		t.Errorf("незнакомая стратегия должна показываться как есть")
	}
}

// Нулевая политика — полная история: она не должна молча начинать резать
// контекст и звать вторую модель.
func TestNormalizedDefaultsToFull(t *testing.T) {
	p := Policy{}.Normalized()
	if p.Mode != ModeFull || p.Keep != DefaultKeep {
		t.Errorf("умолчания: %+v", p)
	}
	if DefaultPolicy().Mode != ModeFacts {
		t.Errorf("умолчание приложения — факты: %+v", DefaultPolicy())
	}
}

// Граница режется по началу хода: ответ инструмента без вызова, который
// его породил, API отвергает.
func TestBoundaryAlignsToTurnStart(t *testing.T) {
	hist := dialog(6) // 12 сообщений, чётные — пользователь

	if got := Boundary(hist, 100); got != 0 {
		t.Errorf("история короче окна — резать нечего: %d", got)
	}
	// Нулевое окно не значит «пустой контекст»: последний ход всё равно
	// уходит целиком, иначе модель получила бы вопрос без своего же
	// предыдущего ответа.
	if got := Boundary(hist, 0); got != 10 {
		t.Errorf("нулевое окно должно оставить последний ход: %d", got)
	}
	// Заказано 5, граница сдвигается вперёд до начала хода: окно выйдет
	// чуть меньше заказанного, зато целым.
	got := Boundary(hist, 5)
	if got != 8 || hist[got].Role != llm.RoleUser {
		t.Errorf("граница %d, роль %q", got, hist[got].Role)
	}

	// Последний ход длиннее окна: много ответов инструментов подряд.
	long := []llm.Message{
		{Role: llm.RoleUser, Content: "первый вопрос"},
		{Role: llm.RoleAssistant, Content: "первый ответ"},
		{Role: llm.RoleUser, Content: "второй вопрос"},
		{Role: llm.RoleAssistant},
		{Role: llm.RoleTool, Content: "1"},
		{Role: llm.RoleTool, Content: "2"},
		{Role: llm.RoleTool, Content: "3"},
		{Role: llm.RoleAssistant, Content: "второй ответ"},
	}
	// Окно в 2 сообщения пришлось бы на середину хода — отступаем назад
	// к его началу.
	if got := Boundary(long, 2); got != 2 || long[got].Content != "второй вопрос" {
		t.Errorf("граница внутри хода: %d", got)
	}
	if got := Boundary(nil, 4); got != 0 {
		t.Errorf("пустая история: %d", got)
	}
}

func TestApply(t *testing.T) {
	hist := dialog(6)
	var card facts.State
	card.Set("цель", "учёт списаний", 1)

	block, window := Apply(card, hist, Policy{Mode: ModeFull, Keep: 4})
	if block != "" || len(window) != len(hist) {
		t.Errorf("полная история: блок %q, окно %d", block, len(window))
	}

	block, window = Apply(card, hist, Policy{Mode: ModeWindow, Keep: 4})
	if block != "" {
		t.Errorf("у окна блока памяти быть не должно: %q", block)
	}
	if len(window) != 4 || window[0].Role != llm.RoleUser {
		t.Errorf("окно: %d сообщений, первое %q", len(window), window[0].Role)
	}

	block, window = Apply(card, hist, Policy{Mode: ModeFacts, Keep: 4})
	if !strings.Contains(block, "цель: учёт списаний") {
		t.Errorf("в блоке нет карточки: %q", block)
	}
	if len(window) != 4 {
		t.Errorf("окно у фактов должно быть тем же: %d", len(window))
	}

	// Пустая карточка не занимает места в запросе: платить не за что.
	block, _ = Apply(facts.State{}, hist, Policy{Mode: ModeFacts, Keep: 4})
	if block != "" {
		t.Errorf("пустая карточка: %q", block)
	}
}

func TestLabelMentionsStrategy(t *testing.T) {
	if got := (Policy{Mode: ModeFull}).Label(); !strings.Contains(got, "целиком") {
		t.Errorf("full: %q", got)
	}
	if got := (Policy{Mode: ModeWindow, Keep: 8}).Label(); !strings.Contains(got, "8 сообщений") {
		t.Errorf("window: %q", got)
	}
	got := DefaultPolicy().Label()
	if !strings.Contains(got, "карточкой фактов") || !strings.Contains(got, "24") {
		t.Errorf("facts: %q", got)
	}
	if got := (Policy{Mode: ModeFacts, Keep: 4, Facts: facts.Policy{Model: "лёгкая"}}).Label(); !strings.Contains(got, "моделью лёгкая") {
		t.Errorf("отдельная модель извлекателя: %q", got)
	}
}

func TestPlural(t *testing.T) {
	cases := map[int]string{
		1: "1 сообщение", 2: "2 сообщения", 4: "4 сообщения", 5: "5 сообщений",
		11: "11 сообщений", 12: "12 сообщений", 21: "21 сообщение", 24: "24 сообщения",
		111: "111 сообщений", 0: "0 сообщений",
	}
	for n, want := range cases {
		if got := Plural(n, "сообщение", "сообщения", "сообщений"); got != want {
			t.Errorf("%d: %q, ждали %q", n, got, want)
		}
	}
}
