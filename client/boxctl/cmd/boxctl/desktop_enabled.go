//go:build windows || darwin

package main

import "boxctl/internal/desktop"

func runDesktop(args []string) error { return desktop.Main(args) }
