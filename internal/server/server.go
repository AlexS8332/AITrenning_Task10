// Package server — HTTP API и раздача фронтенда. Обработчики ничего не
// знают об агентах: они создают диалоги и ходы, отдают состояние и журнал.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agents"
	"github.com/AlexS8332/AITrenning_Task10/internal/history"
	"github.com/AlexS8332/AITrenning_Task10/internal/runs"
	"github.com/AlexS8332/AITrenning_Task10/internal/strategy"
)

const (
	maxRequestBody = 64 << 10
	// keepAlive держит поток живым: ход молчит, пока модель думает, а
	// некоторые прокси рвут молчащее соединение.
	keepAliveInterval = 20 * time.Second
)

type Server struct {
	runs    *runs.Manager
	mux     *http.ServeMux
	started time.Time
}

// New собирает сервер. static — каталог фронтенда (обычно встроенный).
func New(manager *runs.Manager, static fs.FS) *Server {
	s := &Server{runs: manager, mux: http.NewServeMux(), started: time.Now()}
	s.mux.Handle("/", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("/api/agents", s.handleAgents)
	s.mux.HandleFunc("/api/conversations", s.handleConversations)
	s.mux.HandleFunc("/api/conversations/", s.handleConversation)
	s.mux.HandleFunc("/api/comparisons", s.handleComparisons)
	s.mux.HandleFunc("/api/comparisons/", s.handleComparison)
	s.mux.HandleFunc("/api/turns/", s.handleTurn)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleAgents отдаёт каталог типов агентов, каталог истории, стратегии
// работы с контекстом и время старта сервера: по нему интерфейс отмечает
// в ленте, между какими ходами был перезапуск. Стратегия важна так же,
// как каталог: от неё зависит, что агент вообще помнит.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"agents":        agents.Infos(),
		"historyDir":    s.runs.DisplayDir(),
		"serverStarted": s.started,
		"strategy":      s.runs.Strategy(),
		"strategies":    strategy.Modes(),
	})
}

type messageRequest struct {
	Agent string `json:"agent"`
	Text  string `json:"text"`
	// Strategy — стратегия нового диалога; пусто — та, с которой запущен
	// сервер. У стенда не спрашивается: там по диалогу на каждую.
	Strategy string `json:"strategy"`
}

// branchRequest — тело запросов о ветвлении и стратегии. Одно на все
// четыре: полей мало, и каждому запросу нужно своё подмножество.
type branchRequest struct {
	// Name — имя точки или ветки, From — точка, от которой ветвиться
	// (пусто — от текущего конца), Branch — куда переключиться,
	// Strategy — на какую стратегию перейти.
	Name     string `json:"name"`
	From     string `json:"from"`
	Branch   string `json:"branch"`
	Strategy string `json:"strategy"`
}

// handleConversations: GET — список диалогов, POST — новый диалог с
// первым сообщением.
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"conversations": s.runs.List()})
	case http.MethodPost:
		var body messageRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(body.Text) == "" {
			writeError(w, http.StatusBadRequest, "сообщение не введено")
			return
		}
		if body.Agent == "" {
			body.Agent = agents.Catalog()[0].Key
		}
		session, err := s.runs.StartWith(body.Agent, body.Text, body.Strategy)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, session.View())
	default:
		writeError(w, http.StatusMethodNotAllowed, "нужен GET или POST")
	}
}

// handleComparisons: POST — новое сравнение. Один и тот же вопрос уходит
// сразу нескольким агентам, отличающимся только тем, что они помнят;
// в ответе — начатые ходы, по одному на дорожку.
func (s *Server) handleComparisons(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "нужен POST")
		return
	}
	var body messageRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, "сообщение не введено")
		return
	}
	if body.Agent == "" {
		body.Agent = agents.Catalog()[0].Key
	}
	group, sessions, err := s.runs.StartComparison(body.Agent, body.Text)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"group": group, "turns": views(sessions)})
}

