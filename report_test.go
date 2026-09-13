package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

// isExtract — запрос к извлекателю фактов узнаётся по его системному
// промпту: у него другая роль, чем у агента диалога.
func isExtract(req llm.Request) bool {
	return len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "карточку фактов диалога")
}

// growingFake — подставная модель: отвечает длинно и честно сообщает
// расход, посчитанный по длине запроса. Этого хватает, чтобы в отчёте
// росли и токены, и стоимость. Запрос извлекателя она узнаёт по
// системному промпту и отвечает правкой карточки, в которой есть имя и
// город: контрольные вопросы отчёта проверяют именно их.
func growingFake() *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		var runes int
		for _, m := range req.Messages {
			runes += len([]rune(m.Content))
		}
		text := strings.Repeat("Зафиксировал требование. Какой следующий вопрос разбираем? ", 12)
		if isExtract(req) {
			text = `{"set":[{"key":"имя","value":"Марина"},{"key":"город","value":"Казань"}],"delete":[]}`
		}
		resp := llmtest.Text(text)
		prompt := runes / 2
		resp.Usage = llm.Usage{Prompt: prompt, Completion: 120, Total: prompt + 120, CacheHit: prompt / 3}
		return resp, nil
	}}
}

func reportDeps(fake *llmtest.Fake) agents.Deps {
	return agents.Deps{
		Runner:   agent.Runner{LLM: fake, Model: "deepseek-v4-flash"},
		Tools:    tools.NewRegistry(),
		Strategy: strategy.DefaultPolicy(),
	}
}

func TestReportComparesStrategiesAndBranches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.md")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := runReport(ctx, reportDeps(growingFake()), "deepseek-v4-flash", path, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	for _, want := range []string{
		"вся история", "окно", "факты",
		"Контрольные вопросы", "Ответы на контрольные вопросы",
		"Карточка фактов в конце диалога",
		"## Ветвление", "Дерево диалога после опыта", "одной лентой",
		"Что ответили ветки на один и тот же вопрос",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в отчёте нет %q", want)
		}
	}
}

