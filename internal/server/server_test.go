package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/AlexS8332/AITrenning_Task10/internal/agent"
	"github.com/AlexS8332/AITrenning_Task10/internal/agents"
	"github.com/AlexS8332/AITrenning_Task10/internal/history"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm"
	"github.com/AlexS8332/AITrenning_Task10/internal/llm/llmtest"
	"github.com/AlexS8332/AITrenning_Task10/internal/runs"
	"github.com/AlexS8332/AITrenning_Task10/internal/tools"
)

func newTestServer(t *testing.T, dir string, fake *llmtest.Fake) (*httptest.Server, *runs.Manager) {
	t.Helper()
	manager := runs.NewManager(agents.Deps{
		Runner: agent.Runner{LLM: fake, Model: "deepseek-v4-flash"},
		Tools:  tools.NewRegistry(),
	}, history.NewStore(dir), time.Minute)
	manager.Load()
	static := fstest.MapFS{"index.html": {Data: []byte("<!DOCTYPE html><title>t</title>")}}
	srv := httptest.NewServer(New(manager, static))
	t.Cleanup(srv.Close)
	return srv, manager
}

func echoFake() *llmtest.Fake {
	return &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		var users []string
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser {
				users = append(users, m.Content)
			}
		}
		return llmtest.Text("помню: " + strings.Join(users, " | ")), nil
	}}
}

func postJSON(t *testing.T, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

func getJSON(t *testing.T, url string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

// drain читает поток SSE до события done и возвращает имена событий.
func drain(t *testing.T, srv *httptest.Server, turnID string) []string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/turns/" + turnID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("тип потока: %s", ct)
	}
	var events []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			name := strings.TrimPrefix(line, "event: ")
			events = append(events, name)
			if name == "done" {
				return events
			}
		}
	}
	t.Fatal("поток закончился без done")
	return nil
}

