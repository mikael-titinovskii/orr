package main

import (
	"fmt"
	"os"

	"github.com/mikael-titinovskii/orr/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "orr:", err)
		os.Exit(1)
	}
}
