package main

import "os"

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	os.Exit(RunWithIO(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cwd))
}
