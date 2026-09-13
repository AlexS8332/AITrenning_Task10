package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/agents"
	"github.com/AlexS8332/AITrenning_Task10/internal/facts"
	"github.com/AlexS8332/AITrenning_Task10/internal/history"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/strategy"
	"github.com/AlexS8332/AITrenning_Task10/internal/tokens"
)

// Здесь живёт опытная часть задания. Она состоит из двух половин.
//
// Первая: один и тот же сбор технического задания прогоняется тремя
// стратегиями — с полной историей, со скользящим окном и с карточкой
// фактов — и сводится в таблицу. Сравнивать надо две вещи сразу: сколько
// потрачено токенов и что агент при этом помнит. По отдельности они врут:
// выбросить историю целиком дёшево, но агент забудет всё.
//
// Вторая: ветвление. От одной точки разговора расходятся две ветки, в
// каждой обсуждается свой вариант решения, и тот же вопрос задаётся в
// обеих. Рядом идёт тот же разговор одной лентой, без ветвления. Так
// видно, что ветка даёт помимо экономии: в ней нет чужого варианта, и
// путать нечего.
//
// Сервер для опыта не нужен — нужен тот же агент, что и в интерфейсе.

// reportAgent — агент, на котором ставится опыт. Инструментов у него нет
// намеренно: в опыте важна работа с контекстом, а не работа с источниками.
const reportAgent = "analyst"

// scenario — один прогон: какой стратегией обходиться с историей.
type scenario struct {
	Name string
	Note string
	Mode string
}

func scenarios() []scenario {
	return []scenario{
		{
			Name: "вся история",
			Note: "весь разговор уходит модели заново каждый ход — эталон, с которым сравнивают остальные",
			Mode: strategy.ModeFull,
		},
		{
			Name: "окно",
			Note: "модель получает только последние сообщения, всё старше выброшено без замены",
			Mode: strategy.ModeWindow,
		},
		{
			Name: "факты",
			Note: "те же последние сообщения плюс карточка фактов — ключ-значение, которую ведёт отдельный запрос к модели после каждой реплики пользователя",
			Mode: strategy.ModeFacts,
		},
	}
}

// check — что должно найтись в ответе (Any) и чего в нём быть не должно
// (None), чтобы считать, что агент помнит. Проверка грубая, зато
// воспроизводимая и бесплатная: судить о памяти по вхождению слова в
// ответ честнее, чем спрашивать о ней ту же модель.
type check struct {
	What string
	Any  []string
	None []string
}

// ok — прошла ли проверка на этом ответе.
func (c check) ok(reply string) bool {
	if len(c.Any) > 0 && !mentions(reply, c.Any) {
		return false
	}
	if len(c.None) > 0 && mentions(reply, c.None) {
		return false
	}
	return true
}

// question — ход диалога и проверки к нему.
type question struct {
	Text   string
	Checks []check
}

// Сценарий подобран так, чтобы начало было содержательным и
// невосстановимым из хвоста: в первых ходах пользователь называет себя,
// компанию, город, срок и бюджет, в середине меняет решение о бюджете, а
// контрольные вопросы в конце спрашивают ровно о том, что к этому времени
// уже выпало из окна последних сообщений.
var dialogQuestions = []question{
	{Text: "Привет! Меня зовут Марина, я продакт в сети пекарен «Тёплый угол» — 14 точек в Казани. Хочу собрать с тобой ТЗ на систему учёта списаний."},
	{Text: "Пользователей ровно двое: пекарь на точке и управляющий сетью. Пекарь отмечает списание, управляющий смотрит сводку."},
	{Text: "Списывают по трём причинам: не продали до конца дня, брак при выпечке, бой при перевозке. Причину пекарь выбирает из списка."},
	{Text: "Сроки жёсткие: первая версия к 1 марта 2027 года. Бюджет пока 900 тысяч рублей."},
	{Text: "Номенклатуру берём из 1С: выгрузка на все точки раз в сутки, ночью."},
	{Text: "Интерфейс пекаря — только телефон, компьютеров на точках нет. У управляющего — веб."},
	{Text: "Важное ограничение: пекарь работает в перчатках и в муке. Значит, крупные кнопки и не больше трёх касаний на одно списание."},
	{Text: "Фиксируем: офлайн-режим обязателен, на трёх точках интернет отваливается. Данные уходят на сервер, когда связь появляется."},
	{Text: "Отчёты управляющему нужны три: списания за день по точкам, топ причин за неделю, сравнение точек за месяц."},
	{Text: "Я передумала насчёт денег: бюджет согласовали в 1 200 000 рублей. Срок при этом сдвигать нельзя."},
	{Text: "В первую версию не входит: прогноз спроса, интеграция с кассой и отдельное приложение для управляющего."},
	{Text: "Авторизация: у пекаря пин-код на точке, у управляющего почта и пароль."},
	{Text: "И ещё: все суммы в рублях, НДС не считаем — это внутренний учёт, не бухгалтерия."},
	{
		Text: "Напомни, как меня зовут, что за компания и где она работает.",
		Checks: []check{
			{What: "имя", Any: []string{"марин"}},
			{What: "компания", Any: []string{"тёпл", "тепл"}},
			{What: "город", Any: []string{"казан"}},
		},
	},
	{
		Text: "Какой у нас сейчас бюджет и какой срок?",
		Checks: []check{
			// Обратного условия «и чтобы старой суммы в ответе не было»
			// здесь нет намеренно. Оно было и давало ложные срабатывания:
			// ответ «1 200 000 (ты подняла его с 900 тысяч)» — правильный,
			// а проверка его заваливала. Отличить «назвал отменённую сумму
			// как действующую» от «упомянул историю решения» вхождением
			// слова нельзя, и делать вид, что можно, нечестно.
			{What: "бюджет после правки", Any: []string{"1 200 000", "1200000", "1,2 млн", "1.2 млн"}},
			{What: "срок", Any: []string{"1 марта", "01.03", "март"}},
		},
	},
	{
		Text: "Почему мы решили делать крупные кнопки и ограничить списание тремя касаниями?",
		Checks: []check{
			{What: "перчатки и мука", Any: []string{"перчат", "мук"}},
		},
	},
	{
		Text: "Что мы договорились не делать в первой версии?",
		Checks: []check{
			{What: "прогноз спроса", Any: []string{"прогноз"}},
			{What: "интеграция с кассой", Any: []string{"касс"}},
		},
	},
	{
		Text: "Собери итоговое ТЗ по всему, о чём мы договорились.",
		Checks: []check{
			{What: "город", Any: []string{"казан"}},
			{What: "срок", Any: []string{"1 марта", "01.03", "март"}},
			{What: "бюджет после правки", Any: []string{"1 200 000", "1200000", "1,2 млн", "1.2 млн"}},
			{What: "офлайн-режим", Any: []string{"офлайн", "оффлайн", "без связи", "без интернета"}},
			{What: "пин-код", Any: []string{"пин"}},
			{What: "три причины списания", Any: []string{"бой", "перевозк"}},
		},
	},
}

