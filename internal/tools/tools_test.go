package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeWiki — подставная Википедия: поиск и статьи по заголовку.
func fakeWiki(t *testing.T) *httptest.Server {
	t.Helper()
	const lynxExtract = "Обыкновенная рысь (лат. Lynx lynx) — вид млекопитающих из рода рысей.\n\n\n== Внешний вид ==\nКрупная кошка.\n\n\n== Распространение ==\nЛесная зона Евразии.\n\n\n=== Подвиды ===\nНесколько подвидов.\n\n\n== Образ жизни, поведение и питание ==\nОхотится на зайцев.\n"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("запрос без User-Agent")
		}
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case q.Get("list") == "search":
			if q.Get("srsearch") == "ничего" {
				w.Write([]byte(`{"query":{"search":[]}}`))
				return
			}
			w.Write([]byte(`{"query":{"search":[
  {"title":"Рыси","snippet":"Обыкновенная <span class=\"searchmatch\">рысь</span> (Lynx lynx) &amp; другие"},
  {"title":"Обыкновенная рысь","snippet":"вид"}]}}`))
		case q.Get("prop") == "extracts":
			title := q.Get("titles")
			switch title {
			case "Рысь":
				resp := map[string]any{"query": map[string]any{
					"redirects": []map[string]string{{"from": "Рысь", "to": "Обыкновенная рысь"}},
					"pages":     []map[string]any{{"title": "Обыкновенная рысь", "extract": lynxExtract}},
				}}
				json.NewEncoder(w).Encode(resp)
			case "Обыкновенная рысь":
				json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{
					"pages": []map[string]any{{"title": "Обыкновенная рысь", "extract": lynxExtract}},
				}})
			default:
				json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{
					"pages": []map[string]any{{"title": title, "missing": true}},
				}})
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestWikipediaSearchStripsHTML(t *testing.T) {
	srv := fakeWiki(t)
	defer srv.Close()

	w := NewWikipedia(srv.URL, NewFetcher())
	hits, err := w.Search(context.Background(), "рысь")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Title != "Рыси" {
		t.Fatalf("результаты: %+v", hits)
	}
	if strings.Contains(hits[0].Snippet, "<") || !strings.Contains(hits[0].Snippet, "& другие") {
		t.Errorf("фрагмент не очищен: %q", hits[0].Snippet)
	}
}

func TestWikipediaArticleSectionsAndRedirect(t *testing.T) {
	srv := fakeWiki(t)
	defer srv.Close()

	w := NewWikipedia(srv.URL, NewFetcher())
	art, err := w.Article(context.Background(), "Рысь")
	if err != nil {
		t.Fatal(err)
	}
	if art.Title != "Обыкновенная рысь" || art.RedirectedFrom != "Рысь" {
		t.Errorf("заголовок %q, перенаправление с %q", art.Title, art.RedirectedFrom)
	}
	if !strings.HasPrefix(art.Intro, "Обыкновенная рысь (лат. Lynx lynx)") {
		t.Errorf("вступление: %q", art.Intro)
	}
	titles := art.SectionTitles()
	want := []string{"Внешний вид", "Распространение", "  Подвиды", "Образ жизни, поведение и питание"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Errorf("оглавление: %q", titles)
	}

	// Подраздел входит в текст родителя и остаётся отдельной записью.
	sec, ok := art.FindSection("распространение")
	if !ok || !strings.Contains(sec.Text, "Лесная зона") || !strings.Contains(sec.Text, "Несколько подвидов") {
		t.Errorf("раздел «Распространение»: %+v", sec)
	}
	sub, ok := art.FindSection("Подвиды")
	if !ok || sub.Level != 3 || strings.Contains(sub.Text, "Лесная зона") {
		t.Errorf("подраздел: %+v", sub)
	}
	// Подстрока находит длинный заголовок.
	diet, ok := art.FindSection("питание")
	if !ok || !strings.Contains(diet.Text, "зайцев") {
		t.Errorf("раздел о питании: %+v", diet)
	}
	if _, ok := art.FindSection("генетика"); ok {
		t.Errorf("несуществующий раздел найден")
	}
	if !strings.Contains(art.URL, "/wiki/") {
		t.Errorf("url: %q", art.URL)
	}
}

func TestWikipediaArticleMissing(t *testing.T) {
	srv := fakeWiki(t)
	defer srv.Close()

	_, err := NewWikipedia(srv.URL, NewFetcher()).Article(context.Background(), "Полосатый манул")
	if err == nil || !strings.Contains(err.Error(), "нет") {
		t.Fatalf("ожидалась ошибка отсутствия статьи, получено: %v", err)
	}
}

