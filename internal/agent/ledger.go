package agent

import (
	"sync"

	"github.com/guygrigsby/jess/ledger"
)

// captureRun wraps the durable ledger to learn the run id jess minted, so
// the attempt can record it before any tool runs.
type captureRun struct {
	ledger.DurableSink
	once  sync.Once
	mu    sync.Mutex
	runID string
	began func(string)
}

func (c *captureRun) see(e ledger.Event) {
	if e.RunID == "" {
		return
	}
	c.once.Do(func() {
		c.mu.Lock()
		c.runID = e.RunID
		c.mu.Unlock()
		if c.began != nil {
			c.began(e.RunID)
		}
	})
}

func (c *captureRun) Record(e ledger.Event) error {
	c.see(e)
	return c.DurableSink.Record(e)
}

func (c *captureRun) CommitAction(e ledger.Event) error {
	c.see(e)
	return c.DurableSink.CommitAction(e)
}

func (c *captureRun) RunID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runID
}
