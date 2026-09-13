// Package facts — память диалога в виде «ключ — значение».
//
// Конспект пересказывает разговор целиком и потому стареет весь сразу:
// чтобы поправить одну строку, его надо переписать. Карточка фактов
// устроена иначе. В ней лежат отдельные записи — цель, срок, стек,
// ограничение, договорённость, — и каждая правится независимо: новое
// значение замещает старое по тому же ключу, отменённое удаляется.
//
// Карточка обновляется после каждого сообщения пользователя: отдельный
// запрос к модели получает текущие факты и новое сообщение, а возвращает
// список правок. Сами сообщения диалога при этом не трогаются — карточка
// хранится рядом с ними и уходит модели вместо начала истории.
package facts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
)

// Значения по умолчанию.
const (
	// DefaultMaxFacts — потолок числа записей. Карточка должна быть
	// дешевле истории, которую она заменяет; без потолка она растёт
	// вместе с диалогом и экономия кончается.
	DefaultMaxFacts = 24
	// DefaultMaxValueRunes — потолок длины одного значения. Факт — это
	// строка «что именно решили», а не абзац рассуждений.
	DefaultMaxValueRunes = 200
	// DefaultMaxTokens — потолок ответа извлекателя. Он возвращает JSON с
	// правками, а не текст, и длинным этот ответ быть не должен.
	DefaultMaxTokens = 600
	// recentMessages — сколько последних сообщений показать извлекателю
	// вместе с новой репликой. Без них «да, подходит» и «второй вариант»
	// не во что превратить.
	recentMessages = 6
	// toolRunes — до скольких символов сокращать ответы инструментов,
	// когда они уходят извлекателю: ему нужен ход разговора, а не выдача
	// поиска.
	toolRunes = 400
)

// Entry — один факт. Since и Turn показывают, когда он появился и когда
// последний раз менялся: по ним видно, что карточка живёт, а не копится.
type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Since — ход, на котором факт появился, Turn — ход последнего
	// изменения.
	Since   int       `json:"since"`
	Turn    int       `json:"turn"`
	Updated time.Time `json:"updated,omitempty"`
}

// State — карточка фактов целиком вместе с ценой её ведения. Расход
// считается отдельно от диалога: карточка стоит запроса к модели на
// каждом ходе, и сравнение стратегий без этих денег было бы враньём.
type State struct {
	Entries []Entry `json:"entries"`
	// Version — сколько раз карточка менялась. Ход, на котором модель не
	// нашла новых фактов, версию не увеличивает.
	Version int       `json:"version"`
	Updated time.Time `json:"updated,omitempty"`
	Usage   llm.Usage `json:"usage"`
	Cost    llm.Cost  `json:"cost"`
	Seconds float64   `json:"seconds"`
	// Calls — сколько раз вызывался извлекатель, включая ходы без правок:
	// платим за них одинаково.
	Calls int `json:"calls"`
}

// Empty — в карточке нет ни одного факта.
func (s State) Empty() bool { return len(s.Entries) == 0 }

// Runes — размер карточки в символах: то, что уходит модели вместо
// начала истории.
func (s State) Runes() int {
	n := 0
	for _, e := range s.Entries {
		n += len([]rune(e.Key)) + len([]rune(e.Value))
	}
	return n
}

// Clone — глубокая копия. Карточка форкается вместе с веткой диалога, и
// правки в одной ветке не должны доходить до другой.
func (s State) Clone() State {
	out := s
	out.Entries = make([]Entry, len(s.Entries))
	copy(out.Entries, s.Entries)
	return out
}

// Get — значение факта по ключу.
func (s State) Get(key string) (string, bool) {
	for _, e := range s.Entries {
		if equalKey(e.Key, key) {
			return e.Value, true
		}
	}
	return "", false
}

// Set записывает факт: по существующему ключу значение замещается, новый
// ключ дописывается в конец. Порядок записей — порядок появления: по нему
// карточка читается как летопись решений.
func (s *State) Set(key, value string, turn int) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	for i, e := range s.Entries {
		if equalKey(e.Key, key) {
			s.Entries[i].Value = value
			s.Entries[i].Turn = turn
			s.Entries[i].Updated = time.Now()
			return
		}
	}
	s.Entries = append(s.Entries, Entry{Key: key, Value: value, Since: turn, Turn: turn, Updated: time.Now()})
}