// checkResult — итог одной проверки контрольного вопроса.
type checkResult struct {
	What string
	OK   bool
}

// turnRow — строка таблицы: один ход одного сценария.
type turnRow struct {
	N          int
	User       string
	Branch     string
	HistoryMsg int // сколько сообщений в пути ветки к началу хода
	SentMsg    int // сколько из них ушло модели дословно
	Estimate   tokens.Estimate
	Full       int // оценка того же хода с полным путём ветки
	Linear     int // оценка того же хода, если бы веток не было
	FirstReal  int
	Usage      llm.Usage
	Cost       llm.Cost
	Cumulative float64
	FactsVer   int
	FactsCount int
	Reply      string
	Checks     []checkResult
	Seconds    float64
	Err        string
}

// Passed — все ли проверки хода прошли.
func (r turnRow) Passed() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return len(r.Checks) > 0
}

// result — итог сценария.
type result struct {
	scenario scenario
	rows     []turnRow
	total    llm.Usage
	cost     float64
	facts    facts.State
	seconds  float64
}

// checksPassed — сколько контрольных проверок прошло из скольких.
func (r result) checksPassed() (ok, total int) {
	for _, row := range r.rows {
		for _, c := range row.Checks {
			total++
			if c.OK {
				ok++
			}
		}
	}
	return ok, total
}

// factsCost — во что обошлась карточка фактов за весь диалог.
func factsCost(s facts.State) float64 {
	if !s.Cost.Known {
		return 0
	}
	return s.Cost.USD
}

// runReport прогоняет обе половины опыта и пишет отчёт в markdown. Каждый
// сценарий начинается с пустой истории: диалоги независимы, сравнивать их
// можно. При parallel сценарии идут одновременно — они друг другу не
// мешают, а три диалога подряд занимают втрое больше времени.
func runReport(ctx context.Context, deps agents.Deps, model, path string, parallel bool, log io.Writer) error {
	list := scenarios()
	results := make([]result, len(list))
	errs := make([]error, len(list))
	out := &syncWriter{w: log}

	if parallel {
		var wg sync.WaitGroup
		for i, sc := range list {
			wg.Add(1)
			go func(i int, sc scenario) {
				defer wg.Done()
				results[i], errs[i] = runScenario(ctx, deps, sc, out)
			}(i, sc)
		}
		wg.Wait()
	} else {
		for i, sc := range list {
			results[i], errs[i] = runScenario(ctx, deps, sc, out)
		}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	fmt.Fprintf(log, "\nОпыт с ветвлением: общая часть, две ветки от одной точки, тот же разговор одной лентой.\n")
	forks, err := runBranching(ctx, deps, out)
	if err != nil {
		return err
	}

	var b strings.Builder
	writeReport(&b, model, deps.Strategy.Normalized(), results, forks)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("запись отчёта: %w", err)
	}
	fmt.Fprintf(log, "\nОтчёт записан: %s\n", history.Display(path))
	return nil
}

// syncWriter — общий вывод для параллельных сценариев: строки не должны
// перемешиваться посреди слова.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// runScenario ведёт диалог до конца или до первой ошибки. Ошибка ход не
// прерывает молча: она попадает в таблицу.
func runScenario(ctx context.Context, deps agents.Deps, sc scenario, log io.Writer) (result, error) {
	entry, ok := agents.Find(reportAgent)
	if !ok {
		return result{}, fmt.Errorf("агент %s не найден", reportAgent)
	}
	deps.Strategy.Mode = sc.Mode
	a := entry.Build(deps)

	res := result{scenario: sc}
	var hist []llm.Message
	var card facts.State
	var cumulative float64

	for i, q := range dialogQuestions {
		started := time.Now()
		reply, err := a.Reply(ctx, agent.Input{History: hist, Facts: card, User: q.Text, Turn: i + 1}, nil)
		elapsed := time.Since(started).Seconds()
		card = reply.Facts

		row := turnRow{
			N: i + 1, User: q.Text, HistoryMsg: len(hist),
			SentMsg:    len(hist) - reply.Stats.Context.Dropped,
			Estimate:   reply.Stats.Context.Estimate,
			Full:       reply.Stats.Context.Full,
			FirstReal:  reply.Stats.Context.FirstPrompt,
			Usage:      reply.Stats.Usage,
			Cost:       reply.Stats.Cost,
			FactsVer:   reply.Stats.Context.FactsVersion,
			FactsCount: len(card.Entries),
			Reply:      reply.Text,
			Seconds:    elapsed,
		}
		if err != nil {
			row.Err = err.Error()
			res.rows = append(res.rows, row)
			fmt.Fprintf(log, "  [%s] ход %2d: %s\n", sc.Name, row.N, err)
			break
		}
		for _, c := range q.Checks {
			row.Checks = append(row.Checks, checkResult{What: c.What, OK: c.ok(reply.Text)})
		}

		if reply.Stats.Cost.Known {
			cumulative += reply.Stats.Cost.USD
		}
		row.Cumulative = cumulative
		res.rows = append(res.rows, row)
		res.total = res.total.Add(reply.Stats.Usage)
		res.seconds += elapsed
		res.cost = cumulative
		hist = append(hist, reply.Added...)

		fmt.Fprintf(log, "  [%s] ход %2d: пути %2d сообщ., модели %2d, ≈%5d ток. (без стратегии ≈%5d), факт %5d, %s%s\n",
			sc.Name, row.N, row.HistoryMsg, row.SentMsg, row.Estimate.Total, row.Full, row.FirstReal,
			usd(row.Cost), checksNote(row.Checks))
	}
	res.facts = card
	return res, nil
}