func TestAgentsAndStatic(t *testing.T) {
	srv, manager := newTestServer(t, t.TempDir(), &llmtest.Fake{})

	_, body := getJSON(t, srv.URL+"/api/agents")
	if list, _ := body["agents"].([]any); len(list) != 3 || body["historyDir"] != manager.DisplayDir() {
		t.Errorf("каталог: %+v", body)
	}
	// Стратегия важна не меньше каталога: от неё зависит, что агент
	// вообще помнит, и интерфейс показывает её в шапке.
	if modes, _ := body["strategies"].([]any); len(modes) != len(runs.ComparisonModes) {
		t.Errorf("список стратегий: %+v", body["strategies"])
	}
	if _, ok := body["strategy"].(map[string]any); !ok {
		t.Errorf("настройки стратегии: %+v", body["strategy"])
	}

	resp, _ := http.Get(srv.URL + "/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("страница: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestValidation(t *testing.T) {
	srv, _ := newTestServer(t, t.TempDir(), &llmtest.Fake{})

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/conversations", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/conversations: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/conversations", `{"text":"  "}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("пустое сообщение: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/conversations", `{"text":"рысь","agent":"nope"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("неизвестный агент: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/conversations", `{bad`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("битый JSON: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/conversations/0123456789abcdef"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("отсутствующий диалог: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/conversations/0123456789abcdef/turns", `{"text":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("ход в отсутствующем диалоге: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/conversations/0123456789abcdef/file"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("файл отсутствующего диалога: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/conversations/0123456789abcdef/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("неизвестное действие: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/turns/missing"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("отсутствующий ход: %d", resp.StatusCode)
	}
	if resp, _ := getJSON(t, srv.URL+"/api/turns/missing/events"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("поток отсутствующего хода: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/conversations/0123456789abcdef", nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("удаление отсутствующего: %d", resp.StatusCode)
	}
}

func TestDialogAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newTestServer(t, dir, echoFake())

	// Первый ход создаёт диалог.
	resp, view := postJSON(t, srv.URL+"/api/conversations", `{"agent":"chat","text":"меня зовут Алекс"}`)
	if resp.StatusCode != http.StatusAccepted || view["status"] != "running" {
		t.Fatalf("создание диалога: %d %+v", resp.StatusCode, view)
	}
	convID, _ := view["conversationId"].(string)
	turnID, _ := view["id"].(string)
	events := drain(t, srv, turnID)
	// Модель в тесте отвечает мгновенно, и журнал целиком может прийти уже в
	// снимке; отдельные log-события в таком случае необязательны.
	if events[0] != "snapshot" || events[len(events)-1] != "done" || !contains(events, "state") {
		t.Errorf("события потока: %v", events)
	}

	// Снимок хода и диалог целиком.
	_, snap := getJSON(t, srv.URL+"/api/turns/"+turnID)
	if v, _ := snap["view"].(map[string]any); v["status"] != "done" || v["reply"] != "помню: меня зовут Алекс" {
		t.Errorf("снимок хода: %+v", snap)
	}
	_, d := getJSON(t, srv.URL+"/api/conversations/"+convID)
	if msgs, _ := d["messages"].([]any); len(msgs) != 2 || d["title"] != "меня зовут Алекс" || d["active"] != nil {
		t.Errorf("диалог: %+v", d)
	}

	// Второй ход в том же диалоге.
	resp, view = postJSON(t, srv.URL+"/api/conversations/"+convID+"/turns", `{"text":"как меня зовут?"}`)
	if resp.StatusCode != http.StatusAccepted || view["history"].(float64) != 2 {
		t.Fatalf("второй ход: %d %+v", resp.StatusCode, view)
	}
	drain(t, srv, view["id"].(string))

	_, list := getJSON(t, srv.URL+"/api/conversations")
	convs, _ := list["conversations"].([]any)
	if len(convs) != 1 || convs[0].(map[string]any)["turns"].(float64) != 2 {
		t.Errorf("список: %+v", list)
	}
	_, file := getJSON(t, srv.URL+"/api/conversations/"+convID+"/file")
	if !strings.HasSuffix(file["path"].(string), convID+".json") || !strings.Contains(file["json"].(string), "как меня зовут?") {
		t.Errorf("файл: %+v", file)
	}

	// Перезапуск: новый сервер на том же каталоге продолжает диалог.
	srv.Close()
	srv2, _ := newTestServer(t, dir, echoFake())
	_, d = getJSON(t, srv2.URL+"/api/conversations/"+convID)
	if msgs, _ := d["messages"].([]any); len(msgs) != 4 {
		t.Fatalf("после перезапуска: %+v", d)
	}
	// Ход прошлого запуска в памяти нового сервера не живёт.
	if resp, _ := getJSON(t, srv2.URL+"/api/turns/"+turnID); resp.StatusCode != http.StatusNotFound {
		t.Errorf("старый ход после перезапуска: %d", resp.StatusCode)
	}
	resp, view = postJSON(t, srv2.URL+"/api/conversations/"+convID+"/turns", `{"text":"а что я говорил?"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ход после перезапуска: %d %+v", resp.StatusCode, view)
	}
	drain(t, srv2, view["id"].(string))
	_, snap = getJSON(t, srv2.URL+"/api/turns/"+view["id"].(string))
	if v, _ := snap["view"].(map[string]any); v["reply"] != "помню: меня зовут Алекс | как меня зовут? | а что я говорил?" {
		t.Errorf("после перезапуска агент не помнит: %+v", snap["view"])
	}

	req, _ := http.NewRequest(http.MethodDelete, srv2.URL+"/api/conversations/"+convID, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("удаление: %d", resp.StatusCode)
	}
	_, list = getJSON(t, srv2.URL+"/api/conversations")
	if convs, _ := list["conversations"].([]any); len(convs) != 0 {
		t.Errorf("список после удаления: %+v", list)
	}
}

func TestBusyConflict(t *testing.T) {
	release := make(chan struct{})
	fake := &llmtest.Fake{Fn: func(req llm.Request) (llm.Response, error) {
		<-release
		return llmtest.Text("ок"), nil
	}}
	srv, _ := newTestServer(t, t.TempDir(), fake)
	_, view := postJSON(t, srv.URL+"/api/conversations", `{"agent":"chat","text":"раз"}`)
	convID := view["conversationId"].(string)

	if resp, _ := postJSON(t, srv.URL+"/api/conversations/"+convID+"/turns", `{"text":"два"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("ход во время хода: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/conversations/"+convID, nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("удаление во время хода: %d", resp.StatusCode)
	}
	_, d := getJSON(t, srv.URL+"/api/conversations/"+convID)
	if d["active"] == nil || d["running"] != true {
		t.Errorf("идущий ход не виден в диалоге: %+v", d)
	}
	close(release)
	drain(t, srv, view["id"].(string))
}

func TestWriteEventAndHelpers(t *testing.T) {
	var buf bytes.Buffer
	writeEvent(&buf, "state", `{"a":1}`)
	if buf.String() != "event: state\ndata: {\"a\":1}\n\n" {
		t.Errorf("формат SSE: %q", buf.String())
	}
	if got := mustJSON(map[string]any{"x": make(chan int)}); got != "{}" {
		t.Errorf("mustJSON на несериализуемом: %q", got)
	}
	if statusOf(runs.ErrBusy) != http.StatusConflict || statusOf(runs.ErrNotFound) != http.StatusNotFound || statusOf(http.ErrBodyNotAllowed) != http.StatusBadRequest {
		t.Errorf("коды ошибок")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestComparisonAPI(t *testing.T) {
	srv, _ := newTestServer(t, t.TempDir(), echoFake())

	resp, body := postJSON(t, srv.URL+"/api/comparisons", `{"agent":"chat","text":"расскажи про рысь"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("создание сравнения: %d %+v", resp.StatusCode, body)
	}
	group, _ := body["group"].(string)
	turns, _ := body["turns"].([]any)
	if group == "" || len(turns) != len(runs.ComparisonModes) {
		t.Fatalf("ответ: %+v", body)
	}
	for _, tv := range turns {
		view, _ := tv.(map[string]any)
		id, _ := view["id"].(string)
		drain(t, srv, id)
	}

	resp, body = getJSON(t, srv.URL+"/api/comparisons/"+group)
	lanes, _ := body["lanes"].([]any)
	if resp.StatusCode != http.StatusOK || len(lanes) != len(runs.ComparisonModes) {
		t.Fatalf("сравнение: %d %+v", resp.StatusCode, body)
	}
	for i, l := range lanes {
		lane, _ := l.(map[string]any)
		if lane["strategy"] != runs.ComparisonModes[i] || lane["group"] != group {
			t.Errorf("дорожка %d: %+v", i, lane)
		}
	}

	resp, body = postJSON(t, srv.URL+"/api/comparisons/"+group+"/turns", `{"text":"а чем она питается?"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("следующий вопрос: %d %+v", resp.StatusCode, body)
	}
	turns, _ = body["turns"].([]any)
	for _, tv := range turns {
		view, _ := tv.(map[string]any)
		drain(t, srv, view["id"].(string))
	}

	if resp, _ := getJSON(t, srv.URL+"/api/comparisons/нет-такого"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("несуществующее сравнение: %d", resp.StatusCode)
	}
	if resp, _ := postJSON(t, srv.URL+"/api/comparisons", `{"text":"  "}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("пустое сообщение: %d", resp.StatusCode)
	}
}

// Ветвление через API: точка, ветка от неё, переключение обратно и смена
// стратегии посреди диалога. Всё это — четыре запроса к одному диалогу, и
// каждый возвращает диалог целиком, чтобы интерфейсу не приходилось
// перечитывать его отдельно.
func TestBranchingAPI(t *testing.T) {
	srv, _ := newTestServer(t, t.TempDir(), echoFake())

	resp, body := postJSON(t, srv.URL+"/api/conversations", `{"agent":"analyst","text":"собираем ТЗ"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("создание диалога: %d %+v", resp.StatusCode, body)
	}
	id, _ := body["conversationId"].(string)
	drain(t, srv, body["id"].(string))

	// Точка сохранения.
	resp, body = postJSON(t, srv.URL+"/api/conversations/"+id+"/checkpoints", `{"name":"перед выбором"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("точка: %d %+v", resp.StatusCode, body)
	}
	cp, _ := body["checkpoint"].(map[string]any)
	if cp["name"] != "перед выбором" || cp["at"].(float64) != 2 {
		t.Fatalf("точка: %+v", cp)
	}
	conv, _ := body["conversation"].(map[string]any)
	if list, _ := conv["checkpoints"].([]any); len(list) != 1 {
		t.Errorf("точки в диалоге: %+v", conv["checkpoints"])
	}

	// Ветка от точки: разговор сразу переходит в неё.
	resp, body = postJSON(t, srv.URL+"/api/conversations/"+id+"/branches",
		`{"from":"`+cp["id"].(string)+`","name":"PWA"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ветка: %d %+v", resp.StatusCode, body)
	}
	if branch, _ := body["branch"].(string); branch == "" {
		t.Errorf("в ответе нет идентификатора ветки: %+v", body)
	}
	conv, _ = body["conversation"].(map[string]any)
	if conv["branchName"] != "PWA" || conv["branches"].(float64) != 2 {
		t.Fatalf("после ветвления: %+v", conv)
	}

	// Ход в ветке и возврат в корневую: модель видит разные пути.
	resp, body = postJSON(t, srv.URL+"/api/conversations/"+id+"/turns", `{"text":"делаем PWA"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ход в ветке: %d %+v", resp.StatusCode, body)
	}
	drain(t, srv, body["id"].(string))

	_, body = getJSON(t, srv.URL+"/api/conversations/"+id)
	tree, _ := body["tree"].([]any)
	if len(tree) != 2 {
		t.Fatalf("дерево: %+v", tree)
	}
	root, _ := tree[0].(map[string]any)
	resp, body = postJSON(t, srv.URL+"/api/conversations/"+id+"/switch",
		`{"branch":"`+root["id"].(string)+`"}`)
	if resp.StatusCode != http.StatusOK || body["branchId"] != root["id"] {
		t.Fatalf("переключение: %d %+v", resp.StatusCode, body)
	}
	if msgs, _ := body["messages"].([]any); len(msgs) != 2 {
		t.Errorf("в корневой ветке хода из ветки быть не должно: %d сообщений", len(msgs))
	}

	// Переключатель стратегий.
	resp, body = postJSON(t, srv.URL+"/api/conversations/"+id+"/strategy", `{"strategy":"window"}`)
	if resp.StatusCode != http.StatusOK || body["strategy"] != "window" {
		t.Fatalf("смена стратегии: %d %+v", resp.StatusCode, body)
	}

	// Ошибки: несуществующая ветка, точка и неизвестная стратегия.
	if resp, _ = postJSON(t, srv.URL+"/api/conversations/"+id+"/switch", `{"branch":"нет"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("переключение в несуществующую ветку: %d", resp.StatusCode)
	}
	if resp, _ = postJSON(t, srv.URL+"/api/conversations/"+id+"/branches", `{"from":"нет"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("ветвление от несуществующей точки: %d", resp.StatusCode)
	}
	if resp, _ = postJSON(t, srv.URL+"/api/conversations/"+id+"/strategy", `{"strategy":"summary"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("неизвестная стратегия: %d", resp.StatusCode)
	}
	if resp, _ = postJSON(t, srv.URL+"/api/conversations/0123456789abcdef/checkpoints", `{}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("точка в несуществующем диалоге: %d", resp.StatusCode)
	}
}
