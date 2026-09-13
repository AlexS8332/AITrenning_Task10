// Package history — диалог как данные и его хранение между запусками.
//
// Диалог здесь не лента, а дерево. Ветка — последовательность ходов,
// растущая из точки на другой ветке; путь ветки — то, что видит модель:
// унаследованная часть пути родителя плюс собственные сообщения. Обычный
// диалог без ветвлений — это дерево из одной ветки, и весь остальной код
// о разнице не знает: он спрашивает путь текущей ветки.
//
// Сообщения хранятся в том виде, в каком их получает модель (роли user,
// assistant, tool, вызовы инструментов с их id). Рядом лежат ходы — кто
// что спросил, что ответил агент, во что это обошлось и журнал работы, —
// и карточка фактов ветки. Один диалог — один JSON-файл в каталоге
// хранилища.
package history

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/facts"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
)

// Статусы хода.
const (
	TurnDone   = "done"
	TurnFailed = "failed"
)

// Сколько символов первого сообщения идёт в название диалога.
const titleRunes = 60

// RootBranchName — имя ветки, с которой диалог начинается.
const RootBranchName = "основная"

// ErrNoBranch — ветки с таким идентификатором в диалоге нет.
var ErrNoBranch = errors.New("ветка не найдена")

// ErrNoCheckpoint — точки сохранения с таким идентификатором нет.
var ErrNoCheckpoint = errors.New("точка не найдена")

// Totals — счётчики хода или диалога.
type Totals struct {
	LLMCalls  int       `json:"llmCalls"`
	ToolCalls int       `json:"toolCalls"`
	Usage     llm.Usage `json:"usage"`
	Cost      llm.Cost  `json:"cost"`
	Seconds   float64   `json:"seconds"`
}

// Add складывает счётчики.
func (t Totals) Add(o Totals) Totals {
	return Totals{
		LLMCalls:  t.LLMCalls + o.LLMCalls,
		ToolCalls: t.ToolCalls + o.ToolCalls,
		Usage:     t.Usage.Add(o.Usage),
		Cost:      t.Cost.Add(o.Cost),
		Seconds:   t.Seconds + o.Seconds,
	}
}

// Turn — один ход диалога: сообщение пользователя и ответ агента.
// Messages — сколько сообщений этот ход добавил в ветку; у неудачного
// хода ноль: в историю попадают только завершённые ходы.
type Turn struct {
	ID      string    `json:"id"`
	Started time.Time `json:"started"`
	Status  string    `json:"status"`
	// Branch — ветка, в которой сделан ход. Поле избыточно, пока ход
	// лежит внутри своей ветки, но путь склеивается из нескольких веток,
	// и в ленте видно, где проходит место ветвления.
	Branch   string        `json:"branch,omitempty"`
	User     string        `json:"user"`
	Reply    string        `json:"reply,omitempty"`
	Error    string        `json:"error,omitempty"`
	Messages int           `json:"messages"`
	Totals   Totals        `json:"totals"`
	Context  agent.Context `json:"context"`
	Events   []agent.Event `json:"events,omitempty"`
}

// Branch — ветка диалога. Messages и Turns — только собственные, после
// места ветвления; всё, что было до него, берётся у родителя и в файле не
// дублируется. Facts — карточка фактов ветки: при ветвлении она
// копируется, дальше у каждой ветки своя.
type Branch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Parent — ветка, из которой выросла эта; пусто у корневой.
	Parent string `json:"parent,omitempty"`
	// ForkAt — сколько первых сообщений пути родителя унаследовано,
	// ForkTurn — сколько его ходов. Индексы считаются по пути родителя, а
	// не по его собственным сообщениям: родитель сам может быть веткой.
	ForkAt   int       `json:"forkAt"`
	ForkTurn int       `json:"forkTurn"`
	Created  time.Time `json:"created"`

	Messages []llm.Message `json:"messages"`
	Turns    []Turn        `json:"turns"`
	Facts    facts.State   `json:"facts"`
}