/* ---------- Опыт с ветвлением ---------- */

// Общая часть разговора: до места ветвления. Короче основного сценария —
// ветвление проверяют не на памяти, а на независимости веток.
var branchBase = []question{
	{Text: "Привет! Меня зовут Марина, я продакт в сети пекарен «Тёплый угол» — 14 точек в Казани. Собираем ТЗ на систему учёта списаний."},
	{Text: "Пользователей двое: пекарь на точке и управляющий сетью. Пекарь отмечает списание, управляющий смотрит сводку."},
	{Text: "Первая версия к 1 марта 2027 года, бюджет 1 200 000 рублей."},
	{Text: "Пекарь работает в перчатках и в муке, компьютеров на точках нет — значит, интерфейс на телефоне и крупные кнопки."},
	{Text: "Офлайн-режим обязателен: на трёх точках интернет отваливается."},
	{Text: "Теперь надо выбрать, чем именно пекарь пользуется на точке. Давай разберём варианты по очереди."},
}

// Две ветки от одной точки: в каждой обсуждается свой вариант клиента.
// Вопросы намеренно одинаковы по смыслу и различаются только выбором —
// иначе разницу в ответах можно было бы списать на разные разговоры.
var branchOptions = []struct {
	Name      string
	Questions []question
}{
	{
		Name: "нативное приложение",
		Questions: []question{
			{Text: "Вариант первый: нативное приложение на Kotlin под Android, раздаём apk через MDM. Разбираем его."},
			{Text: "Офлайн там из коробки: локальная база на устройстве, синхронизация фоновой задачей. Фиксируем этот вариант как выбранный."},
			{
				Text: "Напомни, какой вариант клиента мы выбрали и почему он подходит под наши ограничения.",
				Checks: []check{
					{What: "выбран нативный клиент", Any: []string{"kotlin", "нативн", "apk", "android"}},
					{What: "чужого варианта в ответе нет", None: []string{"pwa", "прогрессивн"}},
				},
			},
		},
	},
	{
		Name: "PWA",
		Questions: []question{
			{Text: "Вариант первый: PWA — веб-приложение, которое пекарь ставит на телефон из браузера. Разбираем его."},
			{Text: "Офлайн там через service worker и IndexedDB, синхронизация при появлении сети. Фиксируем этот вариант как выбранный."},
			{
				Text: "Напомни, какой вариант клиента мы выбрали и почему он подходит под наши ограничения.",
				Checks: []check{
					{What: "выбран PWA", Any: []string{"pwa", "service worker", "браузер"}},
					{What: "чужого варианта в ответе нет", None: []string{"kotlin", "apk"}},
				},
			},
		},
	},
}

// branchRun — одна дорожка опыта с ветвлением: ветка или линейный
// разговор без ветвления.
type branchRun struct {
	Name string
	Note string
	Rows []turnRow
	// Own — сколько ходов дорожка сделала сама, сверх общей части.
	Own   int
	Usage llm.Usage
	Cost  float64
}

// checksPassed — сколько проверок прошло из скольких.
func (r branchRun) checksPassed() (ok, total int) {
	for _, row := range r.Rows {
		for _, c := range row.Checks {
			total++
			if c.OK {
				ok++
			}
		}
	}
	return ok, total
}

// branchReport — итог опыта с ветвлением: общая часть, две ветки и тот же
// разговор одной лентой.
type branchReport struct {
	Base    branchRun
	Forks   []branchRun
	Linear  branchRun
	Tree    []string // дерево диалога словами, для отчёта
	Sharing int      // сколько сообщений унаследовали обе ветки
}

// dialog — диалог, который ведёт опыт: настоящее дерево веток из пакета
// history, а не список сообщений. Так опыт проверяет тот же код, что
// работает в интерфейсе, а не его пересказ.
type dialog struct {
	conv *history.Conversation
	a    agent.Agent
}

func newDialog(deps agents.Deps, mode, model string) (*dialog, error) {
	entry, ok := agents.Find(reportAgent)
	if !ok {
		return nil, fmt.Errorf("агент %s не найден", reportAgent)
	}
	deps.Strategy.Mode = mode
	return &dialog{conv: history.New(reportAgent, model, mode), a: entry.Build(deps)}, nil
}

