package facts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm/llmtest"
)

func TestSetReplacesByKeyAndKeepsOrder(t *testing.T) {
	var s State
	s.Set("цель", "учёт списаний", 1)
	s.Set("срок", "1 марта", 2)
	s.Set("бюджет", "900 тысяч", 3)

	// Тот же ключ в другом регистре и с пробелами — та же запись: иначе
	// карточка раздваивается и начинает противоречить сама себе.
	s.Set("  Бюджет ", "1 200 000", 7)
	if len(s.Entries) != 3 {
		t.Fatalf("записей: %d, ждали 3 — %+v", len(s.Entries), s.Entries)
	}
	if v, ok := s.Get("бюджет"); !ok || v != "1 200 000" {
		t.Errorf("значение по ключу: %q %v", v, ok)
	}
	if s.Entries[2].Since != 3 || s.Entries[2].Turn != 7 {
		t.Errorf("правка не должна менять ход появления: %+v", s.Entries[2])
	}
	// Порядок — порядок появления: карточка читается как летопись решений.
	if s.Entries[0].Key != "цель" || s.Entries[1].Key != "срок" || s.Entries[2].Key != "бюджет" {
		t.Errorf("порядок записей сбился: %+v", s.Entries)
	}

	if !s.Delete("СРОК") || len(s.Entries) != 2 {
		t.Errorf("удаление по ключу без учёта регистра: %+v", s.Entries)
	}
	if s.Delete("чего не было") {
		t.Errorf("удаление несуществующего ключа не должно сообщать об успехе")
	}
	s.Set("пусто", "  ", 8)
	s.Set("  ", "значение", 8)
	if len(s.Entries) != 2 {
		t.Errorf("пустые ключ или значение записываться не должны: %+v", s.Entries)
	}
}

// Карточка форкается вместе с веткой: правка в одной ветке не должна
// доходить до другой.
func TestCloneIsDeep(t *testing.T) {
	var s State
	s.Set("цель", "учёт списаний", 1)
	other := s.Clone()
	other.Set("цель", "другое", 2)
	if v, _ := s.Get("цель"); v != "учёт списаний" {
		t.Errorf("копия правит оригинал: %q", v)
	}
}

func TestPromptExplainsCardAndListsFacts(t *testing.T) {
	var s State
	if s.Prompt() != "" {
		t.Errorf("пустая карточка не должна занимать место в запросе")
	}
	s.Set("цель", "учёт списаний", 1)
	p := s.Prompt()
	if !strings.Contains(p, "цель: учёт списаний") {
		t.Errorf("в блоке нет самих фактов: %q", p)
	}
	// Без пояснения модель принимает карточку за реплику пользователя и
	// начинает её обсуждать.
	if !strings.Contains(p, "карточка фактов") || !strings.Contains(p, "не упоминай") {
		t.Errorf("в блоке нет пояснения, чем он является: %q", p)
	}
	if s.Runes() != len([]rune("цель"))+len([]rune("учёт списаний")) {
		t.Errorf("размер карточки: %d", s.Runes())
	}
}

func TestParsePatch(t *testing.T) {
	cases := []struct {
		name string
		in   string
		sets int
		dels int
		bad  bool
	}{
		{name: "как есть", in: `{"set":[{"key":"цель","value":"x"}],"delete":["срок"]}`, sets: 1, dels: 1},
		{name: "в ограде", in: "```json\n{\"set\":[],\"delete\":[\"цель\"]}\n```", dels: 1},
		{name: "с пояснением", in: "Вот правки:\n{\"set\":[{\"key\":\"a\",\"value\":\"b\"}]}\nГотово.", sets: 1},
		{name: "пусто", in: "   ", bad: true},
		{name: "не json", in: "извините, не понял задачу", bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePatch(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("ждали ошибку, получили %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Set) != tc.sets || len(p.Delete) != tc.dels {
				t.Errorf("разобрано: %+v", p)
			}
		})
	}
}

func extractor(fn func(llm.Request) (llm.Response, error), p Policy) (Extractor, *llmtest.Fake) {
	fake := &llmtest.Fake{Fn: fn}
	return Extractor{LLM: fake, Model: "m", Policy: p}, fake
}

