package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Delivery is one webhook delivery, byte-exact.
type Delivery struct {
	ID          string
	Event       string
	Action      string
	SenderLogin string
	Payload     []byte
	ReceivedAt  time.Time
}

// StoreDelivery inserts the delivery and notifies; a delivery id already
// stored reports duplicate and changes nothing.
func (s *Store) StoreDelivery(ctx context.Context, d Delivery) (bool, error) {
	var dup bool
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `insert into webhook_deliveries (delivery_id, event, action, sender_login, payload, received_at)
			values ($1, $2, $3, $4, $5, $6) on conflict (delivery_id) do nothing`,
			d.ID, d.Event, d.Action, d.SenderLogin, d.Payload, d.ReceivedAt)
		if err != nil {
			return err
		}
		dup = tag.RowsAffected() == 0
		if dup {
			return nil
		}
		return notify(ctx, tx, "delivery:"+d.ID)
	})
	return dup, err
}

// UnprocessedDeliveries lists stored deliveries with no processing row.
func (s *Store) UnprocessedDeliveries(ctx context.Context) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `select d.delivery_id, d.event, d.action, d.sender_login, d.payload, d.received_at
		from webhook_deliveries d left join webhook_delivery_processings p on p.delivery_id = d.delivery_id
		where p.delivery_id is null order by d.received_at, d.delivery_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.Event, &d.Action, &d.SenderLogin, &d.Payload, &d.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LastDeliveryAt reports the most recent delivery's received_at, across
// every stored delivery whether processed or not. ok is false when none
// have arrived yet.
func (s *Store) LastDeliveryAt(ctx context.Context) (time.Time, bool, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `select max(received_at) from webhook_deliveries`).Scan(&at); err != nil {
		return time.Time{}, false, err
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return *at, true, nil
}

// RecordProcessing marks a delivery processed exactly once.
func (s *Store) RecordProcessing(ctx context.Context, deliveryID, result, detail string) error {
	_, err := s.pool.Exec(ctx, `insert into webhook_delivery_processings (delivery_id, result, detail) values ($1, $2, $3)`, deliveryID, result, detail)
	return err
}