// handleComparison разбирает /api/comparisons/{group}[/turns].
func (s *Server) handleComparison(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/comparisons/")
	group, action, _ := strings.Cut(rest, "/")

	switch {
	case action == "" && r.Method == http.MethodGet:
		cmp, ok := s.runs.Comparison(group)
		if !ok {
			writeError(w, http.StatusNotFound, "сравнение не найдено")
			return
		}
		writeJSON(w, http.StatusOK, cmp)
	case action == "turns" && r.Method == http.MethodPost:
		var body messageRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(body.Text) == "" {
			writeError(w, http.StatusBadRequest, "сообщение не введено")
			return
		}
		sessions, err := s.runs.SendComparison(group, body.Text)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"group": group, "turns": views(sessions)})
	default:
		writeError(w, http.StatusNotFound, "неизвестный запрос")
	}
}

// views — состояния начатых ходов в порядке дорожек.
func views(sessions []*runs.Session) []runs.View {
	out := make([]runs.View, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.View())
	}
	return out
}

// handleConversation разбирает /api/conversations/{id}[/turns|/file].
func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/conversations/")
	id, action, _ := strings.Cut(rest, "/")

	switch {
	case action == "" && r.Method == http.MethodGet:
		d, ok := s.runs.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "диалог не найден")
			return
		}
		writeJSON(w, http.StatusOK, d)
	case action == "" && r.Method == http.MethodDelete:
		if err := s.runs.Delete(id); err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	case action == "turns" && r.Method == http.MethodPost:
		var body messageRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(body.Text) == "" {
			writeError(w, http.StatusBadRequest, "сообщение не введено")
			return
		}
		session, err := s.runs.Send(id, body.Text)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, session.View())
	case action == "checkpoints" && r.Method == http.MethodPost:
		var body branchRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		d, cp, err := s.runs.Mark(id, body.Name)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"checkpoint": cp, "conversation": d})
	case action == "branches" && r.Method == http.MethodPost:
		var body branchRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		d, b, err := s.runs.Fork(id, body.From, body.Name)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"branch": b.ID, "name": b.Name, "conversation": d})
	case action == "switch" && r.Method == http.MethodPost:
		var body branchRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		d, err := s.runs.Switch(id, body.Branch)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, d)
	case action == "strategy" && r.Method == http.MethodPost:
		var body branchRequest
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		d, err := s.runs.SetStrategy(id, body.Strategy)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, d)
	case action == "file" && r.Method == http.MethodGet:
		path, data, err := s.runs.Raw(id)
		if err != nil {
			writeError(w, statusOf(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "json": string(data)})
	default:
		writeError(w, http.StatusNotFound, "неизвестный запрос")
	}
}

// handleTurn разбирает /api/turns/{id} и /api/turns/{id}/events.
func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/turns/")
	id, action, _ := strings.Cut(rest, "/")

	session, ok := s.runs.Turn(id)
	if !ok {
		writeError(w, http.StatusNotFound, "ход не найден: возможно, сервер перезапускали; журнал завершённых ходов лежит в диалоге")
		return
	}

	switch action {
	case "":
		writeJSON(w, http.StatusOK, runs.Snapshot{View: session.View(), Events: session.Events()})
	case "events":
		s.stream(w, r, session)
	default:
		writeError(w, http.StatusNotFound, "неизвестное действие "+action)
	}
}

// stream отдаёт ход потоком Server-Sent Events: сначала снимок (состояние
// и весь журнал), затем события по мере поступления. Состояние и журнал
// идут разными типами событий, и фронтенд обновляет их независимо.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, session *runs.Session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "поток событий не поддерживается")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	snap, updates, unsubscribe := session.Subscribe()
	defer unsubscribe()

	writeEvent(w, "snapshot", mustJSON(snap))
	flusher.Flush()

	ping := time.NewTicker(keepAliveInterval)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case msg, ok := <-updates:
			if !ok {
				writeEvent(w, "state", mustJSON(session.View()))
				writeEvent(w, "done", "{}")
				flusher.Flush()
				return
			}
			writeEvent(w, msg.Event, msg.Data)
			flusher.Flush()
		}
	}
}

func statusOf(err error) int {
	switch {
	case errors.Is(err, runs.ErrNotFound),
		errors.Is(err, history.ErrNoBranch),
		errors.Is(err, history.ErrNoCheckpoint):
		return http.StatusNotFound
	case errors.Is(err, runs.ErrBusy):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("тело запроса не разобралось: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeEvent пишет одно SSE-сообщение. json.Marshal переводов строки не
// ставит, поэтому многострочные данные здесь невозможны по построению.
func writeEvent(w io.Writer, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