func TestExtractorAppliesPatch(t *testing.T) {
	e, fake := extractor(func(req llm.Request) (llm.Response, error) {
		// Извлекателю нужен контекст: текущая карточка, хвост диалога и
		// новая реплика.
		body := req.Messages[1].Content
		for _, want := range []string{"цель: учёт списаний", "Пользователь: сколько точек", "бюджет 1 200 000"} {
			if !strings.Contains(body, want) {
				t.Errorf("в запросе извлекателя нет %q:\n%s", want, body)
			}
		}
		return llmtest.Text(`{"set":[{"key":"бюджет","value":"1 200 000 рублей"}],"delete":["цель"]}`), nil
	}, DefaultPolicy())

	var s State
	s.Set("цель", "учёт списаний", 1)
	hist := []llm.Message{
		{Role: llm.RoleUser, Content: "сколько точек — 14"},
		{Role: llm.RoleAssistant, Content: "записал"},
	}
	upd, err := e.Run(context.Background(), s, hist, "бюджет 1 200 000", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !upd.Changed || upd.State.Version != 1 {
		t.Fatalf("карточка не изменилась: %+v", upd)
	}
	if v, ok := upd.State.Get("бюджет"); !ok || v != "1 200 000 рублей" {
		t.Errorf("новый факт: %q %v", v, ok)
	}
	if _, ok := upd.State.Get("цель"); ok {
		t.Errorf("отменённый факт должен уйти из карточки")
	}
	if len(upd.Set) != 1 || len(upd.Deleted) != 1 {
		t.Errorf("отчёт о правках: %+v", upd)
	}
	if upd.State.Calls != 1 || upd.State.Usage.Total == 0 {
		t.Errorf("расход извлекателя не посчитан: %+v", upd.State)
	}
	if fake.Requests[0].Temperature != 0 {
		t.Errorf("извлекать факты надо при нулевой температуре")
	}
	// Исходная карточка не тронута: её держит ветка диалога.
	if _, ok := s.Get("цель"); !ok {
		t.Errorf("Run изменил исходную карточку")
	}
}

// Ход без новых фактов — обычное дело, и версию он поднимать не должен:
// иначе кэш префикса ломался бы впустую на каждом ходе.
func TestUnchangedPatchKeepsVersionButCountsCall(t *testing.T) {
	e, _ := extractor(func(llm.Request) (llm.Response, error) {
		return llmtest.Text(`{"set":[],"delete":[]}`), nil
	}, DefaultPolicy())

	var s State
	s.Set("цель", "учёт списаний", 1)
	upd, err := e.Run(context.Background(), s, nil, "спасибо, понятно", 2)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Changed || upd.State.Version != 0 {
		t.Errorf("версия не должна расти без правок: %+v", upd)
	}
	if upd.State.Calls != 1 {
		t.Errorf("запрос всё равно был и должен считаться: %+v", upd.State)
	}

	// Повтор того же значения по тому же ключу — тоже не правка.
	upd2, err := extractorSet(t, `{"set":[{"key":"цель","value":"учёт списаний"}]}`).Run(
		context.Background(), s, nil, "цель прежняя", 3)
	if err != nil {
		t.Fatal(err)
	}
	if upd2.Changed {
		t.Errorf("запись того же значения правкой не считается: %+v", upd2)
	}
}

func extractorSet(t *testing.T, answer string) Extractor {
	t.Helper()
	e, _ := extractor(func(llm.Request) (llm.Response, error) { return llmtest.Text(answer), nil }, DefaultPolicy())
	return e
}

func TestExtractorClipsLongValues(t *testing.T) {
	long := strings.Repeat("очень длинное значение ", 40)
	e := extractorSet(t, `{"set":[{"key":"заметка","value":"`+long+`"}]}`)
	upd, err := e.Run(context.Background(), State{}, nil, "запиши", 1)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := upd.State.Get("заметка")
	if len([]rune(v)) > DefaultMaxValueRunes+1 {
		t.Errorf("значение не обрезано: %d символов", len([]rune(v)))
	}
	if !strings.HasSuffix(v, "…") {
		t.Errorf("обрезку надо показывать: %q", v[len(v)-20:])
	}
}

// Потолок карточки — осознанная потеря: вытесняется самый давно не
// обновлявшийся факт, и об этом сообщают наружу.
func TestTrimDropsLeastRecentlyUpdated(t *testing.T) {
	e, _ := extractor(func(llm.Request) (llm.Response, error) {
		return llmtest.Text(`{"set":[{"key":"новый","value":"важное"}]}`), nil
	}, Policy{MaxFacts: 3})

	var s State
	s.Set("древний", "x", 1)
	s.Set("средний", "y", 5)
	s.Set("свежий", "z", 9)

	upd, err := e.Run(context.Background(), s, nil, "ещё факт", 10)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Dropped != 1 || len(upd.State.Entries) != 3 {
		t.Fatalf("потолок не сработал: dropped %d, записей %d", upd.Dropped, len(upd.State.Entries))
	}
	if _, ok := upd.State.Get("древний"); ok {
		t.Errorf("вытеснить надо самый давний: %+v", upd.State.Entries)
	}
	if _, ok := upd.State.Get("новый"); !ok {
		t.Errorf("новый факт не записан: %+v", upd.State.Entries)
	}

	// Переполненная карточка просит модель удалить лишнее сама.
	full := State{}
	for i, key := range []string{"a", "b", "c"} {
		full.Set(key, "v", i+1)
	}
	body := extractRequest(full, nil, "ещё", Policy{MaxFacts: 3})
	if !strings.Contains(body, "потолке 3") {
		t.Errorf("извлекателю не сказано о потолке: %s", body)
	}
}

func TestExtractorErrorsAreDistinguished(t *testing.T) {
	// Модель не ответила: запрос не оплачен, карточка не тронута.
	e, _ := extractor(func(llm.Request) (llm.Response, error) {
		return llm.Response{}, errors.New("сеть недоступна")
	}, DefaultPolicy())
	upd, err := e.Run(context.Background(), State{}, nil, "q", 1)
	if err == nil {
		t.Fatal("ошибка модели должна доходить до вызывающего")
	}
	if upd.State.Calls != 0 {
		t.Errorf("неотвеченный запрос не должен попадать в счёт: %+v", upd.State)
	}

	// Модель ответила мусором: запрос оплачен, и это должно быть видно.
	e2 := extractorSet(t, "не понял задачу")
	upd2, err := e2.Run(context.Background(), State{}, nil, "q", 1)
	if err == nil {
		t.Fatal("неразобранный ответ должен доходить до вызывающего")
	}
	if upd2.State.Calls != 1 || upd2.State.Usage.Total == 0 {
		t.Errorf("оплаченный запрос должен попасть в счёт: %+v", upd2.State)
	}

	// Клиента нет вовсе — это ошибка настройки, а не хода.
	var bare Extractor
	if _, err := bare.Run(context.Background(), State{}, nil, "q", 1); err == nil {
		t.Error("без клиента модели извлечение невозможно")
	}
}

func TestRenderShortensToolReplies(t *testing.T) {
	long := strings.Repeat("текст ", 500)
	out := Render([]llm.Message{
		{Role: llm.RoleUser, Content: "вопрос"},
		{Role: llm.RoleAssistant, Content: "ответ", ToolCalls: []llm.ToolCall{{
			Function: llm.FunctionCall{Name: "search_wikipedia", Arguments: `{"q":"рысь"}`}}}},
		{Role: llm.RoleTool, Content: long},
	})
	if !strings.Contains(out, "Пользователь: вопрос") || !strings.Contains(out, "Агент: ответ") {
		t.Errorf("роли не переведены: %s", out)
	}
	if !strings.Contains(out, "Агент вызвал search_wikipedia") {
		t.Errorf("вызов инструмента не показан: %s", out)
	}
	if len([]rune(out)) > 2000 {
		t.Errorf("ответ инструмента не сокращён: %d символов", len([]rune(out)))
	}
}

// Хвост диалога режется до нескольких последних сообщений: извлекателю
// нужен контекст реплики, а не весь разговор — иначе карточка стоила бы
// столько же, сколько история, которую она заменяет.
func TestExtractRequestKeepsOnlyRecentMessages(t *testing.T) {
	var hist []llm.Message
	for i := 0; i < 20; i++ {
		hist = append(hist, llm.Message{Role: llm.RoleUser, Content: "сообщение " + string(rune('а'+i))})
	}
	body := extractRequest(State{}, hist, "новое", DefaultPolicy())
	if strings.Contains(body, "сообщение а") {
		t.Errorf("в запрос попал весь диалог:\n%s", body)
	}
	if !strings.Contains(body, "сообщение "+string(rune('а'+19))) {
		t.Errorf("последнее сообщение должно дойти:\n%s", body)
	}
	if !strings.Contains(body, "(пусто)") {
		t.Errorf("о пустой карточке надо сказать прямо:\n%s", body)
	}
}