// Checkpoint — точка сохранения: место в ветке, от которого можно
// отпочковаться. Снимок карточки фактов лежит здесь же: ветка должна
// начинаться с той памятью, какая была в точке, а не с нынешней.
type Checkpoint struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// At — сколько сообщений пути ветки зафиксировано, Turn — сколько
	// ходов.
	At   int `json:"at"`
	Turn int `json:"turn"`
	// After — идентификатор хода, после которого стоит точка; пусто, если
	// точка стоит в самом начале разговора. По нему интерфейс находит это
	// место в ленте: номер хода для навигации не годится, потому что в
	// ленте «весь диалог» ходы идут не по одной ветке.
	After   string      `json:"after,omitempty"`
	Created time.Time   `json:"created"`
	Facts   facts.State `json:"facts"`
}

// Conversation — диалог целиком: дерево веток, точки сохранения и та
// ветка, в которой сейчас идёт разговор. Системного промпта в сообщениях
// нет — его добавляет агент на каждом ходе, поэтому правка промпта в коде
// действует и на старые диалоги.
type Conversation struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	AgentKey string `json:"agent"`
	Model    string `json:"model"`
	// Strategy — что этот диалог отправляет модели вместо истории: full,
	// window или facts. Стратегия хранится в диалоге, а не берётся из
	// настроек сервера: дорожки стенда отличаются именно ею, и после
	// перезапуска каждая должна продолжаться по своим правилам.
	Strategy string `json:"strategy,omitempty"`
	// Group — идентификатор стенда: диалоги, которым задают одни и те же
	// вопросы одновременно. Пусто у обычного одиночного диалога.
	Group   string    `json:"group,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	Branches    []*Branch    `json:"branches"`
	Active      string       `json:"active"`
	Checkpoints []Checkpoint `json:"checkpoints,omitempty"`
}

// New создаёт диалог с одной веткой: пока не ветвились, дерево от ленты
// ничем не отличается.
func New(agentKey, model, strategy string) *Conversation {
	now := time.Now()
	root := &Branch{
		ID:       NewID(),
		Name:     RootBranchName,
		Created:  now,
		Messages: []llm.Message{},
		Turns:    []Turn{},
	}
	return &Conversation{
		ID:       NewID(),
		AgentKey: agentKey,
		Model:    model,
		Strategy: strategy,
		Created:  now,
		Updated:  now,
		Branches: []*Branch{root},
		Active:   root.ID,
	}
}

// Find — ветка по идентификатору.
func (c *Conversation) Find(id string) *Branch {
	for _, b := range c.Branches {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// Current — ветка, в которой идёт разговор. Если Active указывает в
// никуда (файл правили руками), берётся первая: диалог без текущей ветки
// нельзя ни показать, ни продолжить.
func (c *Conversation) Current() *Branch {
	if b := c.Find(c.Active); b != nil {
		return b
	}
	if len(c.Branches) > 0 {
		return c.Branches[0]
	}
	return nil
}

// Path — сообщения ветки в том порядке, в каком их видит модель:
// унаследованная часть пути родителя и собственные сообщения.
func (c *Conversation) Path(id string) []llm.Message {
	b := c.Find(id)
	if b == nil {
		return nil
	}
	var head []llm.Message
	if b.Parent != "" {
		head = c.Path(b.Parent)
		if b.ForkAt < len(head) {
			head = head[:b.ForkAt]
		}
	}
	out := make([]llm.Message, 0, len(head)+len(b.Messages))
	out = append(out, head...)
	return append(out, b.Messages...)
}

// PathTurns — ходы ветки вместе с унаследованными: лента показывает
// разговор с начала, а не с места ветвления.
func (c *Conversation) PathTurns(id string) []Turn {
	b := c.Find(id)
	if b == nil {
		return nil
	}
	var head []Turn
	if b.Parent != "" {
		head = c.PathTurns(b.Parent)
		if b.ForkTurn < len(head) {
			head = head[:b.ForkTurn]
		}
	}
	out := make([]Turn, 0, len(head)+len(b.Turns))
	out = append(out, head...)
	return append(out, b.Turns...)
}

// Linear — все сообщения всех веток одной лентой, в порядке появления.
// Так выглядел бы тот же разговор, если бы ветвиться было нельзя и всё
// шло одним потоком. Число нужно для сравнения: оно показывает, сколько
// контекста ветка не тащит за собой.
func (c *Conversation) Linear() []llm.Message {
	var out []llm.Message
	for _, b := range c.linearBranches() {
		out = append(out, b.Messages...)
	}
	return out
}

// LinearTurns — ходы всех веток той же лентой и в том же порядке, что и
// Linear. Строгой хронологией это не является: в ветку можно вернуться
// через десяток ходов в соседней, и тогда её ходы встанут раньше. Но
// сравнивают не хронологию, а «диалог, в котором ветвиться было нельзя»,
// — а он именно такой: сначала общая часть, потом один вариант, потом
// другой.
func (c *Conversation) LinearTurns() []Turn {
	var out []Turn
	for _, b := range c.linearBranches() {
		out = append(out, b.Turns...)
	}
	return out
}

// linearBranches — ветки в порядке появления.
func (c *Conversation) linearBranches() []*Branch {
	order := make([]*Branch, len(c.Branches))
	copy(order, c.Branches)
	sort.SliceStable(order, func(i, j int) bool { return order[i].Created.Before(order[j].Created) })
	return order
}

// Messages — путь текущей ветки: то, что уйдёт модели на следующем ходе.
func (c *Conversation) Messages() []llm.Message { return c.Path(c.Active) }

// Turns — ходы текущей ветки вместе с унаследованными.
func (c *Conversation) Turns() []Turn { return c.PathTurns(c.Active) }

// Facts — карточка фактов текущей ветки.
func (c *Conversation) Facts() facts.State {
	if b := c.Current(); b != nil {
		return b.Facts
	}
	return facts.State{}
}

// Append записывает завершённый ход в ветку: его сообщения уходят в
// историю ветки, сам ход — в её список ходов, карточка фактов ветки
// заменяется той, с которой ход закончился. Название диалога берётся из
// первого сообщения.
func (c *Conversation) Append(branchID string, turn Turn, added []llm.Message, f facts.State) error {
	b := c.Find(branchID)
	if b == nil {
		return fmt.Errorf("%w: %s", ErrNoBranch, branchID)
	}
	turn.Messages = len(added)
	turn.Branch = b.ID
	if f.Version >= b.Facts.Version {
		b.Facts = f
	}
	b.Messages = append(b.Messages, added...)
	b.Turns = append(b.Turns, turn)
	c.Updated = time.Now()
	if c.Title == "" && turn.User != "" {
		c.Title = MakeTitle(turn.User)
	}
	return nil
}

// Mark ставит точку сохранения на текущем конце ветки. Имя необязательно:
// без него точка называется по номеру хода, после которого стоит.
func (c *Conversation) Mark(branchID, name string) (Checkpoint, error) {
	b := c.Find(branchID)
	if b == nil {
		return Checkpoint{}, fmt.Errorf("%w: %s", ErrNoBranch, branchID)
	}
	at := len(c.Path(branchID))
	turns := c.PathTurns(branchID)
	turn := len(turns)
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("после хода %d", turn)
	}
	after := ""
	if turn > 0 {
		after = turns[turn-1].ID
	}
	cp := Checkpoint{
		ID: NewID(), Name: name, Branch: b.ID,
		At: at, Turn: turn, After: after, Created: time.Now(),
		Facts: b.Facts.Clone(),
	}
	c.Checkpoints = append(c.Checkpoints, cp)
	c.Updated = time.Now()
	return cp, nil
}

// Checkpoint — точка сохранения по идентификатору.
func (c *Conversation) Checkpoint(id string) (Checkpoint, bool) {
	for _, cp := range c.Checkpoints {
		if cp.ID == id {
			return cp, true
		}
	}
	return Checkpoint{}, false
}

// Fork заводит ветку от точки сохранения. Новая ветка начинается пустой:
// всё, что было до точки, она берёт у родителя, а всё, что после, — не
// видит вовсе. Карточка фактов копируется из снимка точки: ветка должна
// помнить то, что было известно в месте ветвления.
func (c *Conversation) Fork(checkpointID, name string) (*Branch, error) {
	cp, ok := c.Checkpoint(checkpointID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoCheckpoint, checkpointID)
	}
	if c.Find(cp.Branch) == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoBranch, cp.Branch)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("ветка %d", len(c.Branches)+1)
	}
	b := &Branch{
		ID: NewID(), Name: name, Parent: cp.Branch,
		ForkAt: cp.At, ForkTurn: cp.Turn, Created: time.Now(),
		Messages: []llm.Message{}, Turns: []Turn{},
		Facts: cp.Facts.Clone(),
	}
	c.Branches = append(c.Branches, b)
	c.Updated = time.Now()
	return b, nil
}

// Switch переводит разговор в другую ветку. Ничего не копирует и не
// удаляет: переключение — это смена того, какой путь уйдёт модели
// на следующем ходе.
func (c *Conversation) Switch(branchID string) error {
	if c.Find(branchID) == nil {
		return fmt.Errorf("%w: %s", ErrNoBranch, branchID)
	}
	c.Active = branchID
	c.Updated = time.Now()
	return nil
}

// Totals — сумма по всем ходам всех веток. Считается по дереву, а не по
// пути: заплачено и за ту ветку, в которой сейчас не сидим.
func (c *Conversation) Totals() Totals {
	var t Totals
	for _, b := range c.Branches {
		for _, turn := range b.Turns {
			t = t.Add(turn.Totals)
		}
	}
	return t
}

// FactsTotals — во что обошлась карточка фактов по всем веткам. Ветки
// наследуют расход родителя вместе с карточкой, поэтому суммировать их
// подряд нельзя: берётся наибольшее по каждой ветке дерева.
func (c *Conversation) FactsTotals() (llm.Usage, llm.Cost, float64) {
	var usage llm.Usage
	var cost llm.Cost
	var seconds float64
	// Расход карточки накапливается в State, а при ветвлении копируется.
	// Поэтому у каждой ветки считаем только её прирост над родителем.
	for _, b := range c.Branches {
		base := llm.Usage{}
		baseSeconds := 0.0
		if b.Parent != "" {
			if p := c.Find(b.Parent); p != nil {
				base = p.Facts.Usage
				baseSeconds = p.Facts.Seconds
			}
		}
		usage = usage.Add(llm.Usage{
			Prompt:     b.Facts.Usage.Prompt - base.Prompt,
			Completion: b.Facts.Usage.Completion - base.Completion,
			Total:      b.Facts.Usage.Total - base.Total,
			CacheHit:   b.Facts.Usage.CacheHit - base.CacheHit,
			CacheMiss:  b.Facts.Usage.CacheMiss - base.CacheMiss,
			Reasoning:  b.Facts.Usage.Reasoning - base.Reasoning,
		})
		seconds += b.Facts.Seconds - baseSeconds
		cost = cost.Add(b.Facts.Cost)
	}
	return usage, cost, seconds
}

// Runes — размер пути текущей ветки в символах: то, что модель читает на
// каждом ходе.
func (c *Conversation) Runes() int { return Runes(c.Messages()) }

// Clone — глубокая копия: диалог отдаётся наружу, пока его дописывает ход.
func (c *Conversation) Clone() *Conversation {
	out := *c
	out.Branches = make([]*Branch, len(c.Branches))
	for i, b := range c.Branches {
		copyBranch := *b
		// make + copy, а не append к nil: у пустой ветки append вернул бы
		// nil, и в JSON вместо пустого списка уходил бы null. Такую ветку
		// интерфейс запрашивает сразу после создания, пока первый ход ещё
		// идёт.
		copyBranch.Messages = make([]llm.Message, len(b.Messages))
		copy(copyBranch.Messages, b.Messages)
		copyBranch.Turns = make([]Turn, len(b.Turns))
		for j, t := range b.Turns {
			events := make([]agent.Event, len(t.Events))
			copy(events, t.Events)
			t.Events = events
			copyBranch.Turns[j] = t
		}
		copyBranch.Facts = b.Facts.Clone()
		out.Branches[i] = &copyBranch
	}
	out.Checkpoints = make([]Checkpoint, len(c.Checkpoints))
	copy(out.Checkpoints, c.Checkpoints)
	return &out
}

// MakeTitle — название диалога по первому сообщению: одна строка,
// обрезанная по словам.
func MakeTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) <= titleRunes {
		return text
	}
	cut := string(r[:titleRunes])
	if i := strings.LastIndex(cut, " "); i > titleRunes/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// Runes считает символы сообщений вместе с аргументами вызовов.
func Runes(ms []llm.Message) int {
	n := 0
	for _, m := range ms {
		n += len([]rune(m.Content))
		for _, c := range m.ToolCalls {
			n += len([]rune(c.Function.Arguments))
		}
	}
	return n
}

// Compact сокращает ответы инструментов прошлых ходов до keep символов.
// Файл хранит их целиком, а модели уходит короткий вариант: результаты
// поиска и разделы статей занимают тысячи символов, и без сокращения
// каждый ход тащил бы все прочитанные когда-либо статьи. Ответы модели и
// пользователя не трогаются: в них суть диалога. keep <= 0 — не сокращать.
func Compact(ms []llm.Message, keep int) []llm.Message {
	if keep <= 0 {
		return ms
	}
	out := make([]llm.Message, len(ms))
	for i, m := range ms {
		if m.Role == llm.RoleTool {
			if r := []rune(m.Content); len(r) > keep {
				m.Content = string(r[:keep]) + " …[сокращено: полный текст был в этом диалоге раньше, при необходимости вызови инструмент снова]"
			}
		}
		out[i] = m
	}
	return out
}

// NewID — идентификатор диалога, ветки, точки или хода: 16
// шестнадцатеричных знаков.
func NewID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// Store — каталог с JSON-файлами диалогов, по одному на диалог. Запись
// атомарная: во временный файл рядом и переименование, чтобы обрыв
// процесса посреди записи не оставил полуфайл вместо истории.
type Store struct {
	mu  sync.Mutex
	dir string
}

// NewStore привязывает хранилище к каталогу; сам каталог создаётся при
// первой записи.
func NewStore(dir string) *Store {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return &Store{dir: dir}
}

// Dir — каталог хранилища.
func (s *Store) Dir() string { return s.dir }

// Path — файл диалога.
func (s *Store) Path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// DisplayDir — каталог хранилища для показа человеку.
func (s *Store) DisplayDir() string { return Display(s.dir) }

// DisplayPath — файл диалога для показа человеку.
func (s *Store) DisplayPath(id string) string { return Display(s.Path(id)) }

// Display — путь в том виде, в каком его показывают в журнале запуска и в
// интерфейсе: относительно рабочего каталога, если лежит внутри него, и
// как есть в остальных случаях. Внутри хранилище держит абсолютный путь —
// он не зависит от того, сменит ли процесс каталог, — а наружу уходит
// короткий: обычно это просто «history», а заодно из интерфейса и
// скриншотов не торчит устройство чужой машины.
func Display(path string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// Save записывает диалог.
func (s *Store) Save(c *Conversation) error {
	if !validID(c.ID) {
		return fmt.Errorf("некорректный идентификатор диалога %q", c.ID)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("сериализация диалога: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("каталог истории: %w", err)
	}
	path := s.Path(c.ID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("запись %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("замена %s: %w", path, err)
	}
	return nil
}

// Load читает все диалоги каталога, новые первыми. Битый файл не роняет
// загрузку: он пропускается, а ошибка возвращается вместе с остальными
// диалогами, чтобы её показать.
func (s *Store) Load() ([]*Conversation, []error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("каталог истории: %w", err)}
	}

	var convs []*Conversation
	var problems []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		c, err := s.readFile(filepath.Join(s.dir, name))
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if c.ID != strings.TrimSuffix(name, ".json") {
			problems = append(problems, fmt.Errorf("%s: внутри диалог с id %q", name, c.ID))
			continue
		}
		convs = append(convs, c)
	}
	sort.Slice(convs, func(i, j int) bool {
		return convs[i].Updated.After(convs[j].Updated)
	})
	return convs, problems
}

func (s *Store) readFile(path string) (*Conversation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Conversation
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("не разобрался: %w", err)
	}
	if !validID(c.ID) {
		return nil, fmt.Errorf("некорректный id %q", c.ID)
	}
	if len(c.Branches) == 0 {
		return nil, fmt.Errorf("в диалоге нет ни одной ветки")
	}
	for _, b := range c.Branches {
		if b.Messages == nil {
			b.Messages = []llm.Message{}
		}
		if b.Turns == nil {
			b.Turns = []Turn{}
		}
	}
	if c.Find(c.Active) == nil {
		c.Active = c.Branches[0].ID
	}
	return &c, nil
}

// Raw — файл диалога как есть: интерфейс показывает, что лежит на диске.
func (s *Store) Raw(id string) ([]byte, error) {
	if !validID(id) {
		return nil, fmt.Errorf("некорректный идентификатор диалога %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.ReadFile(s.Path(id))
}

// Delete удаляет файл диалога. Отсутствие файла ошибкой не считается.
func (s *Store) Delete(id string) error {
	if !validID(id) {
		return fmt.Errorf("некорректный идентификатор диалога %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.Path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// validID — идентификатор безопасен как имя файла: только hex-знаки.
func validID(id string) bool {
	if len(id) < 8 || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
