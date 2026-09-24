package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// codex 票的票表。表结构见迁移 900（kong_ticket_cache）与 905（captured_at 索引）；设计见
// DESIGN-codex-ticket.md。纯 SQL，不走 ent。

// KongTicketRepository 实现 service.KongTicketStore。
type KongTicketRepository struct {
	db *sql.DB
}

// NewKongTicketRepository 创建仓储。
func NewKongTicketRepository(db *sql.DB) *KongTicketRepository {
	return &KongTicketRepository{db: db}
}

var _ service.KongTicketStore = (*KongTicketRepository)(nil)

// InsertObservedTicket 按 (账号, 模型, 原值) 去重写入一张被动收到的票。已存在时不动原行、返回 false：
// 首次收到的时刻才是保留期的起点，重复出现不该延长它。
func (r *KongTicketRepository) InsertObservedTicket(ctx context.Context, t service.KongObservedTicket, expiresAt time.Time) (bool, error) {
	var id int64
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO kong_ticket_cache
			(account_id, model, state, state_len, source, status, captured_at, expires_at, expires_at_source)
		 VALUES ($1, $2, $3, $4, 'observed', 'unverified', $5, $6, 'estimated')
		 ON CONFLICT (account_id, model, state) DO NOTHING
		 RETURNING id`,
		t.AccountID, t.Model, t.State, len(t.State), t.CapturedAt.UTC(), expiresAt.UTC()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert observed ticket: %w", err)
	}
	return true, nil
}

// DeleteTicketsCapturedBefore 删除一批首次收到时刻早于 cutoff 的票。
func (r *KongTicketRepository) DeleteTicketsCapturedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM kong_ticket_cache WHERE id IN (
			SELECT id FROM kong_ticket_cache WHERE captured_at < $1 ORDER BY captured_at LIMIT $2
		)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("delete tickets captured before cutoff: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete tickets rows affected: %w", err)
	}
	return n, nil
}
