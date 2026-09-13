package runs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/agents"
	"github.com/AlexS8332/AITrenning_Task10/internal/facts"
	"github.com/AlexS8332/AITrenning_Task10/internal/history"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/strategy"
)

// Сколько завершённых ходов держим в памяти для потока событий: журнал
// каждого хода после завершения лежит в файле диалога.
const maxTurnsInMemory = 50

var (
	// ErrNotFound — диалога нет ни в памяти, ни в хранилище.
	ErrNotFound = errors.New("диалог не найден")
	// ErrBusy — в диалоге уже идёт ход; следующий можно отправить после ответа.
	ErrBusy = errors.New("агент ещё отвечает на предыдущее сообщение")
)

// FactsInfo — карточка фактов цифрами, без самих записей: список диалогов
// показывает, сколько их и во что они обошлись.
type FactsInfo struct {
	Version int       `json:"version"`
	Count   int       `json:"count"`
	Runes   int       `json:"runes"`
	Calls   int       `json:"calls"`
	Usage   llm.Usage `json:"usage"`
	Cost    llm.Cost  `json:"cost"`
	Seconds float64   `json:"seconds"`
}

func factsInfo(s facts.State) FactsInfo {
	return FactsInfo{
		Version: s.Version,
		Count:   len(s.Entries),
		Runes:   s.Runes(),
		Calls:   s.Calls,
		Usage:   s.Usage,
		Cost:    s.Cost,
		Seconds: s.Seconds,
	}
}

// BranchInfo — ветка для интерфейса: где отпочковалась, сколько своего
// успела нажить и что помнит.
type BranchInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Parent   string    `json:"parent,omitempty"`
	ForkAt   int       `json:"forkAt"`
	ForkTurn int       `json:"forkTurn"`
	Created  time.Time `json:"created"`
	Active   bool      `json:"active"`
	// Turns и Messages — собственные, после места ветвления; PathTurns и
	// PathMessages — вместе с унаследованными, то есть то, что видит
	// модель.
	Turns        int            `json:"turns"`
	Messages     int            `json:"messages"`
	PathTurns    int            `json:"pathTurns"`
	PathMessages int            `json:"pathMessages"`
	Totals       history.Totals `json:"totals"`
	Facts        FactsInfo      `json:"facts"`
}

// Summary — диалог для списка: без сообщений и журналов.
type Summary struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	AgentKey   string         `json:"agentKey"`
	AgentTitle string         `json:"agentTitle"`
	Model      string         `json:"model"`
	Created    time.Time      `json:"created"`
	Updated    time.Time      `json:"updated"`
	Turns      int            `json:"turns"`
	Messages   int            `json:"messages"`
	Runes      int            `json:"runes"`
	Totals     history.Totals `json:"totals"`
	Running    bool           `json:"running"`
	Path       string         `json:"path"`
	// Strategy — что этот диалог шлёт модели вместо истории, Group —
	// стенд, к которому он принадлежит (пусто у одиночного диалога).
	Strategy string `json:"strategy,omitempty"`
	Group    string `json:"group,omitempty"`
	// Facts — карточка фактов текущей ветки в числах.
	Facts FactsInfo `json:"facts"`
	// Branches и CheckpointCount — размер дерева, BranchName — где сейчас
	// идёт разговор. Точки здесь только счётчиком: сами они лежат в
	// Detail, и одноимённое поле в обоих местах запутало бы и JSON, и код.
	Branches        int    `json:"branches"`
	CheckpointCount int    `json:"checkpointCount"`
	BranchID        string `json:"branchId,omitempty"`
	BranchName      string `json:"branchName,omitempty"`
}

// Detail — диалог целиком: сводка, путь текущей ветки, ходы, дерево и
// текущий ход, если он идёт.
type Detail struct {
	Summary
	Messages []llm.Message  `json:"messages"`
	Turns    []history.Turn `json:"turns"`
	Active   *View          `json:"active,omitempty"`
	// FactsCard — карточка фактов целиком, вместе с записями: интерфейс
	// показывает её отдельной панелью, потому что модель видит именно её,
	// а не начало списка сообщений.
	FactsCard facts.State `json:"factsCard"`
	// Tree — все ветки диалога, Checkpoints — точки, от которых можно
	// отпочковаться.
	Tree        []BranchInfo         `json:"tree"`
	Checkpoints []history.Checkpoint `json:"checkpoints"`
}

