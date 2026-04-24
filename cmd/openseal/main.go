package main

import (
	"os"

	"github.com/axiom-studio/openseal/cmd/openseal/commands"
)

func main() {
	commands.Execute(os.Args[1:])
}