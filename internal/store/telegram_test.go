package store

import (
	"altrdb-telegram-bot/migrations"
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUpdateReceiptMigrationKeepsLegacyReplayBoundary(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "legacy.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	all, err := migrations.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Version >= 6 {
			break
		}
		if _, err := st.DB().ExecContext(ctx, m.SQL); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `INSERT INTO schema_migrations VALUES (?, ?, ?)`, m.Version, m.Name, "2026-11-14T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveTelegramOffset(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{99, 100, 102, 101} {
		ok, err := st.ClaimTelegramUpdate(ctx, id)
		if err != nil || ok != (id > 100) {
			t.Fatalf("claim %d: %t, %v", id, ok, err)
		}
	}
}

func TestTelegramReceiptsAllowOutOfOrderAndDeduplicateAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	st, _ := newTestStoreAt(t, path)
	ctx := context.Background()
	for _, id := range []int64{101, 100} {
		ok, err := st.ClaimTelegramUpdate(ctx, id)
		if err != nil || !ok {
			t.Fatalf("claim %d: %t, %v", id, ok, err)
		}
	}
	if off, err := st.TelegramOffset(ctx); err != nil || off != 101 {
		t.Fatalf("offset: %d, %v", off, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ = newTestStoreAt(t, path)
	for _, id := range []int64{101, 100} {
		ok, err := st.ClaimTelegramUpdate(ctx, id)
		if err != nil || ok {
			t.Fatalf("replay %d: %t, %v", id, ok, err)
		}
	}
	if ok, err := st.ClaimTelegramUpdate(ctx, 99); err != nil || !ok {
		t.Fatalf("unseen lower update after restart: %t, %v", ok, err)
	}
}

func TestConcurrentTelegramReplayIsClaimedOnce(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	var claims atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.ClaimTelegramUpdate(ctx, 123)
			if err != nil {
				t.Error(err)
			}
			if ok {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("claims: %d", claims.Load())
	}
}
