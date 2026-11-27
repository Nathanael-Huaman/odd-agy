package main

import "os"

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr, cwd))
}
