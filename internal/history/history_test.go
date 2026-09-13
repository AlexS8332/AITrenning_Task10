package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/facts"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/strategy"
)

func sampleTurn(user, reply string) (Turn, []llm.Message) {
	turn := Turn{
		ID: NewID(), Started: time.Now(), Status: TurnDone, User: user, Reply: reply,
		Totals: Totals{LLMCalls: 2, ToolCalls: 1, Usage: llm.Usage{Prompt: 100, Completion: 20, Total: 120}, Seconds: 1.5},
		Events: []agent.Event{{Seq: 1, Kind: agent.EventAgentStart, Title: "старт"}},
	}
	added := []llm.Message{
		{Role: llm.RoleUser, Content: user},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "search_wikipedia", Arguments: `{"query":"рысь"}`}}}},
		{Role: llm.RoleTool, ToolCallID: "call_1", Content: `{"results":[{"title":"Рысь"}]}`},
		{Role: llm.RoleAssistant, Content: reply},
	}
	return turn, added
}

// say — короткий ход «вопрос — ответ» для опытов с ветками.
func say(c *Conversation, t *testing.T, user, reply string) {
	t.Helper()
	turn := Turn{ID: NewID(), Started: time.Now(), Status: TurnDone, User: user, Reply: reply}
	added := []llm.Message{
		{Role: llm.RoleUser, Content: user},
		{Role: llm.RoleAssistant, Content: reply},
	}
	if err := c.Append(c.Active, turn, added, c.Facts()); err != nil {
		t.Fatal(err)
	}
}

func TestConversationAppendAndTotals(t *testing.T) {
	c := New("analyst", "m", strategy.ModeFacts)
	if c.ID == "" || !validID(c.ID) || c.Title != "" {
		t.Fatalf("новый диалог: %+v", c)
	}
	if len(c.Branches) != 1 || c.Active != c.Branches[0].ID || c.Branches[0].Name != RootBranchName {
		t.Fatalf("у нового диалога должна быть одна ветка: %+v", c.Branches)
	}

	turn, added := sampleTurn("Привет! Меня зовут Марина, собираем ТЗ", "Записал. Какая цель?")
	if err := c.Append(c.Active, turn, added, facts.State{}); err != nil {
		t.Fatal(err)
	}
	turn2, added2 := sampleTurn("учёт списаний", "Понял.")
	if err := c.Append(c.Active, turn2, added2, facts.State{}); err != nil {
		t.Fatal(err)
	}

	if len(c.Messages()) != 8 || len(c.Turns()) != 2 || c.Turns()[0].Messages != 4 {
		t.Errorf("история: сообщений %d, ходов %d, у первого хода %d",
			len(c.Messages()), len(c.Turns()), c.Turns()[0].Messages)
	}
	if c.Turns()[0].Branch != c.Active {
		t.Errorf("ход должен помнить свою ветку: %q", c.Turns()[0].Branch)
	}
	if c.Title != "Привет! Меня зовут Марина, собираем ТЗ" {
		t.Errorf("название: %q", c.Title)
	}
	if tot := c.Totals(); tot.LLMCalls != 4 || tot.ToolCalls != 2 || tot.Usage.Total != 240 || tot.Seconds != 3 {
		t.Errorf("итоги: %+v", tot)
	}
	if c.Runes() == 0 || c.Runes() != Runes(c.Messages()) {
		t.Errorf("размер истории: %d", c.Runes())
	}
	if err := c.Append("нет такой ветки", turn, added, facts.State{}); err == nil {
		t.Errorf("запись в несуществующую ветку должна быть ошибкой")
	}

	clone := c.Clone()
	clone.Branches[0].Messages[0].Content = "подмена"
	clone.Branches[0].Turns[0].Events[0].Title = "подмена"
	if c.Messages()[0].Content == "подмена" || c.Turns()[0].Events[0].Title == "подмена" {
		t.Errorf("Clone должен копировать глубоко")
	}
}

