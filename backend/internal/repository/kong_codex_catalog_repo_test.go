//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestKongCodexCatalogRepositoryEnsureCatalog(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewKongCodexCatalogRepository(db)
	ctx := context.Background()
	record := service.KongCodexCatalogRecord{SHA256: "abc", Slugs: []string{"gpt-6.1-sol", "gpt-6-luna"}, Body: []byte(`{"models":[]}`)}

	mock.ExpectQuery(`(?s)INSERT INTO kong_codex_catalogs .*ON CONFLICT \(sha256\) DO NOTHING.*RETURNING id`).
		WithArgs("abc", pq.Array(record.Slugs), len(record.Body), record.Body).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	id, err := repo.EnsureKongCodexCatalog(ctx, record)
	require.NoError(t, err)
	require.Equal(t, int64(7), id)

	// 已存在：插入不返回行，改按 sha256 取已有的 id。
	mock.ExpectQuery(`(?s)INSERT INTO kong_codex_catalogs`).
		WithArgs("abc", pq.Array(record.Slugs), len(record.Body), record.Body).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT id FROM kong_codex_catalogs WHERE sha256 = \$1`).WithArgs("abc").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	id, err = repo.EnsureKongCodexCatalog(ctx, record)
	require.NoError(t, err)
	require.Equal(t, int64(7), id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKongCodexCatalogRepositoryLoadAndLatest(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewKongCodexCatalogRepository(db)
	ctx := context.Background()

	records, err := repo.LoadKongCodexCatalogs(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, records)

	mock.ExpectQuery(`(?s)SELECT id, sha256, model_slugs, body\s+FROM kong_codex_catalogs\s+WHERE id = ANY\(\$1\)`).
		WithArgs(pq.Array([]int64{7})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "sha256", "model_slugs", "body"}).
			AddRow(int64(7), "abc", "{gpt-6.1-sol,gpt-6-luna}", []byte(`{"models":[]}`)))
	records, err = repo.LoadKongCodexCatalogs(ctx, []int64{7})
	require.NoError(t, err)
	require.Equal(t, []service.KongCodexCatalogRecord{{ID: 7, SHA256: "abc", Slugs: []string{"gpt-6.1-sol", "gpt-6-luna"}, Body: []byte(`{"models":[]}`)}}, records)

	first := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	last := first.Add(time.Hour)
	mock.ExpectQuery(`(?s)SELECT DISTINCT ON \(account_id\) account_id, catalog_id, plan_type, first_seen_at, last_seen_at.*WHERE client_version = \$1.*ORDER BY account_id, last_seen_at DESC, id DESC`).
		WithArgs("0.160.1").
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "catalog_id", "plan_type", "first_seen_at", "last_seen_at"}).
			AddRow(int64(3), int64(7), "pro", first, last))
	observations, err := repo.LatestKongCodexCatalogObservations(ctx, "0.160.1")
	require.NoError(t, err)
	require.Equal(t, []service.KongCodexCatalogObservation{{
		AccountID: 3, ClientVersion: "0.160.1", CatalogID: 7, PlanType: "pro", FirstSeenAt: first, LastSeenAt: last,
	}}, observations)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKongCodexCatalogRepositoryRecordSample(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := NewKongCodexCatalogRepository(db)
	at := time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)

	mock.ExpectExec(`(?s)WITH latest AS \(.*WHERE account_id = \$1 AND client_version = \$2.*LIMIT 1.*extended AS \(.*UPDATE kong_codex_catalog_observations o.*latest.catalog_id = \$3 AND latest.plan_type = \$4.*INSERT INTO kong_codex_catalog_observations.*WHERE NOT EXISTS \(SELECT 1 FROM extended\)`).
		WithArgs(int64(3), "0.160.1", int64(7), "pro", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.RecordKongCodexCatalogSample(context.Background(), service.KongCodexCatalogObservation{
		AccountID: 3, ClientVersion: "0.160.1", CatalogID: 7, PlanType: "pro", FirstSeenAt: at, LastSeenAt: at,
	}))
	require.NoError(t, mock.ExpectationsWereMet())
}
