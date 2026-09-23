package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// kongCodexCatalogRepository 是 Codex 目录版本存档的数据库访问（raw SQL，不走 ent，理由同 kong_ticket_repo.go）。
// 表见 migrations/909_kong_codex_catalog.sql。
type kongCodexCatalogRepository struct {
	db *sql.DB
}

// NewKongCodexCatalogRepository 创建目录存档的数据库仓储。
func NewKongCodexCatalogRepository(db *sql.DB) service.KongCodexCatalogRepository {
	return &kongCodexCatalogRepository{db: db}
}

func (r *kongCodexCatalogRepository) EnsureKongCodexCatalog(ctx context.Context, record service.KongCodexCatalogRecord) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO kong_codex_catalogs (sha256, model_slugs, body_size, body)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (sha256) DO NOTHING
		RETURNING id`,
		record.SHA256, pq.Array(record.Slugs), len(record.Body), record.Body,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = r.db.QueryRowContext(ctx, `SELECT id FROM kong_codex_catalogs WHERE sha256 = $1`, record.SHA256).Scan(&id)
	}
	return id, err
}

func (r *kongCodexCatalogRepository) LoadKongCodexCatalogs(ctx context.Context, ids []int64) ([]service.KongCodexCatalogRecord, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, sha256, model_slugs, body
		  FROM kong_codex_catalogs
		 WHERE id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []service.KongCodexCatalogRecord
	for rows.Next() {
		var record service.KongCodexCatalogRecord
		if err := rows.Scan(&record.ID, &record.SHA256, pq.Array(&record.Slugs), &record.Body); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (r *kongCodexCatalogRepository) LatestKongCodexCatalogObservations(ctx context.Context, clientVersion string) ([]service.KongCodexCatalogObservation, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT ON (account_id) account_id, catalog_id, plan_type, first_seen_at, last_seen_at
		  FROM kong_codex_catalog_observations
		 WHERE client_version = $1
		 ORDER BY account_id, last_seen_at DESC, id DESC`, clientVersion)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []service.KongCodexCatalogObservation
	for rows.Next() {
		observation := service.KongCodexCatalogObservation{ClientVersion: clientVersion}
		if err := rows.Scan(&observation.AccountID, &observation.CatalogID, &observation.PlanType,
			&observation.FirstSeenAt, &observation.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, observation)
	}
	return out, rows.Err()
}

// RecordKongCodexCatalogSample 在一条语句里完成"续上最新一段或插新段"。同一账号同一客户端版本的采样
// 由网关的准入串行化，这里不另加锁。
func (r *kongCodexCatalogRepository) RecordKongCodexCatalogSample(ctx context.Context, sample service.KongCodexCatalogObservation) error {
	_, err := r.db.ExecContext(ctx, `
		WITH latest AS (
			SELECT id, catalog_id, plan_type
			  FROM kong_codex_catalog_observations
			 WHERE account_id = $1 AND client_version = $2
			 ORDER BY last_seen_at DESC, id DESC
			 LIMIT 1
		), extended AS (
			UPDATE kong_codex_catalog_observations o
			   SET last_seen_at = GREATEST(o.last_seen_at, $5), samples = o.samples + 1
			  FROM latest
			 WHERE o.id = latest.id AND latest.catalog_id = $3 AND latest.plan_type = $4
			RETURNING o.id
		)
		INSERT INTO kong_codex_catalog_observations
			(account_id, client_version, catalog_id, plan_type, first_seen_at, last_seen_at, samples)
		SELECT $1, $2, $3, $4, $5, $5, 1
		 WHERE NOT EXISTS (SELECT 1 FROM extended)`,
		sample.AccountID, sample.ClientVersion, sample.CatalogID, sample.PlanType, sample.LastSeenAt)
	return err
}