// ask делает один ход в текущей ветке и дописывает его в дерево.
func (d *dialog) ask(ctx context.Context, q question) (turnRow, error) {
	branch := d.conv.Current()
	path := d.conv.Path(branch.ID)
	started := time.Now()
	reply, err := d.a.Reply(ctx, agent.Input{
		History: path,
		Facts:   branch.Facts.Clone(),
		User:    q.Text,
		Turn:    len(d.conv.PathTurns(branch.ID)) + 1,
		Branch:  branch.Name,
		Linear:  d.conv.Linear(),
	}, nil)
	elapsed := time.Since(started).Seconds()

	row := turnRow{
		N: len(d.conv.PathTurns(branch.ID)) + 1, User: q.Text, Branch: branch.Name,
		HistoryMsg: len(path),
		SentMsg:    len(path) - reply.Stats.Context.Dropped,
		Estimate:   reply.Stats.Context.Estimate,
		Full:       reply.Stats.Context.Full,
		Linear:     reply.Stats.Context.Linear,
		FirstReal:  reply.Stats.Context.FirstPrompt,
		Usage:      reply.Stats.Usage,
		Cost:       reply.Stats.Cost,
		Reply:      reply.Text,
		Seconds:    elapsed,
	}
	if err != nil {
		row.Err = err.Error()
		return row, err
	}
	for _, c := range q.Checks {
		row.Checks = append(row.Checks, checkResult{What: c.What, OK: c.ok(reply.Text)})
	}
	turn := history.Turn{ID: history.NewID(), Started: started, Status: history.TurnDone,
		User: q.Text, Reply: reply.Text}
	if err := d.conv.Append(branch.ID, turn, reply.Added, reply.Facts); err != nil {
		return row, err
	}
	return row, nil
}

// runBranching ставит опыт с ветвлением: общая часть, две ветки от одной
// точки и тот же разговор одной лентой. Стратегия здесь — полная история:
// окно и факты сюда не подмешиваются, чтобы разница в числах была разницей
// ветвления, а не обрезки.
func runBranching(ctx context.Context, deps agents.Deps, log io.Writer) (branchReport, error) {
	var rep branchReport

	d, err := newDialog(deps, strategy.ModeFull, deps.Runner.Model)
	if err != nil {
		return rep, err
	}
	rep.Base = branchRun{Name: "общая часть", Note: "разговор до места ветвления; его унаследуют обе ветки"}
	for _, q := range branchBase {
		row, err := d.ask(ctx, q)
		rep.Base.Rows = append(rep.Base.Rows, row)
		rep.Base.Usage = rep.Base.Usage.Add(row.Usage)
		if row.Cost.Known {
			rep.Base.Cost += row.Cost.USD
		}
		if err != nil {
			return rep, fmt.Errorf("общая часть, ход %d: %w", row.N, err)
		}
		fmt.Fprintf(log, "  [ветки: общая часть] ход %d: ≈%d ток., %s\n", row.N, row.Estimate.Total, usd(row.Cost))
	}

	cp, err := d.conv.Mark(d.conv.Active, "конец общей части")
	if err != nil {
		return rep, err
	}
	rep.Sharing = cp.At

	for _, option := range branchOptions {
		b, err := d.conv.Fork(cp.ID, option.Name)
		if err != nil {
			return rep, err
		}
		if err := d.conv.Switch(b.ID); err != nil {
			return rep, err
		}
		run := branchRun{Name: option.Name, Note: "ветка от точки «" + cp.Name + "»"}
		for _, q := range option.Questions {
			row, err := d.ask(ctx, q)
			run.Rows = append(run.Rows, row)
			run.Own++
			run.Usage = run.Usage.Add(row.Usage)
			if row.Cost.Known {
				run.Cost += row.Cost.USD
			}
			if err != nil {
				return rep, fmt.Errorf("ветка %s, ход %d: %w", option.Name, row.N, err)
			}
			fmt.Fprintf(log, "  [ветка: %s] ход %d: пути %d сообщ., ≈%d ток. (одной лентой было бы ≈%d), %s%s\n",
				option.Name, row.N, row.HistoryMsg, row.Estimate.Total, row.Linear, usd(row.Cost), checksNote(row.Checks))
		}
		rep.Forks = append(rep.Forks, run)
	}
	rep.Tree = treeLines(d.conv)

	// Та же история одной лентой: общая часть, оба варианта подряд и тот
	// же вопрос в конце. Ветвиться нельзя — значит, оба варианта лежат в
	// одном контексте, и за оба платят.
	lin, err := newDialog(deps, strategy.ModeFull, deps.Runner.Model)
	if err != nil {
		return rep, err
	}
	rep.Linear = branchRun{Name: "одной лентой",
		Note: "тот же разговор без ветвления: оба варианта обсуждаются подряд в одном контексте"}
	linearQuestions := append([]question{}, branchBase...)
	for _, option := range branchOptions {
		// Последний вопрос ветки — контрольный, в линейном прогоне он
		// нужен один раз и в самом конце.
		linearQuestions = append(linearQuestions, option.Questions[:len(option.Questions)-1]...)
	}
	last := branchOptions[0].Questions[len(branchOptions[0].Questions)-1]
	linearQuestions = append(linearQuestions, question{Text: last.Text, Checks: []check{
		{What: "назван ровно один вариант", Any: []string{"kotlin", "нативн", "apk", "android"}, None: []string{"pwa", "service worker"}},
	}})
	for _, q := range linearQuestions {
		row, err := lin.ask(ctx, q)
		rep.Linear.Rows = append(rep.Linear.Rows, row)
		rep.Linear.Usage = rep.Linear.Usage.Add(row.Usage)
		if row.Cost.Known {
			rep.Linear.Cost += row.Cost.USD
		}
		if err != nil {
			return rep, fmt.Errorf("одной лентой, ход %d: %w", row.N, err)
		}
		fmt.Fprintf(log, "  [одной лентой] ход %2d: пути %2d сообщ., ≈%d ток., %s%s\n",
			row.N, row.HistoryMsg, row.Estimate.Total, usd(row.Cost), checksNote(row.Checks))
	}
	rep.Linear.Own = len(rep.Linear.Rows)
	return rep, nil
}