// Delete убирает факт. Возвращает, был ли он вообще: отчёт о правках не
// должен обещать удаление того, чего не было.
func (s *State) Delete(key string) bool {
	for i, e := range s.Entries {
		if equalKey(e.Key, strings.TrimSpace(key)) {
			s.Entries = append(s.Entries[:i], s.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// trim удерживает карточку в пределах потолка. Лишним считается самый
// давно не обновлявшийся факт: карточка про то, где диалог находится
// сейчас, а не про то, с чего он начинался. Это осознанная потеря, и
// поэтому число выброшенного возвращается наружу — его видно в журнале.
func (s *State) trim(maxFacts int) int {
	if maxFacts <= 0 || len(s.Entries) <= maxFacts {
		return 0
	}
	dropped := 0
	for len(s.Entries) > maxFacts {
		oldest := 0
		for i, e := range s.Entries {
			if e.Turn < s.Entries[oldest].Turn {
				oldest = i
			}
		}
		s.Entries = append(s.Entries[:oldest], s.Entries[oldest+1:]...)
		dropped++
	}
	return dropped
}

// equalKey — ключи сравниваются без учёта регистра и краевых пробелов:
// модель пишет то «Срок», то «срок», и без этого карточка раздваивается.
func equalKey(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// Render — карточка в виде текста: по строке на факт. Этим же видом она
// уходит извлекателю, чтобы он правил именно те ключи, что уже есть.
func (s State) Render() string {
	var b strings.Builder
	for _, e := range s.Entries {
		fmt.Fprintf(&b, "- %s: %s\n", e.Key, e.Value)
	}
	return b.String()
}

// Prompt — карточка в том виде, в каком её получает модель: отдельным
// системным сообщением перед окном последних сообщений. Пояснение нужно
// не меньше самих фактов: без него модель принимает карточку за реплику
// пользователя и начинает её обсуждать.
func (s State) Prompt() string {
	if s.Empty() {
		return ""
	}
	return `Ниже — карточка фактов этого же диалога: всё существенное, что пользователь сообщил раньше, и всё, о чём вы договорились. Начало разговора дословно не показано — вместо него дана эта карточка, а дословно приведены только последние сообщения.
Считай факты из карточки частью диалога: они известны тебе от пользователя. Если он спрашивает о том, что было раньше, отвечай по карточке. Саму карточку не пересказывай и не упоминай, что часть диалога свёрнута.

` + strings.TrimRight(s.Render(), "\n")
}

// Policy — как вести карточку: потолки размера и модель извлекателя.
type Policy struct {
	// MaxFacts — потолок числа записей.
	MaxFacts int `json:"maxFacts"`
	// MaxValueRunes — потолок длины одного значения.
	MaxValueRunes int `json:"maxValueRunes"`
	// MaxTokens — потолок ответа извлекателя.
	MaxTokens int `json:"maxTokens"`
	// Model — модель извлекателя; пустая означает модель диалога.
	Model string `json:"model,omitempty"`
}

// DefaultPolicy — умолчания.
func DefaultPolicy() Policy {
	return Policy{MaxFacts: DefaultMaxFacts, MaxValueRunes: DefaultMaxValueRunes, MaxTokens: DefaultMaxTokens}
}

func (p Policy) normalized() Policy {
	if p.MaxFacts <= 0 {
		p.MaxFacts = DefaultMaxFacts
	}
	if p.MaxValueRunes <= 0 {
		p.MaxValueRunes = DefaultMaxValueRunes
	}
	if p.MaxTokens <= 0 {
		p.MaxTokens = DefaultMaxTokens
	}
	return p
}

// Patch — что вернул извлекатель: какие факты записать и какие убрать.
// Записи идут списком, а не объектом: порядок в JSON-объекте Go не
// сохраняет, а карточка читается по порядку.
type Patch struct {
	Set    []Entry  `json:"set"`
	Delete []string `json:"delete"`
}

// Update — итог одного обновления карточки.
type Update struct {
	State State
	// Changed — изменилась ли карточка. Ход без правок — обычное дело:
	// «спасибо, понятно» новых фактов не приносит.
	Changed bool
	// Set и Deleted — ключи, которые записаны и убраны этим обновлением,
	// Dropped — сколько фактов выброшено потолком.
	Set     []string
	Deleted []string
	Dropped int
	Usage   llm.Usage
	Cost    llm.Cost
	Seconds float64
}

// Extractor ведёт карточку. Он не знает ни об агенте, ни о журнале:
// вызывающий сам решает, что показать.
type Extractor struct {
	LLM    llm.Chatter
	Model  string
	Policy Policy
}

// Run обновляет карточку по новому сообщению пользователя. История нужна
// для контекста: по одной реплике «да, второй вариант» факта не собрать.
// Ошибка извлечения — не ошибка хода: вызывающий волен продолжить со
// старой карточкой.
func (e Extractor) Run(ctx context.Context, state State, history []llm.Message, user string, turn int) (Update, error) {
	if e.LLM == nil {
		return Update{State: state}, fmt.Errorf("карточка фактов не настроена: нет клиента модели")
	}
	p := e.Policy.normalized()
	model := p.Model
	if model == "" {
		model = e.Model
	}

	started := time.Now()
	resp, err := e.LLM.Chat(ctx, llm.Request{
		Model: model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: extractSystem},
			{Role: llm.RoleUser, Content: extractRequest(state, history, user, p)},
		},
		Temperature: 0,
		MaxTokens:   p.MaxTokens,
	})
	elapsed := time.Since(started)

	// Расход считается и при неудаче разбора: запрос уже оплачен.
	usage := resp.Usage
	cost := llm.PriceOf(model, usage, started)
	next := state.Clone()
	next.Calls++
	next.Usage = next.Usage.Add(usage)
	next.Cost = next.Cost.Add(cost)
	next.Seconds += elapsed.Seconds()

	if err != nil {
		return Update{State: state}, fmt.Errorf("карточка фактов: %w", err)
	}
	patch, err := ParsePatch(resp.Message.Content)
	if err != nil {
		return Update{State: next, Usage: usage, Cost: cost, Seconds: elapsed.Seconds()},
			fmt.Errorf("карточка фактов: %w", err)
	}

	upd := Update{Usage: usage, Cost: cost, Seconds: elapsed.Seconds()}
	for _, key := range patch.Delete {
		if next.Delete(key) {
			upd.Deleted = append(upd.Deleted, strings.TrimSpace(key))
		}
	}
	for _, entry := range patch.Set {
		key := strings.TrimSpace(entry.Key)
		value := clip(entry.Value, p.MaxValueRunes)
		if key == "" || value == "" {
			continue
		}
		if old, ok := next.Get(key); ok && old == value {
			// Тот же факт тем же значением: правкой это не считается,
			// иначе версия карточки росла бы на каждом ходе.
			continue
		}
		next.Set(key, value, turn)
		upd.Set = append(upd.Set, key)
	}
	upd.Dropped = next.trim(p.MaxFacts)

	upd.Changed = len(upd.Set) > 0 || len(upd.Deleted) > 0 || upd.Dropped > 0
	if upd.Changed {
		next.Version++
		next.Updated = time.Now()
	}
	upd.State = next
	return upd, nil
}

// ParsePatch разбирает ответ извлекателя. Модель то и дело заворачивает
// JSON в ```-ограду или предваряет его фразой, поэтому берётся самый
// внешний объект ответа, а не весь текст целиком.
func ParsePatch(s string) (Patch, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return Patch{}, fmt.Errorf("пустой ответ")
	}
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			text = text[i : j+1]
		}
	}
	var p Patch
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return Patch{}, fmt.Errorf("ответ не разобрался как JSON: %w", err)
	}
	return p, nil
}