// ComparisonModes — стратегии, которые сравниваются на стенде, в порядке
// дорожек на экране: от «помнит всё» к «помнит факты».
var ComparisonModes = strategy.Modes()

// Comparison — стенд: несколько диалогов, которым задают одни и те же
// вопросы. Дорожки идут в порядке ComparisonModes, ходы в них совпадают по
// номеру: n-й ход всюду отвечает на n-й вопрос.
type Comparison struct {
	Group   string    `json:"group"`
	Title   string    `json:"title"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	Lanes   []Detail  `json:"lanes"`
}

// Manager хранит диалоги, запускает ходы и записывает историю.
type Manager struct {
	mu      sync.Mutex
	convs   map[string]*history.Conversation
	active  map[string]*Session // диалог → ход в работе
	turns   map[string]*Session // ход → сессия, для потока событий
	order   []string            // ходы в порядке запуска, для вытеснения
	deps    agents.Deps
	store   *history.Store
	timeout time.Duration
}

func NewManager(deps agents.Deps, store *history.Store, timeout time.Duration) *Manager {
	return &Manager{
		convs:   make(map[string]*history.Conversation),
		active:  make(map[string]*Session),
		turns:   make(map[string]*Session),
		deps:    deps,
		store:   store,
		timeout: timeout,
	}
}

// Load поднимает диалоги из хранилища. Это и есть восстановление контекста
// после перезапуска: всё, что лежит в каталоге, снова доступно для
// продолжения — вместе с ветками, точками и карточками фактов. Ошибки
// чтения отдельных файлов возвращаются списком.
func (m *Manager) Load() (int, []error) {
	convs, errs := m.store.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range convs {
		m.convs[c.ID] = c
	}
	return len(convs), errs
}

// DisplayDir — каталог хранилища для показа.
func (m *Manager) DisplayDir() string { return m.store.DisplayDir() }

// Strategy — стратегия работы с историей, с которой запущен сервер.
func (m *Manager) Strategy() strategy.Policy { return m.deps.Strategy }

// List — все диалоги, новые первыми.
func (m *Manager) List() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Summary, 0, len(m.convs))
	for _, c := range m.convs {
		out = append(out, m.summaryLocked(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated.Equal(out[j].Updated) {
			return out[i].ID < out[j].ID
		}
		return out[i].Updated.After(out[j].Updated)
	})
	return out
}

func (m *Manager) summaryLocked(c *history.Conversation) Summary {
	title := c.AgentKey
	if e, ok := agents.Find(c.AgentKey); ok {
		title = e.Title
	}
	_, running := m.active[c.ID]
	branch := c.Current()
	s := Summary{
		ID:              c.ID,
		Title:           c.Title,
		AgentKey:        c.AgentKey,
		AgentTitle:      title,
		Model:           c.Model,
		Created:         c.Created,
		Updated:         c.Updated,
		Turns:           len(c.Turns()),
		Messages:        len(c.Messages()),
		Runes:           c.Runes(),
		Totals:          c.Totals(),
		Running:         running,
		Path:            m.store.DisplayPath(c.ID),
		Strategy:        c.Strategy,
		Group:           c.Group,
		Branches:        len(c.Branches),
		CheckpointCount: len(c.Checkpoints),
	}
	if branch != nil {
		s.Facts = factsInfo(branch.Facts)
		s.BranchID = branch.ID
		s.BranchName = branch.Name
	}
	return s
}

// detailLocked — диалог целиком по клону: наружу не должно уйти ничего,
// что дописывает идущий ход.
func (m *Manager) detailLocked(c *history.Conversation) Detail {
	clone := c.Clone()
	d := Detail{
		Summary:     m.summaryLocked(c),
		Messages:    clone.Messages(),
		Turns:       clone.Turns(),
		FactsCard:   clone.Facts(),
		Tree:        treeOf(clone),
		Checkpoints: clone.Checkpoints,
	}
	if d.Checkpoints == nil {
		d.Checkpoints = []history.Checkpoint{}
	}
	if s, running := m.active[c.ID]; running {
		v := s.View()
		d.Active = &v
	}
	return d
}

// treeOf — ветки диалога для интерфейса, в порядке появления.
func treeOf(c *history.Conversation) []BranchInfo {
	out := make([]BranchInfo, 0, len(c.Branches))
	for _, b := range c.Branches {
		var totals history.Totals
		for _, t := range b.Turns {
			totals = totals.Add(t.Totals)
		}
		out = append(out, BranchInfo{
			ID: b.ID, Name: b.Name, Parent: b.Parent,
			ForkAt: b.ForkAt, ForkTurn: b.ForkTurn, Created: b.Created,
			Active:       b.ID == c.Active,
			Turns:        len(b.Turns),
			Messages:     len(b.Messages),
			PathTurns:    len(c.PathTurns(b.ID)),
			PathMessages: len(c.Path(b.ID)),
			Totals:       totals,
			Facts:        factsInfo(b.Facts),
		})
	}
	return out
}

// Get — диалог целиком.
func (m *Manager) Get(id string) (Detail, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.convs[id]
	if !ok {
		return Detail{}, false
	}
	return m.detailLocked(c), true
}

// Turn — ход по идентификатору, для потока событий.
func (m *Manager) Turn(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.turns[id]
	return s, ok
}

// Start создаёт диалог со стратегией по умолчанию и делает в нём первый
// ход.
func (m *Manager) Start(agentKey, text string) (*Session, error) {
	return m.StartWith(agentKey, text, "")
}

// StartWith создаёт диалог с заданной стратегией. Пустая означает
// настройку сервера: стратегия запоминается в диалоге, потому что дальше
// её можно переключить, а после перезапуска диалог должен продолжаться по
// своим правилам.
func (m *Manager) StartWith(agentKey, text, mode string) (*Session, error) {
	entry, text, err := m.prepare(agentKey, text)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = m.deps.Strategy.Normalized().Mode
	}
	if !strategy.Valid(mode) {
		return nil, fmt.Errorf("неизвестная стратегия %q", mode)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	c := history.New(entry.Key, m.deps.Runner.Model, mode)
	m.convs[c.ID] = c
	return m.sendLocked(c, entry, text)
}

// prepare проверяет общее для всех запусков: тип агента и непустой текст.
func (m *Manager) prepare(agentKey, text string) (agents.Entry, string, error) {
	entry, ok := agents.Find(agentKey)
	if !ok {
		return agents.Entry{}, "", fmt.Errorf("неизвестный тип агента: %q", agentKey)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return agents.Entry{}, "", errors.New("сообщение пустое")
	}
	return entry, text, nil
}

// StartComparison заводит стенд: по диалогу на каждую стратегию. Дальше им
// задают одни и те же вопросы, и разница в ответах — это разница памяти, а
// не разных разговоров.
func (m *Manager) StartComparison(agentKey, text string) (string, []*Session, error) {
	entry, text, err := m.prepare(agentKey, text)
	if err != nil {
		return "", nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	group := history.NewID()
	sessions := make([]*Session, 0, len(ComparisonModes))
	for _, mode := range ComparisonModes {
		c := history.New(entry.Key, m.deps.Runner.Model, mode)
		c.Group = group
		m.convs[c.ID] = c
		s, err := m.sendLocked(c, entry, text)
		if err != nil {
			return "", nil, err
		}
		sessions = append(sessions, s)
	}
	return group, sessions, nil
}

// SendComparison задаёт следующий вопрос всем дорожкам стенда сразу.
// Если хоть одна ещё отвечает, ход не начинается ни в одной: дорожки
// должны идти в ногу, иначе сравнивать нечего.
func (m *Manager) SendComparison(group, text string) ([]*Session, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("сообщение пустое")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	convs := m.groupLocked(group)
	if len(convs) == 0 {
		return nil, ErrNotFound
	}
	for _, c := range convs {
		if _, running := m.active[c.ID]; running {
			return nil, ErrBusy
		}
	}

	sessions := make([]*Session, 0, len(convs))
	for _, c := range convs {
		entry, ok := agents.Find(c.AgentKey)
		if !ok {
			return nil, fmt.Errorf("диалог вёл агент %q, которого больше нет", c.AgentKey)
		}
		s, err := m.sendLocked(c, entry, text)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// Comparison — стенд целиком: дорожки в порядке ComparisonModes.
func (m *Manager) Comparison(group string) (Comparison, bool) {
	m.mu.Lock()
	convs := m.groupLocked(group)
	details := make([]Detail, 0, len(convs))
	for _, c := range convs {
		details = append(details, m.detailLocked(c))
	}
	m.mu.Unlock()

	if len(details) == 0 {
		return Comparison{}, false
	}
	cmp := Comparison{Group: group, Lanes: details, Created: details[0].Created, Title: details[0].Title}
	for _, d := range details {
		if d.Updated.After(cmp.Updated) {
			cmp.Updated = d.Updated
		}
		if d.Title != "" && cmp.Title == "" {
			cmp.Title = d.Title
		}
	}
	return cmp, true
}

// groupLocked — диалоги стенда в порядке стратегий: дорожки на экране не
// должны меняться местами от загрузки к загрузке.
func (m *Manager) groupLocked(group string) []*history.Conversation {
	if group == "" {
		return nil
	}
	byMode := make(map[string]*history.Conversation)
	var rest []*history.Conversation
	for _, c := range m.convs {
		if c.Group != group {
			continue
		}
		if _, taken := byMode[c.Strategy]; !taken && c.Strategy != "" {
			byMode[c.Strategy] = c
			continue
		}
		rest = append(rest, c)
	}
	out := make([]*history.Conversation, 0, len(byMode)+len(rest))
	for _, mode := range ComparisonModes {
		if c, ok := byMode[mode]; ok {
			out = append(out, c)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Created.Before(rest[j].Created) })
	return append(out, rest...)
}

// Send продолжает диалог: следующий ход в текущей ветке с тем же типом
// агента.
func (m *Manager) Send(conversationID, text string) (*Session, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("сообщение пустое")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.convs[conversationID]
	if !ok {
		return nil, ErrNotFound
	}
	if _, running := m.active[c.ID]; running {
		return nil, ErrBusy
	}
	entry, ok := agents.Find(c.AgentKey)
	if !ok {
		return nil, fmt.Errorf("диалог вёл агент %q, которого больше нет", c.AgentKey)
	}
	return m.sendLocked(c, entry, text)
}

/* ---------- Ветвление ---------- */

// Mark ставит точку сохранения на конце текущей ветки. Точка — это место,
// от которого потом расходятся ветки: она запоминает и длину пути, и
// карточку фактов на этот момент.
func (m *Manager) Mark(conversationID, name string) (Detail, history.Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.convs[conversationID]
	if !ok {
		return Detail{}, history.Checkpoint{}, ErrNotFound
	}
	if _, running := m.active[c.ID]; running {
		return Detail{}, history.Checkpoint{}, ErrBusy
	}
	cp, err := c.Mark(c.Active, name)
	if err != nil {
		return Detail{}, history.Checkpoint{}, err
	}
	if err := m.store.Save(c); err != nil {
		return Detail{}, history.Checkpoint{}, err
	}
	return m.detailLocked(c), cp, nil
}

// Fork заводит ветку от точки сохранения и сразу переводит разговор в
// неё: ветвятся, чтобы продолжить по-другому, а не чтобы посмотреть.
// Пустой checkpointID означает «от текущего конца ветки»: точка ставится
// сама, иначе ветвление требовало бы двух действий подряд.
func (m *Manager) Fork(conversationID, checkpointID, name string) (Detail, *history.Branch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.convs[conversationID]
	if !ok {
		return Detail{}, nil, ErrNotFound
	}
	if _, running := m.active[c.ID]; running {
		return Detail{}, nil, ErrBusy
	}
	if strings.TrimSpace(checkpointID) == "" {
		cp, err := c.Mark(c.Active, "")
		if err != nil {
			return Detail{}, nil, err
		}
		checkpointID = cp.ID
	}
	b, err := c.Fork(checkpointID, name)
	if err != nil {
		return Detail{}, nil, err
	}
	if err := c.Switch(b.ID); err != nil {
		return Detail{}, nil, err
	}
	if err := m.store.Save(c); err != nil {
		return Detail{}, nil, err
	}
	return m.detailLocked(c), b, nil
}

// Switch переводит разговор в другую ветку.
func (m *Manager) Switch(conversationID, branchID string) (Detail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.convs[conversationID]
	if !ok {
		return Detail{}, ErrNotFound
	}
	if _, running := m.active[c.ID]; running {
		return Detail{}, ErrBusy
	}
	if err := c.Switch(branchID); err != nil {
		return Detail{}, err
	}
	if err := m.store.Save(c); err != nil {
		return Detail{}, err
	}
	return m.detailLocked(c), nil
}

// SetStrategy переключает стратегию работы с историей прямо посреди
// диалога. Сообщения при этом не трогаются: меняется только то, что из
// них уйдёт модели на следующем ходе. Карточка фактов остаётся в файле и
// снова заработает, если вернуться к стратегии facts.
func (m *Manager) SetStrategy(conversationID, mode string) (Detail, error) {
	if !strategy.Valid(mode) {
		return Detail{}, fmt.Errorf("неизвестная стратегия %q", mode)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.convs[conversationID]
	if !ok {
		return Detail{}, ErrNotFound
	}
	if _, running := m.active[c.ID]; running {
		return Detail{}, ErrBusy
	}
	c.Strategy = mode
	c.Updated = time.Now()
	if err := m.store.Save(c); err != nil {
		return Detail{}, err
	}
	return m.detailLocked(c), nil
}

// sendLocked запускает ход и уходит: HTTP-запрос не должен ждать модель.
func (m *Manager) sendLocked(c *history.Conversation, entry agents.Entry, text string) (*Session, error) {
	branch := c.Current()
	if branch == nil {
		return nil, fmt.Errorf("%w: в диалоге нет веток", history.ErrNoBranch)
	}
	path := c.Path(branch.ID)
	session := newSession(View{
		ID:             history.NewID(),
		ConversationID: c.ID,
		AgentKey:       entry.Key,
		Model:          c.Model,
		Started:        time.Now(),
		User:           text,
		History:        len(path),
		Branch:         branch.ID,
		BranchName:     branch.Name,
	})
	m.active[c.ID] = session
	m.turns[session.view.ID] = session
	m.order = append(m.order, session.view.ID)
	m.evictLocked()

	// Агент получает копию пути на момент старта: пока ход идёт, других
	// ходов в этом диалоге нет, а копия защищает от правок по ссылке.
	// Вместе с историей уходит карточка фактов ветки и весь диалог одной
	// лентой — последний нужен только для счёта: по нему видно, сколько
	// контекста снимает само ветвление.
	in := agent.Input{
		History: path,
		Facts:   branch.Facts.Clone(),
		User:    text,
		Turn:    len(c.PathTurns(branch.ID)) + 1,
		Branch:  branch.Name,
		Linear:  c.Linear(),
	}
	// Стратегия берётся из диалога: у дорожек стенда она своя, и после
	// перезапуска сервера каждая должна продолжаться по своим правилам.
	deps := m.deps
	if c.Strategy != "" {
		deps.Strategy.Mode = c.Strategy
	}
	go m.run(session, entry.Build(deps), in)
	return session, nil
}

func (m *Manager) run(session *Session, a agent.Agent, in agent.Input) {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()

	reply, err := a.Reply(ctx, in, session)
	session.finish(reply.Text, reply.Stats.Context, err)

	// Ход попадает в историю до закрытия подписчиков: интерфейс по событию
	// done перечитывает диалог и должен увидеть новые сообщения. Неудачный
	// ход записывается без сообщений: история хранит только завершённые.
	added := reply.Added
	if err != nil {
		added = nil
	}
	m.mu.Lock()
	c := m.convs[session.view.ConversationID]
	var saveErr error
	if c != nil {
		// Карточка сохраняется и после неудачного хода: сообщений он не
		// добавил, но факты могли обновиться до ошибки, и второй раз
		// платить за то же извлечение незачем.
		if err := c.Append(session.view.Branch, session.turn(), added, reply.Facts); err != nil {
			saveErr = err
		} else {
			saveErr = m.store.Save(c)
		}
	}
	delete(m.active, session.view.ConversationID)
	m.mu.Unlock()

	session.setSaveError(saveErr)
	session.closeSubs()
}

// Delete удаляет диалог из памяти и с диска. Пока в нём идёт ход, удалять
// нельзя: ход всё равно запишет файл заново.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.convs[id]; !ok {
		return ErrNotFound
	}
	if _, running := m.active[id]; running {
		return ErrBusy
	}
	if err := m.store.Delete(id); err != nil {
		return err
	}
	delete(m.convs, id)
	return nil
}

// Raw — файл диалога как есть.
func (m *Manager) Raw(id string) (string, []byte, error) {
	m.mu.Lock()
	_, ok := m.convs[id]
	m.mu.Unlock()
	if !ok {
		return "", nil, ErrNotFound
	}
	data, err := m.store.Raw(id)
	return m.store.DisplayPath(id), data, err
}

// evictLocked выбрасывает из памяти самые старые завершённые ходы.
func (m *Manager) evictLocked() {
	for len(m.order) > maxTurnsInMemory {
		id := m.order[0]
		s := m.turns[id]
		if s != nil && s.View().Status == StatusRunning {
			return
		}
		delete(m.turns, id)
		m.order = m.order[1:]
	}
}
