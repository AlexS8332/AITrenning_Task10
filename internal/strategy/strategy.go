// Package strategy — переключатель стратегий управления контекстом.
//
// История модель получает заново на каждом ходе и платит за неё тоже
// каждый ход. Стратегия решает, что именно из истории туда попадёт.
// Здесь их три, и они сравнимы между собой, потому что различаются одним:
//
//	full   — вся история как есть; эталон, с которым сравнивают остальные;
//	window — только последние Keep сообщений, всё старше выброшено;
//	facts  — карточка фактов плюс те же последние Keep сообщений.
//
// Четвёртая стратегия — ветвление — живёт не здесь, а в пакете history:
// она решает не «сколько истории уходит модели», а «какая история вообще
// существует». Ветка задаёт путь по дереву диалога, а любая из здешних
// стратегий применяется уже к этому пути.
package strategy

import (
	"fmt"

	"github.com/AlexS8332/AITrenning_Task10/internal/facts"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
)

// Стратегии работы с историей. Они же — дорожки стенда и сценарии опыта:
// сравнивать окно и факты имеет смысл только рядом с полной историей,
// иначе непонятно, что дала стратегия, а что — сама обрезка.
const (
	// ModeFull — вся история пути уходит модели как есть.
	ModeFull = "full"
	// ModeWindow — только последние сообщения, всё старше выброшено без
	// замены. Точка отсчёта по деньгам: дешевле некуда, но и памяти нет.
	ModeWindow = "window"
	// ModeFacts — карточка фактов плюс последние сообщения.
	ModeFacts = "facts"
)

// DefaultKeep — сколько последних сообщений уходит модели дословно.
// Восемь — это примерно четыре последних хода: короткие реплики вроде
// «да, давай так» опираются на них дословно, и сворачивать их нельзя.
const DefaultKeep = 8

// Modes — все стратегии в порядке показа: от «помнит всё» к «помнит
// факты».
func Modes() []string { return []string{ModeFull, ModeWindow, ModeFacts} }

// Valid — знакома ли стратегия.
func Valid(mode string) bool {
	for _, m := range Modes() {
		if m == mode {
			return true
		}
	}
	return false
}

// Title — название стратегии для человека.
func Title(mode string) string {
	switch mode {
	case ModeFull:
		return "вся история"
	case ModeWindow:
		return "окно"
	case ModeFacts:
		return "факты"
	default:
		return mode
	}
}

// Policy — стратегия вместе с её настройками.
type Policy struct {
	Mode string `json:"mode"`
	// Keep — сколько последних сообщений отправлять дословно. У ModeFull
	// не используется.
	Keep int `json:"keep"`
	// Facts — настройки карточки фактов; работают только у ModeFacts.
	Facts facts.Policy `json:"facts"`
}

// DefaultPolicy — факты с умолчаниями.
func DefaultPolicy() Policy {
	return Policy{Mode: ModeFacts, Keep: DefaultKeep, Facts: facts.DefaultPolicy()}
}

// Normalized — политика с подставленными умолчаниями вместо нулей.
// Пустая стратегия — это ModeFull, а не умолчание приложения: нулевая
// политика не должна молча начинать резать историю и звать вторую модель.
// Умолчание для запуска живёт в DefaultPolicy и во флагах.
func (p Policy) Normalized() Policy {
	if p.Mode == "" {
		p.Mode = ModeFull
	}
	if p.Keep <= 0 {
		p.Keep = DefaultKeep
	}
	return p
}

// Label — стратегия словами, для журнала запуска и интерфейса.
func (p Policy) Label() string {
	p = p.Normalized()
	switch p.Mode {
	case ModeFull:
		return "уходит модели целиком, без обрезки"
	case ModeWindow:
		return "только последние " + Plural(p.Keep, "сообщение", "сообщения", "сообщений") + ", остальное выбрасывается"
	default:
		f := p.Facts
		model := f.Model
		if model == "" {
			model = "той же моделью"
		} else {
			model = "моделью " + model
		}
		if f.MaxFacts <= 0 {
			f.MaxFacts = facts.DefaultMaxFacts
		}
		return fmt.Sprintf("последние %s дословно, остальное — карточкой фактов (%s, до %d записей)",
			Plural(p.Keep, "сообщение", "сообщения", "сообщений"), model, f.MaxFacts)
	}
}

// Boundary — сколько первых сообщений истории отрезается, чтобы модели
// осталось около keep последних. Граница выравнивается по началу хода:
// ответ инструмента без вызова, который его породил, API отвергает,
// поэтому резать историю посреди хода нельзя.
func Boundary(history []llm.Message, keep int) int {
	if keep < 0 {
		keep = 0
	}
	if len(history) <= keep {
		return 0
	}
	cut := len(history) - keep
	// Вперёд до начала следующего хода: окно получится чуть меньше
	// заказанного, зато целым.
	for i := cut; i < len(history); i++ {
		if history[i].Role == llm.RoleUser {
			return i
		}
	}
	// Последний ход длиннее окна (много ответов инструментов) — отступаем
	// назад к его началу: окно выйдет больше keep, но отрезать что-то всё
	// равно лучше, чем не отрезать ничего.
	for i := cut - 1; i > 0; i-- {
		if history[i].Role == llm.RoleUser {
			return i
		}
	}
	return 0
}

// Apply — что уходит модели вместо полной истории: отдельный блок памяти
// (карточка фактов или пустая строка) и хвост истории после отрезанной
// части. Сообщения не копируются глубоко: их никто дальше не правит.
func Apply(state facts.State, history []llm.Message, policy Policy) (block string, window []llm.Message) {
	p := policy.Normalized()
	switch p.Mode {
	case ModeFull:
		return "", history
	case ModeWindow:
		return "", history[Boundary(history, p.Keep):]
	default:
		return state.Prompt(), history[Boundary(history, p.Keep):]
	}
}

// Plural — число со словом в нужном падеже: «1 сообщение», «4 сообщения»,
// «12 сообщений». Строки про контекст читают люди, и «4 сообщений» в них
// режет глаз не меньше, чем ошибка в числе.
func Plural(n int, one, few, many string) string {
	word := many
	switch {
	case n%10 == 1 && n%100 != 11:
		word = one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 10 || n%100 >= 20):
		word = few
	}
	return fmt.Sprintf("%d %s", n, word)
}
