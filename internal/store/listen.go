package store

import (
	"context"
	"log"
	"time"
)

// Listen blocks until ctx ends, delivering every autophage_events payload to
// fn. ready, when non-nil, is called once the LISTEN statement has succeeded
// on each (re)connection, so a caller can wait for the subscription to be
// live before doing anything a missed notification would need to cover. A
// lost connection is re-acquired with backoff; notifications sent while
// disconnected are lost, which is why every consumer also scans its input
// tables on wake.
func (s *Store) Listen(ctx context.Context, ready func(), fn func(payload string)) error {
	backoff := time.Second
	for {
		err := s.listenOnce(ctx, ready, fn)
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

func (s *Store) listenOnce(ctx context.Context, ready func(), fn func(string)) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "listen autophage_events"); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}