// Ветка видит путь родителя до места ветвления и дальше только своё.
// Это и есть управление контекстом ветвлением: соседняя ветка в запрос
// не попадает вообще.
func TestForkInheritsPrefixAndStaysIndependent(t *testing.T) {
	c := New("analyst", "m", strategy.ModeFull)
	say(c, t, "собираем ТЗ", "хорошо")
	say(c, t, "срок 1 марта", "записал")

	cp, err := c.Mark(c.Active, "перед выбором")
	if err != nil {
		t.Fatal(err)
	}
	if cp.At != 4 || cp.Turn != 2 {
		t.Fatalf("точка сохранения: %+v", cp)
	}
	// Точка помнит ход, после которого стоит: по номеру хода её место в
	// ленте «весь диалог» не найти — там ходы идут не по одной ветке.
	if cp.After != c.Turns()[1].ID {
		t.Errorf("точка должна помнить ход, после которого стоит: %q", cp.After)
	}
	fresh := New("analyst", "m", strategy.ModeFull)
	empty, err := fresh.Mark(fresh.Active, "")
	if err != nil {
		t.Fatal(err)
	}
	if empty.After != "" || empty.Turn != 0 {
		t.Errorf("у точки в начале разговора хода перед ней нет: %+v", empty)
	}

	kotlin, err := c.Fork(cp.ID, "Kotlin")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Switch(kotlin.ID); err != nil {
		t.Fatal(err)
	}
	say(c, t, "делаем нативное приложение", "ок, Kotlin")

	pwa, err := c.Fork(cp.ID, "PWA")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Switch(pwa.ID); err != nil {
		t.Fatal(err)
	}
	say(c, t, "делаем PWA", "ок, PWA")

	// Путь каждой ветки: общее начало плюс своё.
	for _, tc := range []struct {
		branch *Branch
		own    string
		alien  string
	}{
		{kotlin, "делаем нативное приложение", "делаем PWA"},
		{pwa, "делаем PWA", "делаем нативное приложение"},
	} {
		path := c.Path(tc.branch.ID)
		if len(path) != 6 {
			t.Errorf("%s: в пути %d сообщений, ждали 6", tc.branch.Name, len(path))
		}
		if path[0].Content != "собираем ТЗ" || path[2].Content != "срок 1 марта" {
			t.Errorf("%s: общее начало потерялось: %+v", tc.branch.Name, path[:3])
		}
		var texts []string
		for _, m := range path {
			texts = append(texts, m.Content)
		}
		joined := strings.Join(texts, "|")
		if !strings.Contains(joined, tc.own) {
			t.Errorf("%s: своего хода нет в пути", tc.branch.Name)
		}
		if strings.Contains(joined, tc.alien) {
			t.Errorf("%s: в путь попал ход соседней ветки", tc.branch.Name)
		}
		if turns := c.PathTurns(tc.branch.ID); len(turns) != 3 {
			t.Errorf("%s: ходов в пути %d, ждали 3", tc.branch.Name, len(turns))
		}
	}

	// Одной лентой — каждое сообщение ровно один раз, общее начало не
	// задваивается.
	linear := c.Linear()
	if len(linear) != 8 {
		t.Errorf("одной лентой %d сообщений, ждали 8", len(linear))
	}
	count := 0
	for _, m := range linear {
		if m.Content == "собираем ТЗ" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("общее начало в ленте повторяется %d раз", count)
	}

	// Ходы одной лентой идут в том же порядке, что и сообщения: сначала
	// общая часть, потом один вариант, потом другой. Это не хронология, а
	// «диалог, в котором ветвиться было нельзя», — с ним и сравнивают.
	turns := c.LinearTurns()
	if len(turns) != 4 {
		t.Fatalf("ходов одной лентой %d, ждали 4", len(turns))
	}
	order := []string{"собираем ТЗ", "срок 1 марта", "делаем нативное приложение", "делаем PWA"}
	for i, want := range order {
		if turns[i].User != want {
			t.Errorf("ход %d одной лентой: %q, ждали %q", i+1, turns[i].User, want)
		}
	}
	if turns[2].Branch != kotlin.ID || turns[3].Branch != pwa.ID {
		t.Errorf("ход в ленте должен помнить свою ветку: %q, %q", turns[2].Branch, turns[3].Branch)
	}

	// Переключение меняет то, что уйдёт модели на следующем ходе.
	if err := c.Switch(kotlin.ID); err != nil {
		t.Fatal(err)
	}
	if got := c.Messages(); got[len(got)-1].Content != "ок, Kotlin" {
		t.Errorf("после переключения путь должен быть Kotlin-веткой: %q", got[len(got)-1].Content)
	}
	if err := c.Switch("нет такой"); err == nil {
		t.Errorf("переключение в несуществующую ветку должно быть ошибкой")
	}
	if _, err := c.Fork("нет такой точки", "х"); err == nil {
		t.Errorf("ветвление от несуществующей точки должно быть ошибкой")
	}
}