func TestFactsSpendFewerTokensThanFullHistory(t *testing.T) {
	// Суть задания: с карточкой фактов тот же диалог должен обходиться
	// дешевле по входу, чем с полной историей. Если это перестанет быть
	// правдой, стратегия бессмысленна.
	full, err := runScenario(context.Background(), reportDeps(growingFake()),
		scenario{Name: "full", Mode: strategy.ModeFull}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cards, err := runScenario(context.Background(), reportDeps(growingFake()),
		scenario{Name: "facts", Mode: strategy.ModeFacts}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if len(full.rows) != len(dialogQuestions) || len(cards.rows) != len(dialogQuestions) {
		t.Fatalf("ходов: с полной историей %d, с фактами %d", len(full.rows), len(cards.rows))
	}
	if cards.total.Prompt >= full.total.Prompt {
		t.Errorf("факты не сэкономили: %d токенов против %d", cards.total.Prompt, full.total.Prompt)
	}
	if cards.facts.Version == 0 || len(cards.facts.Entries) == 0 {
		t.Errorf("карточка не собралась: %+v", cards.facts)
	}
	// Карточка сама стоит запросов к модели — по одному на ход, — и отчёт
	// обязан их считать.
	if cards.facts.Calls != len(dialogQuestions) || cards.facts.Usage.Total == 0 {
		t.Errorf("расход на карточку не посчитан: %+v", cards.facts)
	}
	// У поздних ходов оценка запроса должна быть заметно ниже, чем оценка
	// того же хода с полной историей.
	last := cards.rows[len(cards.rows)-1]
	if last.Full <= last.Estimate.Total {
		t.Errorf("последний ход: с полной историей ≈%d, с фактами ≈%d", last.Full, last.Estimate.Total)
	}
}

func TestFullHistoryGrowsEveryTurn(t *testing.T) {
	// Точка отсчёта: с полной историей каждый следующий ход дороже
	// предыдущего.
	res, err := runScenario(context.Background(), reportDeps(growingFake()),
		scenario{Name: "full", Mode: strategy.ModeFull}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(res.rows); i++ {
		if res.rows[i].Estimate.Total <= res.rows[i-1].Estimate.Total {
			t.Errorf("ход %d: контекст не вырос (%d → %d)", i+1,
				res.rows[i-1].Estimate.Total, res.rows[i].Estimate.Total)
		}
		if res.rows[i].Cumulative < res.rows[i-1].Cumulative {
			t.Errorf("ход %d: накопленная стоимость уменьшилась", i+1)
		}
	}
	// История растёт ровно на сообщения хода: вопрос и ответ.
	if res.rows[5].HistoryMsg != 10 {
		t.Errorf("сообщений в истории перед шестым ходом: %d", res.rows[5].HistoryMsg)
	}
	if res.facts.Calls != 0 {
		t.Errorf("с полной историей извлекатель зваться не должен: %+v", res.facts)
	}
}

func TestWindowDropsBeginningWithoutFacts(t *testing.T) {
	window, err := runScenario(context.Background(), reportDeps(growingFake()),
		scenario{Name: "window", Mode: strategy.ModeWindow}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	last := window.rows[len(window.rows)-1]
	if last.SentMsg >= last.HistoryMsg {
		t.Errorf("окно ничего не выбросило: истории %d, ушло %d", last.HistoryMsg, last.SentMsg)
	}
	if window.facts.Version != 0 || window.facts.Calls != 0 {
		t.Errorf("в режиме окна карточка собираться не должна: %+v", window.facts)
	}
}

// Опыт с ветвлением: ветки растут из одной точки, не видят сообщений друг
// друга и вместе обходятся дешевле, чем тот же разбор одной лентой.
func TestBranchingKeepsBranchesApart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rep, err := runBranching(ctx, reportDeps(growingFake()), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Forks) != 2 {
		t.Fatalf("веток: %d", len(rep.Forks))
	}
	if len(rep.Base.Rows) != len(branchBase) || rep.Sharing == 0 {
		t.Errorf("общая часть: %d ходов, унаследовано %d сообщений", len(rep.Base.Rows), rep.Sharing)
	}
	// Обе ветки стартуют с одной и той же длины пути: они выросли из
	// одного места.
	if rep.Forks[0].Rows[0].HistoryMsg != rep.Forks[1].Rows[0].HistoryMsg {
		t.Errorf("ветки стартовали с разных мест: %d и %d",
			rep.Forks[0].Rows[0].HistoryMsg, rep.Forks[1].Rows[0].HistoryMsg)
	}
	if rep.Forks[0].Rows[0].HistoryMsg != rep.Sharing {
		t.Errorf("ветка должна наследовать ровно путь точки: %d при %d",
			rep.Forks[0].Rows[0].HistoryMsg, rep.Sharing)
	}
	// Линейный разбор тащит оба варианта в одном контексте и потому
	// дороже — но сравнивать суммы дорожек в лоб нельзя: ходов в них
	// разное число. Честное сравнение поштучное, по счётчику «во сколько
	// обошёлся бы тот же ход без ветвления».
	var withBranch, withoutBranch, comparable int
	for _, r := range rep.Forks {
		for _, row := range r.Rows {
			if row.Linear <= 0 {
				continue
			}
			withBranch += row.Estimate.Total
			withoutBranch += row.Linear
			comparable++
		}
	}
	if comparable == 0 {
		t.Fatal("ни у одного хода ветки не посчитана оценка без ветвления")
	}
	if withBranch >= withoutBranch {
		t.Errorf("ветвление не сэкономило: ≈%d токенов против ≈%d одной лентой", withBranch, withoutBranch)
	}
	// На последнем ходе каждой ветки видно, во что обошёлся бы тот же ход
	// без ветвления.
	last := rep.Forks[1].Rows[len(rep.Forks[1].Rows)-1]
	if last.Linear <= last.Estimate.Total {
		t.Errorf("оценка без ветвления не посчитана: %+v", last)
	}
	if len(rep.Tree) != 3 || !strings.Contains(rep.Tree[1], "от «"+history.RootBranchName+"»") {
		t.Errorf("дерево в отчёте: %+v", rep.Tree)
	}
}

func TestLimitLabel(t *testing.T) {
	if got := limitLabel(0, agent.OverflowFail); !strings.Contains(got, "только API") {
		t.Errorf("без лимита: %q", got)
	}
	if got := limitLabel(4000, agent.OverflowTrim); !strings.Contains(got, "старые ходы") || !strings.Contains(got, "4000") {
		t.Errorf("с лимитом: %q", got)
	}
}

func TestOverflowHistoryReachesTarget(t *testing.T) {
	// Проба имеет смысл, только если набранная история действительно
	// больше заказанного: на первом заходе шаг цикла считался по удвоенному
	// весу длинного сообщения, и набиралась ровно половина.
	for _, target := range []int{10_000, 200_000, 1_050_000} {
		_, est := overflowHistory(target)
		if est.Total < target {
			t.Errorf("для %d набрано только ≈%d токенов", target, est.Total)
		}
	}
}

// Проверки контрольных вопросов умеют требовать и отсутствие слова:
// старая сумма, которую пользователь отменил, в ответе появляться не
// должна.
func TestCheckHandlesForbiddenWords(t *testing.T) {
	c := check{What: "бюджет", Any: []string{"1 200 000"}, None: []string{"900 000"}}
	if !c.ok("Бюджет 1 200 000 рублей.") {
		t.Errorf("верный ответ не принят")
	}
	if c.ok("Бюджет 900 000, потом 1 200 000.") {
		t.Errorf("ответ со старой суммой должен отвергаться")
	}
	if c.ok("Мы это не обсуждали.") {
		t.Errorf("ответ без суммы должен отвергаться")
	}
	only := check{What: "без чужого", None: []string{"pwa"}}
	if !only.ok("Выбрали Kotlin.") || only.ok("Выбрали PWA.") {
		t.Errorf("проверка только на отсутствие работает неверно")
	}
}