// treeLines — дерево диалога словами: какая ветка от какой и с какого
// места. В отчёте это единственное место, где структуру видно целиком.
func treeLines(c *history.Conversation) []string {
	var out []string
	for _, b := range c.Branches {
		line := fmt.Sprintf("%s — ходов своих %d, в пути %d, сообщений в пути %d",
			b.Name, len(b.Turns), len(c.PathTurns(b.ID)), len(c.Path(b.ID)))
		if b.Parent != "" {
			if p := c.Find(b.Parent); p != nil {
				line = fmt.Sprintf("%s — от «%s» после хода %d; ходов своих %d, в пути %d, сообщений в пути %d",
					b.Name, p.Name, b.ForkTurn, len(b.Turns), len(c.PathTurns(b.ID)), len(c.Path(b.ID)))
			}
		}
		out = append(out, line)
	}
	return out
}

// mentions — есть ли в ответе хоть один из вариантов. Регистр не важен,
// достаточно вхождения корня: «Марина», «Марине» и «Марин» — одно и то же.
func mentions(reply string, any []string) bool {
	low := strings.ToLower(reply)
	for _, v := range any {
		if strings.Contains(low, strings.ToLower(v)) {
			return true
		}
	}
	return false
}

func checksNote(checks []checkResult) string {
	if len(checks) == 0 {
		return ""
	}
	var parts []string
	for _, c := range checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
		}
		parts = append(parts, mark+" "+c.What)
	}
	return " | " + strings.Join(parts, ", ")
}

/* ---------- Проба переполнения ---------- */

// probeQuestion — вопрос, которым заканчивается проба переполнения.
const probeQuestion = "Сколько всего ты помнишь?"

// overflowHistory набирает историю примерно на target токенов из
// повторяющихся ходов. Шаг цикла — вес обоих сообщений: на удвоенном весе
// длинного набиралась ровно половина заказанного, потому что вопрос
// короткий.
func overflowHistory(target int) ([]llm.Message, tokens.Estimate) {
	question := llm.Message{Role: llm.RoleUser, Content: "Продолжаем разбирать требования."}
	answer := llm.Message{Role: llm.RoleAssistant,
		Content: strings.Repeat("Зафиксировано требование к системе учёта списаний на точке продаж. ", 400)}
	step := int(tokens.Default.Message(question) + tokens.Default.Message(answer))

	var hist []llm.Message
	for est := 0; est < target; est += step {
		hist = append(hist, question, answer)
	}
	return hist, tokens.Of("", "", nil, hist, probeQuestion)
}

// probeOverflow отправляет заведомо слишком длинный запрос и показывает,
// что ответит API. Свой лимит здесь выключен намеренно, и стратегия тоже:
// смысл пробы в том, чтобы услышать настоящий отказ модели, а не своё
// сообщение. Отвергнутый запрос не тарифицируется, поэтому проба бесплатна.
func probeOverflow(ctx context.Context, deps agents.Deps, target int, log io.Writer) error {
	hist, est := overflowHistory(target)
	fmt.Fprintf(log, "Проба переполнения: %d сообщений, оценка ≈%d токенов (лимит модели %d).\n",
		len(hist), est.Total, modelContextLimit)

	// Запрос, который модель примет, стоит денег и ничего не доказывает.
	// Проба имеет смысл, только если он заведомо не влезает.
	if est.Total < modelContextLimit {
		return fmt.Errorf("набрано только ≈%d токенов: такой запрос модель примет и он будет оплачен; увеличь -probe-tokens", est.Total)
	}

	deps.Runner.ContextLimit = 0
	deps.Runner.OnOverflow = agent.OverflowOff
	deps.Strategy.Mode = strategy.ModeFull
	entry, _ := agents.Find(reportAgent)
	started := time.Now()
	_, err := entry.Build(deps).Reply(ctx, agent.Input{History: hist, User: probeQuestion, Turn: 1}, nil)
	elapsed := time.Since(started)

	if err == nil {
		fmt.Fprintf(log, "Модель ответила: запрос уложился в контекст. Увеличь -probe-tokens.\n")
		return nil
	}
	fmt.Fprintf(log, "Ответ API через %.1f с:\n\n%s\n", elapsed.Seconds(), err)
	return nil
}

/* ---------- Отчёт ---------- */

func writeReport(b *strings.Builder, model string, policy strategy.Policy, results []result, forks branchReport) {
	fmt.Fprintf(b, "# Управление контекстом: окно, факты и ветки\n\n")
	fmt.Fprintf(b, "Опыт из двух половин. Первая: один и тот же сбор технического задания из %d ходов, ", len(dialogQuestions))
	fmt.Fprintf(b, "прогнанный тремя стратегиями работы с историей. Вторая: то же начало разговора, ")
	fmt.Fprintf(b, "от которого расходятся две ветки, и он же — одной лентой без ветвления.\n\n")
	fmt.Fprintf(b, "Модель `%s`, температура 0, агент-аналитик без инструментов: в опыте важна работа с контекстом, ", model)
	fmt.Fprintf(b, "а не работа с источниками. Дата прогона: %s.\n\n", time.Now().Format("2006-01-02"))
	fmt.Fprintf(b, "Окно — %d последних сообщений, карточка фактов — до %d записей, ", policy.Keep, policy.Facts.MaxFacts)
	fmt.Fprintf(b, "обновляется отдельным запросом к модели после каждой реплики пользователя. ")
	fmt.Fprintf(b, "В первых ходах пользователь называет себя, компанию, город, срок и бюджет, в середине меняет решение о бюджете, ")
	fmt.Fprintf(b, "а последние пять вопросов спрашивают о начале разговора — к этому моменту его в окне уже нет.\n\n")

	writeTotals(b, results)
	writeMoney(b, model, results)
	writeChecks(b, results)

	for _, res := range results {
		writeScenario(b, res)
	}

	writeAnswers(b, results)
	writeCards(b, results)
	writeBranching(b, forks)
}

