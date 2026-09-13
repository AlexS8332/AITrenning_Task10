package llm

import "time"

// Цены DeepSeek V4 за миллион токенов в долларах, по
// https://api-docs.deepseek.com/quick_start/pricing (сентябрь 2026).
// Тариф зависит от времени суток: пиковый действует 01:00–04:00 и
// 06:00–10:00 UTC по будням, остальное время — непиковый, вдвое дешевле.
type rate struct {
	CacheHit  float64
	CacheMiss float64
	Output    float64
}

type priceTable struct {
	Peak    rate
	OffPeak rate
}

var prices = map[string]priceTable{
	"deepseek-v4-flash": {
		Peak:    rate{CacheHit: 0.014, CacheMiss: 0.44, Output: 1.32},
		OffPeak: rate{CacheHit: 0.007, CacheMiss: 0.22, Output: 0.66},
	},
	"deepseek-v4-pro": {
		Peak:    rate{CacheHit: 0.044, CacheMiss: 1.32, Output: 3.96},
		OffPeak: rate{CacheHit: 0.022, CacheMiss: 0.66, Output: 1.98},
	},
}

// Названия тарифов — уходят в интерфейс и отчёт.
const (
	TariffPeak    = "пиковый"
	TariffOffPeak = "непиковый"
)

// Cost — стоимость одного ответа. Known = false означает, что прайса для
// модели нет и считать нечего: ноль в этом случае врал бы.
type Cost struct {
	USD    float64 `json:"usd"`
	Tariff string  `json:"tariff"`
	Known  bool    `json:"known"`
}

// Add складывает стоимости. Неизвестная стоимость заражает сумму: если хоть
// один ответ не посчитан, итог тоже не посчитан.
func (c Cost) Add(o Cost) Cost {
	if !c.Known && c.USD == 0 && c.Tariff == "" {
		return o
	}
	if !o.Known && o.USD == 0 && o.Tariff == "" {
		return c
	}
	sum := Cost{USD: c.USD + o.USD, Known: c.Known && o.Known, Tariff: "смешанный"}
	if c.Tariff == o.Tariff {
		sum.Tariff = c.Tariff
	}
	return sum
}

// IsPeak — попадает ли момент в пиковые часы DeepSeek.
func IsPeak(at time.Time) bool {
	u := at.UTC()
	if u.Weekday() == time.Saturday || u.Weekday() == time.Sunday {
		return false
	}
	h := u.Hour()
	return (h >= 1 && h < 4) || (h >= 6 && h < 10)
}

// PriceOf считает стоимость расхода по прайсу модели на момент запроса.
// Запрос без разбивки на кэш/не кэш считается целиком по полной цене —
// это верхняя оценка, а не заниженная.
func PriceOf(model string, u Usage, at time.Time) Cost {
	table, ok := prices[model]
	if !ok {
		return Cost{}
	}

	r, tariff := table.OffPeak, TariffOffPeak
	if IsPeak(at) {
		r, tariff = table.Peak, TariffPeak
	}

	hit, miss := u.CacheHit, u.CacheMiss
	if hit+miss == 0 {
		miss = u.Prompt
	}

	usd := (float64(hit)*r.CacheHit + float64(miss)*r.CacheMiss + float64(u.Completion)*r.Output) / 1e6
	return Cost{USD: usd, Tariff: tariff, Known: true}
}
