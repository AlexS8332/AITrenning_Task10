// Package runs — диалоги в работе: менеджер держит загруженные диалоги,
// запускает ходы агентов и записывает историю после каждого хода. Ход
// в процессе — Session: состояние для интерфейса, журнал событий и
// подписчики SSE.
package runs

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/history"
)

// Статусы хода в работе. Завершённые ходы хранятся со статусами
// пакета history.
const (
	StatusRunning = "running"
	StatusDone    = history.TurnDone
	StatusFailed  = history.TurnFailed
)

// Сколько сообщений может копиться у медленного подписчика, прежде чем
// они начнут теряться. Журнал одного хода — десятки событий, не тысячи.
const subscriberBuffer = 512

// View — состояние хода для интерфейса. Журнал в него не входит: он уходит
// отдельными сообщениями.
type View struct {
	ID             string         `json:"id"`
	ConversationID string         `json:"conversationId"`
	AgentKey       string         `json:"agentKey"`
	Model          string         `json:"model"`
	Started        time.Time      `json:"started"`
	Status         string         `json:"status"`
	User           string         `json:"user"`
	Reply          string         `json:"reply,omitempty"`
	Error          string         `json:"error,omitempty"`
	Totals         history.Totals `json:"totals"`
	Events         int            `json:"events"`
	// History — сколько сообщений пути ветки агент получил перед этим.
	History int `json:"history"`
	// Branch — ветка, в которой идёт ход, BranchName — её имя. Ветку
	// запоминает ход, а не диалог: пока ход идёт, переключиться нельзя,
	// но запись должна лечь именно туда, откуда стартовала.
	Branch     string `json:"branch,omitempty"`
	BranchName string `json:"branchName,omitempty"`
	SaveError  string `json:"saveError,omitempty"`
	// Context — чем был занят контекст в этом ходе: оценка по частям, пик
	// внутри хода и факт из ответа модели. Заполняется по завершении хода.
	Context agent.Context `json:"context"`
}

// Message — одно сообщение подписчику: вид события SSE и его данные.
type Message struct {
	Event string
	Data  string
}

// Snapshot — что получает новый подписчик: состояние и весь журнал.
type Snapshot struct {
	View   View          `json:"view"`
	Events []agent.Event `json:"events"`
}

// Session — ход в работе. Реализует agent.Emitter: агент пишет в него
// журнал, а сессия рассылает его подписчикам.
type Session struct {
	mu     sync.Mutex
	view   View
	events []agent.Event
	subs   map[chan Message]struct{}
	closed bool
}

func newSession(view View) *Session {
	view.Status = StatusRunning
	return &Session{
		view: view,
		subs: make(map[chan Message]struct{}),
	}
}

// View — копия состояния.
func (s *Session) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view
}

// Events — копия журнала.
func (s *Session) Events() []agent.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Event(nil), s.events...)
}

// Log — событие от агента. Здесь ему присваиваются номер и время, по нему
// обновляются счётчики, и оно уходит подписчикам.
func (s *Session) Log(ev agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ev.Seq = len(s.events) + 1
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	s.events = append(s.events, ev)
	s.view.Events = len(s.events)

	switch {
	case ev.Usage != nil:
		s.view.Totals.LLMCalls++
		s.view.Totals.Usage = s.view.Totals.Usage.Add(*ev.Usage)
		if ev.Cost != nil {
			s.view.Totals.Cost = s.view.Totals.Cost.Add(*ev.Cost)
		}
	case ev.Kind == agent.EventToolCall:
		s.view.Totals.ToolCalls++
	}
	s.view.Totals.Seconds = time.Since(s.view.Started).Seconds()

	s.broadcastLocked(Message{Event: "log", Data: mustJSON(ev)})
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.view)})
}

// finish закрывает ход ответом или ошибкой. Счётчики контекста приходят
// от агента: события журнала знают токены отдельных запросов, а разбивку
// хода целиком — только он.
func (s *Session) finish(reply string, ctx agent.Context, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.view.Context = ctx
	s.view.Totals.Seconds = time.Since(s.view.Started).Seconds()
	if err != nil {
		s.view.Status = StatusFailed
		s.view.Error = err.Error()
	} else {
		s.view.Status = StatusDone
		s.view.Reply = reply
	}
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.view)})
}

// turn — запись хода для истории по текущему состоянию.
func (s *Session) turn() history.Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return history.Turn{
		ID:      s.view.ID,
		Started: s.view.Started,
		Status:  s.view.Status,
		Branch:  s.view.Branch,
		User:    s.view.User,
		Reply:   s.view.Reply,
		Error:   s.view.Error,
		Totals:  s.view.Totals,
		Context: s.view.Context,
		Events:  append([]agent.Event(nil), s.events...),
	}
}

func (s *Session) setSaveError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.view.SaveError = err.Error()
	} else {
		s.view.SaveError = ""
	}
	s.broadcastLocked(Message{Event: "state", Data: mustJSON(s.view)})
}

// Subscribe возвращает снимок на момент подписки, канал последующих
// сообщений и функцию отписки. Канал закрывается, когда ход завершён и
// история записана: для SSE это сигнал закончить поток.
func (s *Session) Subscribe() (Snapshot, <-chan Message, func()) {
	ch := make(chan Message, subscriberBuffer)

	s.mu.Lock()
	defer s.mu.Unlock()

	snap := Snapshot{View: s.view, Events: append([]agent.Event(nil), s.events...)}
	if s.closed {
		close(ch)
		return snap, ch, func() {}
	}
	s.subs[ch] = struct{}{}

	return snap, ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// broadcastLocked рассылает сообщение без ожидания: медленный подписчик
// теряет сообщения, но не задерживает агента.
func (s *Session) broadcastLocked(msg Message) {
	for ch := range s.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *Session) closeSubs() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	for ch := range s.subs {
		delete(s.subs, ch)
		close(ch)
	}
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