// writeTotals — главная таблица: во что обошлась каждая стратегия и что
// от неё осталось в памяти агента.
func writeTotals(b *strings.Builder, results []result) {
	fmt.Fprintf(b, "## Итоги\n\n")
	fmt.Fprintf(b, "| стратегия | вход, токенов | из кэша | выход | карточка: запросов / токенов | стоил диалог | стоила карточка | всего | контрольные вопросы |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|---|---|---|---|\n")
	for _, res := range results {
		ok, total := res.checksPassed()
		checks := "—"
		if total > 0 {
			checks = fmt.Sprintf("%d из %d", ok, total)
		}
		card := "—"
		if res.facts.Calls > 0 {
			card = fmt.Sprintf("%d / %d", res.facts.Calls, res.facts.Usage.Total)
		}
		fmt.Fprintf(b, "| %s | %d | %d | %d | %s | %s | %s | %s | %s |\n",
			res.scenario.Name, res.total.Prompt, res.total.CacheHit, res.total.Completion,
			card, usdValue(res.cost), usd(res.facts.Cost), usdValue(res.cost+factsCost(res.facts)), checks)
	}

	base := results[0]
	fmt.Fprintf(b, "\n")
	for _, res := range results[1:] {
		saved := base.total.Prompt - res.total.Prompt
		pct := 0.0
		if base.total.Prompt > 0 {
			pct = float64(saved) / float64(base.total.Prompt) * 100
		}
		ok, total := res.checksPassed()
		fmt.Fprintf(b, "- **%s**: на вход ушло на %d токенов меньше (%.0f %%), контрольных вопросов пройдено %d из %d.\n",
			res.scenario.Name, saved, pct, ok, total)
	}
	fmt.Fprintf(b, "\n")
}

// writeMoney — почему экономия токенов не равна экономии денег. У DeepSeek
// совпадающий префикс запроса стоит в тридцать раз дешевле, а любая
// стратегия этот префикс меняет. Поэтому рядом с уплаченной ценой стоит
// цена без кэша: она показывает, чего стоил бы тот же диалог у провайдера
// без кэширования префикса.
func writeMoney(b *strings.Builder, model string, results []result) {
	fmt.Fprintf(b, "## Токены и деньги — не одно и то же\n\n")
	fmt.Fprintf(b, "| стратегия | вход, токенов | из них по цене кэша | заплачено | заплатили бы без кэша |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|\n")
	now := time.Now()
	for _, res := range results {
		share := 0.0
		if res.total.Prompt > 0 {
			share = float64(res.total.CacheHit) / float64(res.total.Prompt) * 100
		}
		// Тот же расход, но так, как если бы кэша не было вовсе.
		raw := llm.PriceOf(model, llm.Usage{Prompt: res.total.Prompt, Completion: res.total.Completion}, now)
		rawCard := llm.PriceOf(model, llm.Usage{Prompt: res.facts.Usage.Prompt, Completion: res.facts.Usage.Completion}, now)
		fmt.Fprintf(b, "| %s | %d | %.0f %% | %s | %s |\n",
			res.scenario.Name, res.total.Prompt, share,
			usdValue(res.cost+factsCost(res.facts)), usdValue(raw.USD+rawCard.USD))
	}
	fmt.Fprintf(b, "\nКэш держится на неизменном начале запроса. Пока история только дописывается в конец, ")
	fmt.Fprintf(b, "префикс не меняется и почти весь контекст идёт по цене кэша. Скользящее окно меняет начало запроса ")
	fmt.Fprintf(b, "на каждом ходе, карточка фактов — на каждом ходе, когда в ней что-то поправили: ")
	fmt.Fprintf(b, "это видно в столбце «из кэша» у ходов, отмеченных ✎.\n\n")
}

// writeChecks — что агент помнил в каждой стратегии, вопрос за вопросом.
func writeChecks(b *strings.Builder, results []result) {
	fmt.Fprintf(b, "## Контрольные вопросы\n\n")
	fmt.Fprintf(b, "Проверка грубая и оттого честная: в ответе ищется имя, число или слово, ")
	fmt.Fprintf(b, "которое агент мог знать только из начала диалога. У неё есть цена — она видит вхождение слова, ")
	fmt.Fprintf(b, "а не смысл: связный, уверенный и неверный ответ она засчитает, если нужное слово в нём есть. ")
	fmt.Fprintf(b, "Поэтому ниже лежат и сами ответы: числа показывают цену, а качество видно только глазами.\n\n")

	fmt.Fprintf(b, "| ход | вопрос | что проверяем |")
	for _, res := range results {
		fmt.Fprintf(b, " %s |", res.scenario.Name)
	}
	fmt.Fprintf(b, "\n|---|---|---|")
	for range results {
		fmt.Fprintf(b, "---|")
	}
	fmt.Fprintf(b, "\n")

	for i, q := range dialogQuestions {
		for j, c := range q.Checks {
			text, what := "", c.What
			if j == 0 {
				text = shorten(q.Text)
			}
			fmt.Fprintf(b, "| %d | %s | %s |", i+1, text, what)
			for _, res := range results {
				fmt.Fprintf(b, " %s |", checkMark(res, i+1, c.What))
			}
			fmt.Fprintf(b, "\n")
		}
	}
	fmt.Fprintf(b, "\n")
}

func checkMark(res result, turn int, what string) string {
	for _, row := range res.rows {
		if row.N != turn {
			continue
		}
		if row.Err != "" {
			return "ход не состоялся"
		}
		for _, c := range row.Checks {
			if c.What == what {
				if c.OK {
					return "✓"
				}
				return "✗"
			}
		}
	}
	return "—"
}