func TestWikipediaReadTool(t *testing.T) {
	srv := fakeWiki(t)
	defer srv.Close()

	w := NewWikipedia(srv.URL, NewFetcher())
	tool := w.Tools()[1]
	if tool.Name() != "read_wikipedia" {
		t.Fatalf("имя: %s", tool.Name())
	}

	out, err := tool.Call(context.Background(), json.RawMessage(`{"title":"Рысь"}`))
	if err != nil {
		t.Fatal(err)
	}
	var overview struct {
		Title    string   `json:"title"`
		Redirect string   `json:"redirected_from"`
		Intro    string   `json:"intro"`
		Sections []string `json:"sections"`
	}
	json.Unmarshal([]byte(out), &overview)
	if overview.Title != "Обыкновенная рысь" || overview.Redirect != "Рысь" || len(overview.Sections) != 4 || overview.Intro == "" {
		t.Errorf("обзор: %s", out)
	}

	out, err = tool.Call(context.Background(), json.RawMessage(`{"title":"Обыкновенная рысь","section":"Питание"}`))
	if err != nil {
		t.Fatal(err)
	}
	var section struct {
		Found   bool   `json:"found"`
		Section string `json:"section"`
		Text    string `json:"text"`
	}
	json.Unmarshal([]byte(out), &section)
	if !section.Found || section.Section != "Образ жизни, поведение и питание" || !strings.Contains(section.Text, "зайцев") {
		t.Errorf("раздел: %s", out)
	}

	out, _ = tool.Call(context.Background(), json.RawMessage(`{"title":"Обыкновенная рысь","section":"Генетика"}`))
	if !strings.Contains(out, `"found":false`) || !strings.Contains(out, "hint") {
		t.Errorf("отсутствующий раздел: %s", out)
	}

	if _, err := tool.Call(context.Background(), json.RawMessage(`{"title":""}`)); err == nil {
		t.Errorf("пустой заголовок должен быть ошибкой")
	}
}

func TestWikipediaSearchToolEmptyQuery(t *testing.T) {
	w := NewWikipedia("http://127.0.0.1:1", NewFetcher())
	if _, err := w.Tools()[0].Call(context.Background(), json.RawMessage(`{"query":" "}`)); err == nil {
		t.Errorf("пустой запрос должен быть ошибкой")
	}
}

// fakeGBIF — подставной GBIF: сверка, дерево, народные названия.
func fakeGBIF(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/species/match":
			switch r.URL.Query().Get("name") {
			case "Lynx lynx":
				w.Write([]byte(`{"usageKey":2435240,"scientificName":"Lynx lynx (Linnaeus, 1758)","canonicalName":"Lynx lynx","rank":"SPECIES","status":"ACCEPTED","confidence":99,"matchType":"EXACT","kingdom":"Animalia","class":"Mammalia","order":"Carnivora","family":"Felidae","genus":"Lynx","species":"Lynx lynx"}`))
			case "Lynx striatus":
				w.Write([]byte(`{"usageKey":2435239,"canonicalName":"Lynx","rank":"GENUS","status":"ACCEPTED","confidence":90,"matchType":"HIGHERRANK"}`))
			default:
				w.Write([]byte(`{"confidence":100,"matchType":"NONE","synonym":false}`))
			}
		case r.URL.Path == "/species/2435240/parents":
			w.Write([]byte(`[{"key":1,"rank":"KINGDOM","canonicalName":"Animalia"},{"key":359,"rank":"CLASS","canonicalName":"Mammalia"},{"key":2435239,"rank":"GENUS","canonicalName":"Lynx"}]`))
		case r.URL.Path == "/species/2435240":
			w.Write([]byte(`{"key":2435240,"rank":"SPECIES","canonicalName":"Lynx lynx"}`))
		case r.URL.Path == "/species/2435240/vernacularNames":
			w.Write([]byte(`{"results":[{"vernacularName":"Eurasian lynx","language":"eng"},{"vernacularName":"Обыкновенная рысь","language":"rus"},{"vernacularName":"обыкновенная рысь","language":"rus"},{"vernacularName":"Рысь","language":"rus"}]}`))
		case strings.HasPrefix(r.URL.Path, "/species/404"):
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestGBIFMatch(t *testing.T) {
	srv := fakeGBIF(t)
	defer srv.Close()
	g := NewGBIF(srv.URL, NewFetcher())

	m, err := g.Match(context.Background(), "Lynx lynx")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Found || m.UsageKey != 2435240 || m.Rank != "SPECIES" || m.Class != "Mammalia" {
		t.Errorf("совпадение: %+v", m)
	}

	higher, _ := g.Match(context.Background(), "Lynx striatus")
	if higher.Found || higher.Note == "" || higher.Canonical != "Lynx" {
		t.Errorf("совпадение по роду не должно считаться найденным: %+v", higher)
	}

	none, _ := g.Match(context.Background(), "Foobarus nonexistus")
	if none.Found || none.UsageKey != 0 || none.MatchType != "NONE" {
		t.Errorf("отсутствие: %+v", none)
	}
}

func TestGBIFTreeAndVernacular(t *testing.T) {
	srv := fakeGBIF(t)
	defer srv.Close()
	g := NewGBIF(srv.URL, NewFetcher())

	tree, err := g.Tree(context.Background(), 2435240)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 4 || tree[0].RankRu != "царство" || tree[3].Name != "Lynx lynx" || tree[3].RankRu != "вид" {
		t.Errorf("дерево: %+v", tree)
	}
	if _, err := g.Tree(context.Background(), 404); err == nil {
		t.Errorf("несуществующий таксон должен быть ошибкой")
	}

	names, err := g.Vernacular(context.Background(), 2435240, "rus")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, "|") != "Обыкновенная рысь|Рысь" {
		t.Errorf("народные названия: %q", names)
	}
}

