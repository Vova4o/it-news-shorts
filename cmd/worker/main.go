// Command worker will host the Temporal worker and the daily pipeline.
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

func main() {
	schedule := domain.MoscowSchedule()
	next := schedule.NextRun(time.Now())
	window := schedule.WindowFor(next)
	log.Printf("worker: no workflow engine yet (stage 1)")
	fmt.Printf("next run:     %s\n", next.Format(time.RFC3339))
	fmt.Printf("news window:  %s .. %s\n", window.Start.Format(time.RFC3339), window.End.Format(time.RFC3339))
}
