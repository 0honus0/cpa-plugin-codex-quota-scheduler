package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jeffery/codex-quota-scheduler/internal/refactorgate"
)

func main() {
	root := flag.String("root", ".", "package root")
	entry := flag.String("entry", "handleSchedulerPick", "entry function")
	flag.Parse()
	violations, err := refactorgate.Analyze(*root, *entry)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(violations); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
