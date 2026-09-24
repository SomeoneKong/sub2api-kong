package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// codex 票的票表与账号指纹测试的记录。表结构见迁移 900（kong_ticket_cache、kong_fingerprint_probes）、
// 905（票表的 captured_at 索引）与 906（kong_fingerprint_tests、探测记录的 reported_model）；设计见
// DESIGN-codex-ticket.md 与 DESIGN-openai-fingerprint-test.md。纯 SQL，不走 ent。

// KongTicketRepository 实现 service.KongTicketStore 与 service.KongFingerprintTestStore。
type KongTicketRepository struct {
	db *sql.DB
}

// NewKongTicketRepository 创建仓储。
func NewKongTicketRepository(db *sql.DB) *KongTicketRepository {
	return &KongTicketRepository{db: db}
}

var (
	_ service.KongTicketStore          = (*KongTicketRepository)(nil)
	_ service.KongFingerprintTestStore = (*KongTicketRepository)(nil)
)

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

// InsertFingerprintTest 在测试开始时写一行；结束时由 FinishFingerprintTest 补齐。没收尾的行（进程在
// 测试中途退出）execution 为空。
func (r *KongTicketRepository) InsertFingerprintTest(ctx context.Context, rec *service.KongFingerprintTestRecord) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO kong_fingerprint_tests
			(verification_id, account_id, target_model, proxy_id, started_at, rule_version)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.VerificationID, rec.AccountID, rec.TargetModel, rec.ProxyID, rec.StartedAt.UTC(), rec.RuleVersion)
	if err != nil {
		return fmt.Errorf("insert fingerprint test: %w", err)
	}
	return nil
}

// FinishFingerprintTest 写入测试的结束时刻、执行结果、结束原因与模型结论。
func (r *KongTicketRepository) FinishFingerprintTest(ctx context.Context, rec *service.KongFingerprintTestRecord) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE kong_fingerprint_tests
		    SET finished_at = $2, execution = $3, end_reason = $4, verdict = $5
		  WHERE verification_id = $1`,
		rec.VerificationID, rec.FinishedAt.UTC(), rec.Execution, rec.EndReason, kongNullString(rec.Verdict))
	if err != nil {
		return fmt.Errorf("finish fingerprint test: %w", err)
	}
	return nil
}

// InsertFingerprintProbe 写入一份挑战的探测记录。编码失败时整份不写：这些记录的唯一用途是将来离线重算，
// 少了分数或版本标识就再也解释不了（例如 Scores 含 NaN 时 json.Marshal 必然失败）。
func (r *KongTicketRepository) InsertFingerprintProbe(ctx context.Context, p *service.KongFingerprintProbe) error {
	if p == nil {
		return nil
	}
	digits, err := json.Marshal(p.Digits)
	if err != nil {
		return fmt.Errorf("marshal digits: %w", err)
	}
	scores := []byte("null")
	if len(p.Scores) > 0 {
		if scores, err = json.Marshal(p.Scores); err != nil {
			return fmt.Errorf("marshal probe scores (part %d): %w", p.PartIndex, err)
		}
	}
	library := []byte("{}")
	if len(p.LibraryVersion) > 0 {
		if library, err = json.Marshal(p.LibraryVersion); err != nil {
			return fmt.Errorf("marshal probe library version (part %d): %w", p.PartIndex, err)
		}
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO kong_fingerprint_probes
			(verification_id, part_index, account_id, target_model, verify_egress, challenge_id,
			 digits, digit_count, scores, part_attribution, cum_probability, temperature_tier,
			 library_version, parse_valid, counted_in_average, invalid_reason, latency_ms, output_tokens,
			 reported_model)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		p.VerificationID, p.PartIndex, p.AccountID, p.TargetModel, kongNullString(p.VerifyEgress), p.ChallengeID,
		string(digits), p.DigitCount, string(scores), p.PartAttribution, p.CumProbability, p.TemperatureTier,
		string(library), p.ParseValid, p.CountedInAverage, p.InvalidReason, p.LatencyMs, p.OutputTokens,
		kongNullString(p.ReportedModel))
	if err != nil {
		return fmt.Errorf("insert probe part %d: %w", p.PartIndex, err)
	}
	return nil
}

// kongNullString 把空串写成 NULL：这几列的"没有"与"空串"是同一件事。
func kongNullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