// writeScenario — ход за ходом: сколько сообщений ушло модели, во что это
// обошлось и сколько стоил бы тот же ход с полной историей.
func writeScenario(b *strings.Builder, res result) {
	fmt.Fprintf(b, "## %s\n\n%s\n\n", res.scenario.Name, res.scenario.Note)
	fmt.Fprintf(b, "| ход | реплика | история / модели, сообщ. | оценка | вся история | факт | из кэша | выход | ход стоил | всего |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range res.rows {
		if r.Err != "" {
			fmt.Fprintf(b, "| %d | %s | %d / %d | ≈%d | ≈%d | — | — | — | — | **ход не состоялся** |\n",
				r.N, shorten(r.User), r.HistoryMsg, r.SentMsg, r.Estimate.Total, r.Full)
			continue
		}
		fmt.Fprintf(b, "| %d%s | %s | %d / %d | ≈%d | ≈%d | %d | %d | %d | %s | %s |\n",
			r.N, factsMark(res, r), shorten(r.User), r.HistoryMsg, r.SentMsg,
			r.Estimate.Total, r.Full, r.FirstReal, r.Usage.CacheHit, r.Usage.Completion,
			usd(r.Cost), usdValue(r.Cumulative))
	}
	fmt.Fprintf(b, "\nИтого: %d ходов, %d токенов на вход, %d на выход, %s, %.1f с.\n",
		len(res.rows), res.total.Prompt, res.total.Completion, usdValue(res.cost), res.seconds)
	if res.facts.Calls > 0 {
		fmt.Fprintf(b, "Карточка: %d запросов к модели, %d правок, к концу диалога %d записей на %d символов, ",
			res.facts.Calls, res.facts.Version, len(res.facts.Entries), res.facts.Runes())
		fmt.Fprintf(b, "потрачено %d токенов и %s.\n", res.facts.Usage.Total, usd(res.facts.Cost))
		fmt.Fprintf(b, "\nЗначок ✎ у хода означает, что перед ним карточка изменилась.\n")
	}
	fmt.Fprintf(b, "\n")
	for _, r := range res.rows {
		if r.Err != "" {
			fmt.Fprintf(b, "Ход %d не состоялся:\n\n```\n%s\n```\n\n", r.N, r.Err)
		}
	}
}

// factsMark отмечает ходы, перед которыми карточка изменилась: именно на
// них ломается кэш префикса, и это видно в столбце «из кэша».
func factsMark(res result, row turnRow) string {
	prev := 0
	for _, r := range res.rows {
		if r.N == row.N {
			break
		}
		prev = r.FactsVer
	}
	if row.FactsVer > prev {
		return " ✎"
	}
	return ""
}

// writeAnswers — сами ответы на контрольные вопросы. Числа показывают
// цену, а качество видно только глазами.
func writeAnswers(b *strings.Builder, results []result) {
	fmt.Fprintf(b, "## Ответы на контрольные вопросы\n\n")
	for i, q := range dialogQuestions {
		if len(q.Checks) == 0 {
			continue
		}
		fmt.Fprintf(b, "### Ход %d. %s\n\n", i+1, q.Text)
		for _, res := range results {
			fmt.Fprintf(b, "**%s:**\n\n%s\n\n", res.scenario.Name, answerOf(res, i+1))
		}
	}
}

func answerOf(res result, turn int) string {
	for _, row := range res.rows {
		if row.N == turn {
			if row.Err != "" {
				return "_ход не состоялся: " + row.Err + "_"
			}
			return blockquote(row.Reply)
		}
	}
	return "_хода не было_"
}

