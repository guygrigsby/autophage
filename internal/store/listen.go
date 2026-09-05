package store

import (
	"context"
	"log"
	"time"
)

// Listen blocks until ctx ends, delivering every autophage_events payload to
// fn. A lost connection is re-acquired with backoff; notifications sent while
// disconnected are lost, which is why every consumer also scans its input
// tables on wake.
func (s *Store) Listen(ctx context.Context, fn func(payload string)) error {
	backoff := time.Second
	for {
		err := s.listenOnce(ctx, fn)
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("store: listen: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *Store) listenOnce(ctx context.Context, fn func(string)) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "listen autophage_events"); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}