func TestGBIFTools(t *testing.T) {
	srv := fakeGBIF(t)
	defer srv.Close()
	g := NewGBIF(srv.URL, NewFetcher())
	reg := NewRegistry(g.Tools()...)

	match, _ := reg.Get("match_taxon")
	out, err := match.Call(context.Background(), json.RawMessage(`{"scientific_name":"Lynx lynx"}`))
	if err != nil || !strings.Contains(out, `"found":true`) {
		t.Errorf("match_taxon: %s, %v", out, err)
	}
	if _, err := match.Call(context.Background(), json.RawMessage(`{"scientific_name":""}`)); err == nil {
		t.Errorf("пустое имя должно быть ошибкой")
	}

	tree, _ := reg.Get("taxon_tree")
	out, err = tree.Call(context.Background(), json.RawMessage(`{"usage_key":2435240}`))
	if err != nil || !strings.Contains(out, `"rank_ru":"царство"`) {
		t.Errorf("taxon_tree: %s, %v", out, err)
	}
	if _, err := tree.Call(context.Background(), json.RawMessage(`{"usage_key":0}`)); err == nil {
		t.Errorf("нулевой ключ должен быть ошибкой")
	}

	vern, _ := reg.Get("vernacular_names")
	out, err = vern.Call(context.Background(), json.RawMessage(`{"usage_key":2435240}`))
	if err != nil || !strings.Contains(out, "Обыкновенная рысь") || !strings.Contains(out, `"language":"rus"`) {
		t.Errorf("vernacular_names: %s, %v", out, err)
	}
}

func TestFetcherCachesAndReportsStatus(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	f := NewFetcher()
	var v map[string]bool
	for range 3 {
		if err := f.GetJSON(context.Background(), srv.URL+"/x?a=1", &v); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || !v["ok"] {
		t.Errorf("кэш не сработал: запросов %d", calls)
	}
	if err := f.GetJSON(context.Background(), srv.URL+"/bad", &v); err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("ожидалась ошибка статуса, получено: %v", err)
	}
}

func TestRegistryAndHelpers(t *testing.T) {
	echo := Func{FuncName: "echo", FuncDescription: "d", FuncParameters: json.RawMessage(`{"type":"object"}`),
		FuncCall: func(_ context.Context, args json.RawMessage) (string, error) { return string(args), nil }}
	reg := NewRegistry(echo)

	if got := reg.Names(); len(got) != 1 || got[0] != "echo" {
		t.Errorf("имена: %v", got)
	}
	if defs := Defs(reg.Pick("echo")); len(defs) != 1 || defs[0].Function.Name != "echo" {
		t.Errorf("описания: %+v", defs)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Errorf("Pick с неизвестным именем должен паниковать")
			}
		}()
		reg.Pick("nope")
	}()

	if got := Truncate("абвгд", 3); got != "абв …[обрезано]" {
		t.Errorf("Truncate: %q", got)
	}
	if got := Truncate("абв", 3); got != "абв" {
		t.Errorf("Truncate без обрезки: %q", got)
	}

	var in struct{ A int }
	if err := ParseArgs(nil, &in); err != nil {
		t.Errorf("пустые аргументы: %v", err)
	}
	if err := ParseArgs(json.RawMessage(`{bad`), &in); err == nil {
		t.Errorf("битые аргументы должны быть ошибкой")
	}
}

func TestSplitSectionsEmptyAndNoHeadings(t *testing.T) {
	intro, sections := splitSections("Просто текст без разделов.\n")
	if intro != "Просто текст без разделов." || len(sections) != 0 {
		t.Errorf("без заголовков: %q, %+v", intro, sections)
	}
	intro, sections = splitSections("")
	if intro != "" || len(sections) != 0 {
		t.Errorf("пусто: %q, %+v", intro, sections)
	}
}
