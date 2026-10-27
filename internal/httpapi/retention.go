package httpapi

import (
	"context"
	"log"
	"time"

	"hmoob-dock/internal/retention"
)

// retentionInterval is the time between two runs of the cleaner. The limits
// are days, so an hour is precise enough and keeps the write transactions of
// the cleaner away from the work of the administrator.
const retentionInterval = time.Hour

// startRetention removes the records that passed their limit, again and
// again, until the server stops.
//
// A failure is reported and the next run tries again: a database that is busy
// at one run must not stop the cleaner for good.
func (s *Server) startRetention() {
	cleaner := retention.New(s.db)
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			counts, err := cleaner.RunOnce(context.WithoutCancel(s.ctx), time.Now().UTC())
			if err != nil {
				log.Printf("control: the cleaner did not finish: %v", err)
				continue
			}
			if counts != (retention.Counts{}) {
				log.Printf("control: the cleaner removed %+v", counts)
			}
		}
	}
}
