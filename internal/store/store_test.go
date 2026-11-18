package store_test

import (
	"context"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

// Migration applies on a real PostgreSQL and Vietnamese text folds to ASCII for search (AC-H-008 groundwork).
func TestMigrateAndVietnameseFolding(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	var folded string
	if err := pool.QueryRow(ctx, `SELECT kch_unaccent('Xóa nhóm câu hỏi phải xác nhận, đường dẫn')`).Scan(&folded); err != nil {
		t.Fatal(err)
	}
	if want := "Xoa nhom cau hoi phai xac nhan, duong dan"; folded != want {
		t.Fatalf("folded = %q, want %q", folded, want)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN
		('project','principal','knowledge','task','lease','budget','budget_ledger','evidence','journal','blackboard')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("tables = %d, want 10", n)
	}
}