// blockquote — ответ модели цитатой: он многострочный и в markdown должен
// остаться одним блоком.
func blockquote(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// writeCards — карточка фактов, какой она стала к концу диалога. Это и
// есть вся память агента о начале разговора.
func writeCards(b *strings.Builder, results []result) {
	for _, res := range results {
		if res.facts.Empty() {
			continue
		}
		fmt.Fprintf(b, "## Карточка фактов в конце диалога (%s)\n\n", res.scenario.Name)
		fmt.Fprintf(b, "Версия %d, %d записей, %d символов, %d запросов к модели.\n\n",
			res.facts.Version, len(res.facts.Entries), res.facts.Runes(), res.facts.Calls)
		fmt.Fprintf(b, "| ключ | значение | записан | обновлён |\n|---|---|---|---|\n")
		for _, e := range res.facts.Entries {
			fmt.Fprintf(b, "| %s | %s | ход %d | ход %d |\n", e.Key, e.Value, e.Since, e.Turn)
		}
		fmt.Fprintf(b, "\n")
	}
}

// writeBranching — вторая половина опыта: две ветки от одной точки против
// того же разговора одной лентой.
func writeBranching(b *strings.Builder, rep branchReport) {
	fmt.Fprintf(b, "## Ветвление\n\n")
	fmt.Fprintf(b, "Разговор ведётся до места, где надо выбрать между двумя вариантами клиента. ")
	fmt.Fprintf(b, "Дальше он расходится: в каждой ветке разбирается свой вариант и фиксируется как выбранный, ")
	fmt.Fprintf(b, "а потом в обеих задаётся один и тот же вопрос — какой вариант мы выбрали. ")
	fmt.Fprintf(b, "Рядом тот же разговор идёт одной лентой: ветвиться нельзя, оба варианта лежат в одном контексте. ")
	fmt.Fprintf(b, "Стратегия у всех трёх дорожек — полная история: окно и факты сюда не подмешиваются, ")
	fmt.Fprintf(b, "чтобы разница в числах была разницей ветвления.\n\n")

	if len(rep.Tree) > 0 {
		fmt.Fprintf(b, "Дерево диалога после опыта:\n\n")
		for _, line := range rep.Tree {
			fmt.Fprintf(b, "- %s\n", line)
		}
		fmt.Fprintf(b, "\nОбщая часть — %d сообщений — лежит в файле один раз и наследуется обеими ветками.\n\n", rep.Sharing)
	}

	runs := append([]branchRun{rep.Base}, rep.Forks...)
	runs = append(runs, rep.Linear)
	fmt.Fprintf(b, "| дорожка | своих ходов | вход, токенов | выход | стоила | проверки |\n")
	fmt.Fprintf(b, "|---|---|---|---|---|---|\n")
	for _, r := range runs {
		ok, total := r.checksPassed()
		checks := "—"
		if total > 0 {
			checks = fmt.Sprintf("%d из %d", ok, total)
		}
		own := r.Own
		if own == 0 {
			own = len(r.Rows)
		}
		fmt.Fprintf(b, "| %s | %d | %d | %d | %s | %s |\n",
			r.Name, own, r.Usage.Prompt, r.Usage.Completion, usdValue(r.Cost), checks)
	}

	// Сравнивать сумму веток с суммой ленты в лоб нельзя: ходов в них
	// разное число — контрольный вопрос в ветках задаётся дважды. Честное
	// сравнение поштучное: у каждого хода ветки уже посчитано, во сколько
	// он обошёлся бы, если бы веток не было.
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
	forksTurns := len(rep.Base.Rows)
	forksCost := rep.Base.Cost
	for _, r := range rep.Forks {
		forksTurns += len(r.Rows)
		forksCost += r.Cost
	}

	fmt.Fprintf(b, "\nОбщая часть оплачена один раз (%d токенов на вход) и досталась обеим веткам: ", rep.Base.Usage.Prompt)
	fmt.Fprintf(b, "в файле она лежит в одном экземпляре, и каждая ветка видит её как начало своего пути.\n\n")
	if comparable > 0 {
		fmt.Fprintf(b, "Сколько снимает само ветвление, видно поштучно: %s, у которых есть с чем сравнивать, ",
			strategy.Plural(comparable, "ход ветки", "хода ветки", "ходов веток"))
		fmt.Fprintf(b, "обошлись в ≈%d токенов вместо ≈%d, которые ушли бы на те же ходы одной лентой, — ",
			withBranch, withoutBranch)
		fmt.Fprintf(b, "меньше на ≈%d, и разрыв растёт с каждым ходом. ", withoutBranch-withBranch)
		fmt.Fprintf(b, "«Есть с чем сравнивать» не у всех ходов: пока вторая ветка не заведена, ")
		fmt.Fprintf(b, "диалог ничем не отличается от ленты, и у первой ветки в этом столбце прочерк.\n\n")
	}
	fmt.Fprintf(b, "А вот итог целиком дешевле не выходит: опыт с ветками — это %s и %s, ",
		strategy.Plural(forksTurns, "ход", "хода", "ходов"), usdValue(forksCost))
	fmt.Fprintf(b, "лента — %s и %s. Ходов в ветках больше: контрольный вопрос там задаётся дважды, по разу в каждой. ",
		strategy.Plural(len(rep.Linear.Rows), "ход", "хода", "ходов"), usdValue(rep.Linear.Cost))
	fmt.Fprintf(b, "Ветвление окупается не на двух ходах, а когда ветка длинная: тогда каждый её ход не тащит чужой вариант. ")
	fmt.Fprintf(b, "И главное в нём всё равно не цена, а то, что в каждой ветке ровно один вариант — путать нечего.\n\n")

	fmt.Fprintf(b, "### Ход за ходом\n\n")
	for _, r := range runs {
		fmt.Fprintf(b, "**%s** — %s\n\n", r.Name, r.Note)
		fmt.Fprintf(b, "| ход | реплика | в пути, сообщ. | оценка | одной лентой | факт | ход стоил |\n")
		fmt.Fprintf(b, "|---|---|---|---|---|---|---|\n")
		for _, row := range r.Rows {
			linear := "—"
			if row.Linear > 0 {
				linear = fmt.Sprintf("≈%d", row.Linear)
			}
			if row.Err != "" {
				fmt.Fprintf(b, "| %d | %s | %d | ≈%d | %s | — | **ход не состоялся** |\n",
					row.N, shorten(row.User), row.HistoryMsg, row.Estimate.Total, linear)
				continue
			}
			fmt.Fprintf(b, "| %d | %s | %d | ≈%d | %s | %d | %s |\n",
				row.N, shorten(row.User), row.HistoryMsg, row.Estimate.Total, linear,
				row.FirstReal, usd(row.Cost))
		}
		fmt.Fprintf(b, "\n")
	}

	fmt.Fprintf(b, "### Что ответили ветки на один и тот же вопрос\n\n")
	for _, r := range runs {
		last := lastChecked(r)
		if last == nil {
			continue
		}
		fmt.Fprintf(b, "**%s:**\n\n%s\n\n", r.Name, blockquote(last.Reply))
		for _, c := range last.Checks {
			mark := "✓"
			if !c.OK {
				mark = "✗"
			}
			fmt.Fprintf(b, "- %s %s\n", mark, c.What)
		}
		fmt.Fprintf(b, "\n")
	}
}

// lastChecked — последний ход дорожки, к которому были проверки: именно
// он и есть контрольный вопрос.
func lastChecked(r branchRun) *turnRow {
	for i := len(r.Rows) - 1; i >= 0; i-- {
		if len(r.Rows[i].Checks) > 0 {
			return &r.Rows[i]
		}
	}
	return nil
}

// modelContextLimit — контекст deepseek-v4-flash и deepseek-v4-pro.
// Документация округляет до «1M», API в отказе называет точное число.
const modelContextLimit = 1_048_576

func shorten(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:39]) + "…"
}

func usd(c llm.Cost) string {
	if !c.Known {
		return "—"
	}
	return usdValue(c.USD)
}

func usdValue(v float64) string {
	if v >= 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.6f", v)
}
