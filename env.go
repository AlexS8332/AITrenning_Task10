package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// loadEnvFiles подхватывает переменные из .env-файлов, если их нет в окружении.
// Файлы ищутся в рабочем каталоге и выше по дереву каталогов, поэтому ключ
// можно держать как рядом с проектом, так и в общем каталоге уровнем выше.
// Значения остаются в памяти процесса.
func loadEnvFiles() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}

	for {
		loadEnvFile(filepath.Join(dir, ".env.local"))
		loadEnvFile(filepath.Join(dir, ".env"))

		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

// loadEnvFile разбирает формат KEY=value; строки-комментарии начинаются с #.
// Уже заданные переменные окружения не перетираются.
func loadEnvFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return // файла нет — это обычная ситуация, ключ может быть в окружении
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimPrefix(scanner.Text(), "\ufeff") // BOM в первой строке
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" || value == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		os.Setenv(key, value)
	}

	// Файл мог оборваться на середине — например, строка длиннее 64 КБ.
	// Тихо пропустить нельзя: ключ окажется «не найден» без объяснений.
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "предупреждение: %s прочитан не полностью: %v\n", path, err)
	}
}
