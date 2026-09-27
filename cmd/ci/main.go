package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: ci serve")
		os.Exit(2)
	}

	fmt.Fprintln(os.Stderr, "ci serve is not implemented yet")
	os.Exit(1)
}
