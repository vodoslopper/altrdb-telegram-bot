package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ClaimTelegramUpdate deduplicates individual updates atomically with the
// resume offset. Dispatch order need not match update ID order.
func (s *Store) ClaimTelegramUpdate(ctx context.Context, id int64) (bool, error) {
	claimed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO telegram_update_receipts (update_id)
   SELECT ? WHERE ? > (SELECT COALESCE(MAX(replay_floor), 0) FROM telegram_offset)
   ON CONFLICT (update_id) DO NOTHING`, id, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO telegram_offset (id,last_update_id,updated_at) VALUES (1,?,?)
   ON CONFLICT (id) DO UPDATE SET last_update_id=MAX(telegram_offset.last_update_id,excluded.last_update_id), updated_at=excluded.updated_at`, id, s.now())
		claimed = err == nil
		return err
	})
	if err != nil {
		return false, fmt.Errorf("store: claim telegram update %d: %w", id, err)
	}
	return claimed, nil
}
