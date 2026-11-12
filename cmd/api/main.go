// Command api will expose the read-only HTTP API of the service. Stage one
// only wires configuration-free startup, because the task forbids a web
// interface until the pipeline exists.
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
	log.Printf("api: no HTTP endpoints yet (stage 1); next daily run at %s", next.Format(time.RFC3339))
	fmt.Println("it-news-shorts api started in placeholder mode")
}
