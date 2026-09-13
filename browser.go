package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// openBrowser открывает интерфейс в браузере по умолчанию. Не получилось —
// не беда: адрес напечатан выше, его можно открыть руками.
func openBrowser(url string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		// rundll32 вместо «start»: у start нет отдельного исполняемого файла,
		// это встроенная команда cmd.exe.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "браузер не открылся (%v) — откройте %s вручную\n", err, url)
	}
}