// Точка сохранения хранит снимок карточки фактов: ветка начинается с той
// памятью, какая была в месте ветвления, а не с нынешней.
func TestCheckpointSnapshotsFacts(t *testing.T) {
	c := New("analyst", "m", strategy.ModeFacts)
	root := c.Current()
	root.Facts.Set("цель", "учёт списаний", 1)
	say(c, t, "цель — учёт списаний", "записал")

	cp, err := c.Mark(c.Active, "после цели")
	if err != nil {
		t.Fatal(err)
	}
	// Дальше в корневой ветке появляется ещё один факт — ветка от старой
	// точки о нём знать не должна.
	root.Facts.Set("бюджет", "900 тысяч", 2)

	b, err := c.Fork(cp.ID, "другой бюджет")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Facts.Get("цель"); !ok {
		t.Errorf("ветка должна унаследовать факты точки: %+v", b.Facts.Entries)
	}
	if _, ok := b.Facts.Get("бюджет"); ok {
		t.Errorf("в ветку просочился факт, появившийся после точки")
	}

	// Правка в ветке не должна доходить до родителя.
	b.Facts.Set("бюджет", "1 200 000", 3)
	if v, _ := root.Facts.Get("бюджет"); v != "900 тысяч" {
		t.Errorf("карточка родителя изменилась из ветки: %q", v)
	}
}

// Расход на карточку наследуется вместе с ней, поэтому суммировать ветки
// подряд нельзя: общая часть оплачена один раз.
func TestFactsTotalsCountSharedPartOnce(t *testing.T) {
	c := New("analyst", "m", strategy.ModeFacts)
	root := c.Current()
	root.Facts.Calls = 2
	root.Facts.Usage = llm.Usage{Prompt: 100, Completion: 10, Total: 110}
	root.Facts.Seconds = 2

	cp, err := c.Mark(c.Active, "точка")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Fork(cp.ID, "ветка")
	if err != nil {
		t.Fatal(err)
	}
	// Ветка сделала ещё один запрос сверх унаследованного расхода.
	b.Facts.Calls = 3
	b.Facts.Usage = llm.Usage{Prompt: 160, Completion: 15, Total: 175}
	b.Facts.Seconds = 3

	usage, _, seconds := c.FactsTotals()
	if usage.Prompt != 160 || usage.Total != 175 {
		t.Errorf("расход карточки: %+v, ждали 100 + прирост 60", usage)
	}
	if seconds != 3 {
		t.Errorf("время карточки: %v, ждали 3", seconds)
	}
}

