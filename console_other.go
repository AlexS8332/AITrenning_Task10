//go:build !windows

package main

// На не-Windows консоль и так в UTF-8.
func enableUTF8Console() {}
