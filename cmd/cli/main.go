// Command cli offers operator helpers that do not need the pipeline.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

const version = "0.1.0-dev"

func main() {
	command := "help"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "schedule":
		printSchedule()
	case "youtube":
		runYouTube(os.Args[2:])
	case "infra":
		runInfra(os.Args[2:])
	case "news":
		runNews(os.Args[2:])
	case "editor":
		runEditor(os.Args[2:])
	case "version":
		fmt.Printf("it-news-shorts %s\n", version)
	case "help":
		fmt.Println("usage: cli [schedule|youtube|infra|news|editor|version]")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		fmt.Println("usage: cli [schedule|youtube|infra|news|editor|version]")
		os.Exit(1)
	}
}

func printSchedule() {
	schedule := domain.MoscowSchedule()
	next := schedule.NextRun(time.Now())
	for i := 0; i < domain.MaxStoriesPerRun; i++ {
		window := schedule.WindowFor(next)
		fmt.Printf("run %s: window %s .. %s\n",
			next.Format(time.RFC3339),
			window.Start.Format(time.RFC3339),
			window.End.Format(time.RFC3339))
		next = schedule.NextRun(next)
	}
}