func clip(s string, keep int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	r := []rune(s)
	if keep <= 0 || len(r) <= keep {
		return s
	}
	return string(r[:keep]) + "…"
}

// extractSystem — роль извлекателя. Он не разговаривает с пользователем и
// ничего не пересказывает: его ответ — список правок карточки. Главное в
// промпте — требование переиспользовать существующие ключи: иначе
// «срок», «сроки» и «дедлайн» живут в карточке втроём и противоречат
// друг другу.
const extractSystem = `Ты ведёшь карточку фактов диалога между пользователем и агентом. Карточка заменяет собой начало разговора: сами сообщения будут удалены из контекста, и всё, чего нет в карточке, агент забудет навсегда.

Тебе дают текущую карточку, последние сообщения диалога и новое сообщение пользователя. Верни правки карточки — и ничего больше.

Формат ответа — один объект JSON, без пояснений и без ограды из обратных кавычек:

{"set": [{"key": "ключ", "value": "значение"}], "delete": ["ключ"]}

Правила:
- Если новых фактов нет, верни {"set": [], "delete": []}. Это нормальный и частый ответ.
- Ключ — короткое существительное или словосочетание в нижнем регистре: «цель», «срок», «бюджет», «язык», «база данных», «авторизация», «не делаем».
- Существующие ключи переиспользуй дословно. Новое значение замещает старое; заводить «срок 2» или «сроки» вместо «срок» нельзя.
- Значение — одна строка до 200 знаков, по существу и по-русски. Сохраняй числа, названия и имена дословно.
- Записывай то, что сообщил или решил пользователь: цель, ограничения, предпочтения, принятые решения, договорённости о том, как отвечать, и сведения о нём самом. Согласие пользователя с предложением агента — тоже решение пользователя.
- Не записывай то, что агент предложил, но пользователь не подтвердил, и не додумывай за него.
- Не записывай справочные подробности и рассуждения: их агент при необходимости выведет заново.
- Если пользователь передумал, обнови значение по тому же ключу. Если факт отменён и замены нет — удали ключ через "delete".`