func TestMakeTitle(t *testing.T) {
	if got := MakeTitle("  два   слова \n и ещё "); got != "два слова и ещё" {
		t.Errorf("пробелы: %q", got)
	}
	long := strings.Repeat("слово ", 20)
	got := MakeTitle(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > titleRunes+1 || strings.HasSuffix(got, " …") {
		t.Errorf("обрезка по словам: %q", got)
	}
	if got := MakeTitle(strings.Repeat("x", 80)); len([]rune(got)) != titleRunes+1 {
		t.Errorf("обрезка без пробелов: %q", got)
	}
}

func TestCompactShortensOnlyOldToolReplies(t *testing.T) {
	long := strings.Repeat("я", 500)
	ms := []llm.Message{
		{Role: llm.RoleUser, Content: long},
		{Role: llm.RoleTool, ToolCallID: "1", Content: long},
		{Role: llm.RoleTool, ToolCallID: "2", Content: "короткий"},
		{Role: llm.RoleAssistant, Content: long},
	}
	out := Compact(ms, 100)
	if out[0].Content != long || out[3].Content != long {
		t.Errorf("сообщения пользователя и модели трогать нельзя")
	}
	if !strings.HasPrefix(out[1].Content, strings.Repeat("я", 100)+" …[сокращено") || out[1].ToolCallID != "1" {
		t.Errorf("длинный ответ инструмента не сокращён: %q", out[1].Content[:120])
	}
	if out[2].Content != "короткий" {
		t.Errorf("короткий ответ инструмента изменён: %q", out[2].Content)
	}
	if ms[1].Content != long {
		t.Errorf("Compact изменил исходный срез")
	}
	if got := Compact(ms, 0); &got[0] != &ms[0] {
		t.Errorf("keep = 0 должен вернуть историю как есть")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	store := NewStore(dir)
	if store.Dir() != dir && !filepath.IsAbs(store.Dir()) {
		t.Errorf("каталог: %q", store.Dir())
	}

	// Пустого каталога ещё нет: это не ошибка, диалогов просто нет.
	if convs, errs := store.Load(); len(convs) != 0 || len(errs) != 0 {
		t.Fatalf("загрузка из отсутствующего каталога: %v %v", convs, errs)
	}

	first := New("analyst", "m", strategy.ModeFacts)
	turn, added := sampleTurn("собираем ТЗ", "Записал. Какая цель?")
	card := facts.State{Version: 1}
	card.Set("цель", "учёт списаний", 1)
	if err := first.Append(first.Active, turn, added, card); err != nil {
		t.Fatal(err)
	}
	// Ветка с точкой: они тоже должны пережить запись и чтение.
	cp, err := first.Mark(first.Active, "перед выбором")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := first.Fork(cp.ID, "PWA")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Switch(branch.ID); err != nil {
		t.Fatal(err)
	}
	say(first, t, "делаем PWA", "ок")
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	second := New("chat", "m", strategy.ModeFull)
	second.Updated = first.Updated.Add(time.Minute)
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}

	// Битый файл, чужой файл и файл с чужим id внутри загрузку не роняют.
	os.WriteFile(filepath.Join(dir, "0123456789abcdef.json"), []byte("{не json"), 0o644)
	os.WriteFile(filepath.Join(dir, "заметка.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "fedcba9876543210.json"), []byte(`{"id":"0000000000000000"}`), 0o644)
	if _, err := os.Stat(store.Path(first.ID) + ".tmp"); err == nil {
		t.Errorf("временный файл не должен оставаться после записи")
	}

	convs, errs := store.Load()
	if len(errs) != 2 {
		t.Errorf("ожидались две ошибки чтения, получено: %v", errs)
	}
	if len(convs) != 2 || convs[0].ID != second.ID || convs[1].ID != first.ID {
		t.Fatalf("порядок или состав диалогов: %+v", convs)
	}
	got := convs[1]
	if len(got.Branches) != 2 || got.Active != branch.ID {
		t.Fatalf("дерево не пережило запись: %+v", got.Branches)
	}
	if len(got.Checkpoints) != 1 || got.Checkpoints[0].At != 4 {
		t.Errorf("точки не пережили запись: %+v", got.Checkpoints)
	}
	// Ход в корневой ветке остался на месте, а путь ветки его наследует.
	root := got.Find(got.Branches[0].ID)
	if len(root.Messages) != 4 || root.Messages[1].ToolCalls[0].ID != "call_1" || root.Messages[2].ToolCallID != "call_1" {
		t.Errorf("вызовы инструментов не пережили запись: %+v", root.Messages)
	}
	if len(got.Messages()) != 6 {
		t.Errorf("путь текущей ветки: %d сообщений", len(got.Messages()))
	}
	if v, ok := got.Find(root.ID).Facts.Get("цель"); !ok || v != "учёт списаний" {
		t.Errorf("карточка фактов не пережила запись: %q", v)
	}
	if len(root.Turns) != 1 || root.Turns[0].Reply != "Записал. Какая цель?" || len(root.Turns[0].Events) != 1 {
		t.Errorf("ходы не пережили запись: %+v", root.Turns)
	}
	if got.Title != first.Title || got.AgentKey != "analyst" || !got.Created.Equal(first.Created) {
		t.Errorf("метаданные: %+v", got)
	}
	if len(convs[0].Messages()) != 0 || convs[0].Branches[0].Messages == nil || convs[0].Branches[0].Turns == nil {
		t.Errorf("пустой диалог должен читаться с пустыми, а не nil срезами")
	}

	raw, err := store.Raw(first.ID)
	if err != nil || !strings.Contains(string(raw), `"tool_call_id": "call_1"`) {
		t.Errorf("сырой файл: %v %s", err, raw)
	}
	if _, err := store.Raw("../etc/passwd"); err == nil {
		t.Errorf("подозрительный идентификатор должен отвергаться")
	}

	if err := store.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(first.ID); err != nil {
		t.Errorf("повторное удаление не должно быть ошибкой: %v", err)
	}
	if _, err := os.Stat(store.Path(first.ID)); !os.IsNotExist(err) {
		t.Errorf("файл не удалён")
	}
	if err := store.Save(&Conversation{ID: "bad id"}); err == nil {
		t.Errorf("диалог с некорректным id не должен записываться")
	}
}

// Файл без веток читать нечем: такой диалог нельзя ни показать, ни
// продолжить, и он должен попасть в список проблем, а не уронить загрузку.
func TestLoadRejectsConversationWithoutBranches(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	os.WriteFile(filepath.Join(dir, "abcdef0123456789.json"),
		[]byte(`{"id":"abcdef0123456789","branches":[]}`), 0o644)
	convs, errs := store.Load()
	if len(convs) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "ветки") {
		t.Errorf("диалог без веток: %v %v", convs, errs)
	}
}

// Active может указывать в никуда — файл диалога правят руками. Тогда
// текущей становится первая ветка, а не пустота.
func TestLoadRepairsActiveBranch(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	c := New("analyst", "m", strategy.ModeFull)
	say(c, t, "привет", "здравствуйте")
	c.Active = "0000000000000000"
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	convs, errs := store.Load()
	if len(convs) != 1 || len(errs) != 0 {
		t.Fatalf("загрузка: %v %v", convs, errs)
	}
	if convs[0].Active != convs[0].Branches[0].ID || len(convs[0].Messages()) != 2 {
		t.Errorf("текущая ветка не восстановлена: %+v", convs[0].Active)
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{NewID(), "0123456789abcdef"} {
		if !validID(id) {
			t.Errorf("%q должен быть допустим", id)
		}
	}
	for _, id := range []string{"", "short", "../x", "0123456789ABCDEF", strings.Repeat("a", 40)} {
		if validID(id) {
			t.Errorf("%q не должен быть допустим", id)
		}
	}
}

// Показывать путь относительно рабочего каталога, а вне его — как есть:
// «history» вместо длинного абсолютного пути в журнале и в интерфейсе.
func TestDisplay(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore("history")
	if got := store.DisplayDir(); got != "history" {
		t.Errorf("каталог внутри рабочего: %q, ждали %q", got, "history")
	}
	want := filepath.Join("history", "abcdef0123456789.json")
	if got := store.DisplayPath("abcdef0123456789"); got != want {
		t.Errorf("файл внутри рабочего: %q, ждали %q", got, want)
	}

	// Каталог по соседству с рабочим: относительный путь начинался бы с
	// «..» и был бы длиннее и непонятнее абсолютного.
	outside := filepath.Join(filepath.Dir(cwd), "чужая-история")
	if got := Display(outside); got != outside {
		t.Errorf("каталог вне рабочего: %q, ждали %q", got, outside)
	}

	// Сам рабочий каталог — «.»: путь есть, а показывать нечего.
	if got := Display(cwd); got != "." {
		t.Errorf("рабочий каталог: %q, ждали %q", got, ".")
	}
}

// Копия пустого диалога — это пустой список сообщений, а не null. Такой
// диалог отдаётся интерфейсу сразу после создания, пока первый ход ещё
// идёт: null в JSON ронял ленту на messages.length.
func TestCloneEmptyKeepsSlices(t *testing.T) {
	clone := New("analyst", "m", strategy.ModeFacts).Clone()
	if clone.Branches[0].Messages == nil {
		t.Errorf("сообщения пустой ветки: nil")
	}
	if clone.Branches[0].Turns == nil {
		t.Errorf("ходы пустой ветки: nil")
	}

	data, err := json.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"messages":[]`) || strings.Contains(string(data), `"messages":null`) {
		t.Errorf("в JSON пустого диалога должен быть пустой список сообщений: %s", data)
	}
}
