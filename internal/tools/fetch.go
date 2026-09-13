package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	fetchTimeout  = 30 * time.Second
	maxFetchBytes = 2 << 20

	// DefaultUserAgent — Википедия требует осмысленный User-Agent и режет
	// анонимные клиенты. Ссылка на репозиторий — по её правилам.
	DefaultUserAgent = "AITrenning_Task10/1.0 (https://github.com/AlexS8332/AITrenning_Task10)"
)

// Fetcher — HTTP GET с кэшем ответов в памяти. Агент в одном прогоне
// нередко спрашивает одно и то же дважды (поиск, потом чтение той же
// статьи из другого агента), а внешние источники не любят частых запросов.
type Fetcher struct {
	HTTP      *http.Client
	UserAgent string

	mu    sync.Mutex
	cache map[string][]byte
}

func NewFetcher() *Fetcher {
	return &Fetcher{
		HTTP:      &http.Client{Timeout: fetchTimeout},
		UserAgent: DefaultUserAgent,
		cache:     make(map[string][]byte),
	}
}

// GetJSON запрашивает URL и разбирает JSON-ответ в target.
func (f *Fetcher) GetJSON(ctx context.Context, url string, target any) error {
	data, err := f.Get(ctx, url)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("ответ %s не разобрался: %w", shortURL(url), err)
	}
	return nil
}

// Get возвращает тело ответа, из кэша или из сети.
func (f *Fetcher) Get(ctx context.Context, url string) ([]byte, error) {
	f.mu.Lock()
	data, ok := f.cache[url]
	f.mu.Unlock()
	if ok {
		return data, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("создание запроса: %w", err)
	}
	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "application/json")

	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("запрос %s: %w", shortURL(url), err)
	}
	defer resp.Body.Close()

	data, err = io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return nil, fmt.Errorf("чтение ответа %s: %w", shortURL(url), err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s ответил %s", shortURL(url), resp.Status)
	}

	f.mu.Lock()
	f.cache[url] = data
	f.mu.Unlock()
	return data, nil
}

// shortURL — адрес без параметров запроса: в ошибке важен источник, а не
// вся строка с процентами.
func shortURL(url string) string {
	if i := strings.Index(url, "?"); i > 0 {
		return url[:i]
	}
	return url
}