// extractRequest — что уходит извлекателю: текущая карточка, хвост
// диалога и новое сообщение. Карточка передаётся целиком: правки
// осмысленны только рядом с тем, что уже записано.
func extractRequest(state State, history []llm.Message, user string, p Policy) string {
	var b strings.Builder
	b.WriteString("=== Текущая карточка ===\n")
	if state.Empty() {
		b.WriteString("(пусто)\n")
	} else {
		b.WriteString(state.Render())
	}
	if len(state.Entries) >= p.MaxFacts {
		fmt.Fprintf(&b, "\nВ карточке %d фактов при потолке %d. Обязательно удали в этом же ответе те, что устарели или потеряли смысл.\n",
			len(state.Entries), p.MaxFacts)
	}

	b.WriteString("\n=== Последние сообщения диалога ===\n")
	tail := history
	if len(tail) > recentMessages {
		tail = tail[len(tail)-recentMessages:]
	}
	if len(tail) == 0 {
		b.WriteString("(диалог только начинается)\n")
	} else {
		b.WriteString(Render(tail))
	}

	b.WriteString("\n=== Новое сообщение пользователя ===\n")
	b.WriteString(strings.TrimSpace(user))
	b.WriteString("\n")
	return b.String()
}

// Render — сообщения в виде текста для извлекателя. Роли названы
// по-русски, вызовы инструментов сведены к имени, ответы сокращены:
// карточке нужен ход разговора, а не выдача поиска целиком.
func Render(ms []llm.Message) string {
	var b strings.Builder
	for _, m := range ms {
		switch m.Role {
		case llm.RoleUser:
			fmt.Fprintf(&b, "Пользователь: %s\n", strings.TrimSpace(m.Content))
		case llm.RoleAssistant:
			if text := strings.TrimSpace(m.Content); text != "" {
				fmt.Fprintf(&b, "Агент: %s\n", text)
			}
			for _, c := range m.ToolCalls {
				fmt.Fprintf(&b, "Агент вызвал %s\n", c.Function.Name)
			}
		case llm.RoleTool:
			fmt.Fprintf(&b, "Инструмент вернул: %s\n", clip(m.Content, toolRunes))
		default:
			fmt.Fprintf(&b, "%s: %s\n", m.Role, clip(m.Content, toolRunes))
		}
	}
	return b.String()
}
