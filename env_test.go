package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnvFile кладёт .env-файл во временный каталог теста.
func writeEnvFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("не удалось записать файл: %v", err)
	}
	return path
}

func TestLoadEnvFile(t *testing.T) {
	// BOM в первой строке — обычное дело для файлов, сохранённых из
	// блокнота: без его чистки первый ключ читается с невидимым префиксом.
	const content = "\ufeffTEST_KEY=значение\n" +
		"# комментарий\n" +
		"\n" +
		"TEST_QUOTED=\"в кавычках\"\n" +
		"TEST_SPACED  =  с пробелами  \n" +
		"TEST_EMPTY=\n" +
		"мусор без знака равенства\n"

	for _, key := range []string{"TEST_KEY", "TEST_QUOTED", "TEST_SPACED", "TEST_EMPTY"} {
		os.Unsetenv(key)
		t.Cleanup(func() { os.Unsetenv(key) })
	}

	loadEnvFile(writeEnvFile(t, content))

	tests := []struct {
		key  string
		want string
	}{
		{key: "TEST_KEY", want: "значение"},
		{key: "TEST_QUOTED", want: "в кавычках"},
		{key: "TEST_SPACED", want: "с пробелами"},
		{key: "TEST_EMPTY", want: ""},
	}

	for _, tt := range tests {
		if got := os.Getenv(tt.key); got != tt.want {
			t.Errorf("%s = %q, ожидалось %q", tt.key, got, tt.want)
		}
	}
}

// Переменные окружения имеют приоритет: файл не должен затирать то, что
// пользователь задал явно.
func TestLoadEnvFileKeepsExistingValues(t *testing.T) {
	t.Setenv("TEST_EXISTING", "из окружения")

	loadEnvFile(writeEnvFile(t, "TEST_EXISTING=из файла\n"))

	if got := os.Getenv("TEST_EXISTING"); got != "из окружения" {
		t.Errorf("значение из окружения затёрто файлом: %q", got)
	}
}

// Отсутствие файла — норма: ключ может быть задан переменной окружения.
func TestLoadEnvFileMissing(t *testing.T) {
	loadEnvFile(filepath.Join(t.TempDir(), "нет-такого.env"))
}

// Файлы ищутся вверх по дереву каталогов: ключ может лежать рядом с проектом
// или уровнем выше, общий для нескольких проектов.
func TestLoadEnvFilesWalksUp(t *testing.T) {
	const key = "TEST_PARENT_KEY"

	os.Unsetenv(key)
	t.Cleanup(func() { os.Unsetenv(key) })

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte(key+"=сверху\n"), 0o600); err != nil {
		t.Fatalf("не удалось записать файл: %v", err)
	}

	nested := filepath.Join(root, "проект", "вложенный")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("не удалось создать каталоги: %v", err)
	}

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("не удалось узнать рабочий каталог: %v", err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatalf("не удалось сменить каталог: %v", err)
	}
	t.Cleanup(func() { os.Chdir(original) })

	loadEnvFiles()

	if got := os.Getenv(key); got != "сверху" {
		t.Errorf("%s = %q, ожидалось значение из родительского каталога", key, got)
	}
}

// Битая строка не должна ронять программу: остальные ключи читаются.
func TestLoadEnvFileSkipsBrokenLines(t *testing.T) {
	const key = "TEST_AFTER_BROKEN"

	os.Unsetenv(key)
	t.Cleanup(func() { os.Unsetenv(key) })

	loadEnvFile(writeEnvFile(t, "=без имени\n"+strings.Repeat("#", 100)+"\n"+key+"=ок\n"))

	if got := os.Getenv(key); got != "ок" {
		t.Errorf("%s = %q, ожидалось «ок»", key, got)
	}
}
