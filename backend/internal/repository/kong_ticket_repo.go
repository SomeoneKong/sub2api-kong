package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// kongNonEmpty 去掉空白项。空切片与「只含空串的切片」都表示该维度不过滤——否则
// `model = ANY('{""}')` 会筛成只剩空模型的事件，看起来像「一条都没有」。
func kongNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// kongTicketRepository 是 Codex 票据子系统的仓储（raw SQL）。
//
// 刻意不走 ent：上游把 ent 生成代码全部入库，新增 schema 会重新生成 ent/migrate/schema.go、
// ent/mutation.go 等上游每周改动数次的文件。表定义见 migrations/900_kong_codex_ticket.sql。
type kongTicketRepository struct {
	db *sql.DB
}

// NewKongTicketRepository 创建票据仓储。
func NewKongTicketRepository(db *sql.DB) service.KongTicketRepository {
	return &kongTicketRepository{db: db}
}

const kongTicketColumns = `id, account_id, model, state, state_len, source, status,
fingerprint_model, fingerprint_p, fingerprint_probs, skip_until_new, captured_at, expires_at,
expires_at_source, coalesce(capture_egress,''), capture_idle_seconds`

// kongMarshalProbs 把归因分布编成 JSONB 入参。
//
// 两个要点：
//   - 空分布写 NULL 而不是 `{}`。两者在采纳判据下同样是拒绝，但只有 NULL 能看出这张票根本没落过
//     分布，而不是落了一个空分布。
//   - **传 string 而不是 []byte**。lib/pq 对 []byte 参数的编码取决于推断出的类型 OID，写进 jsonb
//     列能不能成立要靠推断碰对；本仓库其余 jsonb 入参（事件 detail、探测的 digits / scores）一律
//     转成字符串，这里照同一口径。
func kongMarshalProbs(probs map[string]float64) (any, error) {
	if len(probs) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(probs)
	if err != nil {
		return nil, fmt.Errorf("encode fingerprint probs: %w", err)
	}
	return string(encoded), nil
}

func scanKongTicket(row interface{ Scan(...any) error }) (*service.KongTicket, error) {
	var t service.KongTicket
	var probs []byte
	err := row.Scan(&t.ID, &t.AccountID, &t.Model, &t.State, &t.StateLen, &t.Source, &t.Status,
		&t.FingerprintModel, &t.FingerprintP, &probs, &t.SkipUntilNew, &t.CapturedAt, &t.ExpiresAt,
		&t.ExpiresAtSource, &t.CaptureEgress, &t.CaptureIdleSeconds)
	if err != nil {
		return nil, err
	}
	// 分布解不开时当作「没有分布」而不是报错：那张票因此不会被采纳（求和恒为 0），调用方继续看
	// 下一张。让整条读取路径失败的代价大得多——一张坏行会把该账号该模型的每个请求都打掉。
	//
	// **但必须留下诊断**：这张票已经是 verified，而 OldestCandidate 只选 unverified，所以它不会
	// 被重新验证，只会静躺到过期。没有这条日志，「有票却一直判无票」就只能靠翻库排查。NULL 是
	// 正常的缺分布（未验证的票），不进这一支。
	if len(probs) > 0 {
		if err := json.Unmarshal(probs, &t.FingerprintProbs); err != nil {
			t.FingerprintProbs = nil
			slog.Warn("kong ticket fingerprint_probs 解码失败，该票按无分布处理",
				"ticket_id", t.ID, "account_id", t.AccountID, "model", t.Model, "error", err)
		}
	}
	return &t, nil
}

// VerifiedTickets 返回 verified 且未过期的票，按 expires_at 降序。
//
// **归因判据不在这里**——它在 service 侧的 KongTicketAccept 里，只有一份。原先这条 SQL 自带
// `fingerprint_model = $4 AND fingerprint_p >= $5`，而落结论时又在 Go 里判一次；采纳规则变成
// 「白名单内概率求和」之后，两处各写一遍就是两套规则，出入了也不会报错。
//
// verified + 未过期仍然要筛：verified 只代表「按当时的白名单与阈值判过」，那两个值来自环境
// 变量、可以改，所以调用方必须按当前配置重判。返回多张则让「较新但按当前白名单不合格的票」
// 不至于挡住较旧而合格的那张。
//
// **不能加 LIMIT**。截断发生在归因判定之前，于是「最新的 N 张都按当前白名单不合格、更早的一张
// 合格」这个合法状态会被读成无票：那张合格的票更早过期，等下去也永远进不了前 N 名。白名单收紧
// 之后就能构造出这个状态，后果是业务与管理面一致地误报无票，且不报错。行数由票表自身的清理与
// 留存上限约束，不该在这条查询里再截一刀。
func (r *kongTicketRepository) VerifiedTickets(ctx context.Context, accountID int64, model string) ([]*service.KongTicket, error) {
	// **必须筛掉 skip_until_new**：那个标记在一张已 verified 的票上只有一种来源——注入被上游拒绝
	// （它回发了新票，说明我们这张已经不作数）。不筛的话，一张已知注入无效的票仍会被当成当前票
	// 反复注入，而且会挡住更早那张仍然合格的票接替。
	query := `SELECT ` + kongTicketColumns + ` FROM kong_ticket_cache
		WHERE account_id = $1 AND model = $2 AND status = $3 AND expires_at > now()
		  AND skip_until_new = FALSE
		ORDER BY expires_at DESC`
	rows, err := r.db.QueryContext(ctx, query, accountID, model, service.KongTicketStatusVerified)
	if err != nil {
		return nil, fmt.Errorf("query verified tickets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*service.KongTicket
	for rows.Next() {
		t, scanErr := scanKongTicket(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan verified ticket: %w", scanErr)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verified tickets: %w", err)
	}
	return out, nil
}

// OldestCandidate 返回最早的一张可验证候选。
//
// minAge 只作用于 observed 票——fetch 票是我们自己刚要来的唯一一张，等待毫无意义，否则冷启动
// 要凭空多阻塞一个 minAge。skip_until_new 为真的候选不参与选择：它们是「未被上游接受」这个
// 终点留下的，档位没被证伪，但重选只会重复同一个无效尝试。
func (r *kongTicketRepository) OldestCandidate(ctx context.Context, accountID int64, model string, minAge time.Duration, now time.Time) (*service.KongTicket, error) {
	query := `SELECT ` + kongTicketColumns + ` FROM kong_ticket_cache
		WHERE account_id = $1 AND model = $2 AND status = $3
		  AND expires_at > $4 AND NOT skip_until_new
		  AND (source = $5 OR captured_at <= $6)
		ORDER BY captured_at ASC LIMIT 1`
	t, err := scanKongTicket(r.db.QueryRowContext(ctx, query,
		accountID, model, service.KongTicketStatusUnverified, now,
		service.KongTicketSourceFetch, now.Add(-minAge)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query oldest candidate: %w", err)
	}
	return t, nil
}

func (r *kongTicketRepository) InsertTicket(ctx context.Context, t *service.KongTicket) (int64, bool, error) {
	// ON CONFLICT DO NOTHING + 回查：靠唯一约束原子地区分「新票」与「重复票」。
	//
	// 先查再插会在并发下双双判为新票（两个事务都看不到对方未提交的行），于是重复票照样入库、
	// 期限被重算、跳过标记被当成新信息解除。
	var id int64
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO kong_ticket_cache
			(account_id, model, state, state_len, source, status,
			 fingerprint_model, fingerprint_p, skip_until_new,
			 captured_at, expires_at, expires_at_source,
			 capture_egress, capture_idle_seconds)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		 ON CONFLICT (account_id, model, state) DO NOTHING
		 RETURNING id`,
		t.AccountID, kongNullString(t.Model), t.State, t.StateLen, t.Source, t.Status,
		t.FingerprintModel, t.FingerprintP, t.SkipUntilNew,
		t.CapturedAt.UTC(), t.ExpiresAt.UTC(), t.ExpiresAtSource,
		kongNullString(t.CaptureEgress), t.CaptureIdleSeconds).Scan(&id)
	if err == nil {
		t.ID = id
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("insert ticket: %w", err)
	}

	// 冲突：已经有这张票。返回既有行，**不动**它的期限与状态——重复出现不该延长寿命，
	// 也不该把一张已被 rejected / skip 的票重新变成候选。
	err = r.db.QueryRowContext(ctx,
		`SELECT id FROM kong_ticket_cache WHERE account_id = $1 AND model = $2 AND state = $3`,
		t.AccountID, kongNullString(t.Model), t.State).Scan(&id)
	if err != nil {
		// 唯一约束刚刚拒绝了插入，这里却查不到——除了并发删除（过期清理）没有别的解释。
		return 0, false, fmt.Errorf("lookup existing ticket after conflict: %w", err)
	}
	t.ID = id
	return id, false, nil
}

// CommitVerification 在**同一个事务**里落「资格 + 最终事件」。
//
// 两件事必须同时可见。分两步写的话，状态先落库、事件写失败时资格已经露出去了：并发请求与下一个
// 请求都能从 CurrentTicket 取到这张票，而库里没有任何记录解释它为什么可用。事后补偿撤销也不行
// ——那期间的窗口正好是业务在用它。
//
// 返回值为假表示条件不满足（票已过期、已被改写或已被跳过），此时事务回滚、事件也不写，由调用方
// 另记一条作废事件。
func (r *kongTicketRepository) CommitVerification(
	ctx context.Context,
	id int64,
	status string,
	attr service.KongAttribution,
	event *service.KongTicketEvent,
) (bool, error) {
	probs, err := kongMarshalProbs(attr.Probs)
	if err != nil {
		return false, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin verification tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 条件里除了状态还要有未过期与未被跳过：注释一直写着「被跳过时不更新」，只比 status
	// 兑现不了它。
	//
	// 前置状态收 unverified **与 verified** 两种：前者是候选首次验证，后者是人工「立即验票」
	// 重验当前票——只收 unverified 的话那条路永远提交不上，于是每次重验都以「验证期间被改写」
	// 收尾，把一张其实合格的票判成失效。**rejected 仍然排除**：那是验证期间被撤销的信号，
	// 不能凭一个此前的结论把它复活。
	res, err := tx.ExecContext(ctx,
		`UPDATE kong_ticket_cache
		 SET status = $1, fingerprint_model = $2, fingerprint_p = $3, fingerprint_probs = $4
		 WHERE id = $5 AND status = ANY($6) AND skip_until_new = FALSE AND expires_at > now()`,
		status, attr.Model, attr.P, probs, id,
		pq.Array([]string{service.KongTicketStatusUnverified, service.KongTicketStatusVerified}))
	if err != nil {
		return false, fmt.Errorf("commit verification status: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("commit verification rows: %w", err)
	}
	if affected == 0 {
		return false, nil
	}
	if err := insertTicketEventTx(ctx, tx, event); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit verification: %w", err)
	}
	return true, nil
}

func (r *kongTicketRepository) SetTicketStatus(ctx context.Context, id int64, status string, attr service.KongAttribution) (bool, error) {
	probs, err := kongMarshalProbs(attr.Probs)
	if err != nil {
		return false, err
	}
	query := `UPDATE kong_ticket_cache
		SET status = $1, fingerprint_model = $2, fingerprint_p = $3, fingerprint_probs = $4
		WHERE id = $5 AND status = $6`
	res, err := r.db.ExecContext(ctx, query, status, attr.Model, attr.P, probs, id, service.KongTicketStatusUnverified)
	if err != nil {
		return false, fmt.Errorf("set ticket status: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set ticket status rows: %w", err)
	}
	return affected > 0, nil
}

// SkipCandidatesFor 把该账号该模型下**当前所有**未验证候选标为跳过。
//
// 用于「这一段无票期里的 observed 候选机会已经用掉」这个终点：只标一张的话，缓存里还有第二张旧
// 候选时，下一个请求会接着验证它——而候选验证不经过取票冷却，于是一张张烧额度。之后真正新到的票
// 不带这个标记，仍然算新信息。
func (r *kongTicketRepository) SkipCandidatesFor(ctx context.Context, accountID int64, model string, capturedBefore time.Time) error {
	// 只淘汰 capturedBefore 之前收到的候选：验证期间新到的票属于新信息，误标它会封掉唯一的
	// 恢复机会。
	_, err := r.db.ExecContext(ctx,
		`UPDATE kong_ticket_cache SET skip_until_new = TRUE
		 WHERE account_id = $1 AND model = $2 AND status = $3 AND skip_until_new = FALSE
		   AND captured_at <= $4`,
		accountID, model, service.KongTicketStatusUnverified, capturedBefore.UTC())
	if err != nil {
		return fmt.Errorf("skip candidates: %w", err)
	}
	return nil
}

// TicketStatus 读一张票当前的状态；票已不存在时返回空串而不是错误——过期清理会删行，那是预期
// 状态，调用方据空串按"没得出结论"处理即可。
func (r *kongTicketRepository) TicketStatus(ctx context.Context, id int64) (string, error) {
	var status string
	err := r.db.QueryRowContext(ctx, `SELECT status FROM kong_ticket_cache WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query ticket status: %w", err)
	}
	return status, nil
}

// SkipCandidate 标记单张候选。**只对 unverified 生效**——跳过标记是候选池的机制，把它打在一张
// 正在服务的 verified 票上等于悄悄作废它（VerifiedTickets 会筛掉 skip），而"作废当前票"必须是
// 显式动作、走 RevokeTicket。
func (r *kongTicketRepository) SkipCandidate(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE kong_ticket_cache SET skip_until_new = TRUE WHERE id = $1 AND status = $2`,
		id, service.KongTicketStatusUnverified)
	if err != nil {
		return fmt.Errorf("skip candidate: %w", err)
	}
	return nil
}

// ClearSkipMarks 在出现「新信息」时解除跳过标记：新到的 observed 票，或一次成功的主动取票。
// 新的业务请求本身不算新信息——否则每个请求都会重新试一遍同一批坏票。
func (r *kongTicketRepository) ClearSkipMarks(ctx context.Context, accountID int64, model string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE kong_ticket_cache SET skip_until_new = FALSE
		 WHERE account_id = $1 AND model = $2 AND skip_until_new`, accountID, model)
	if err != nil {
		return fmt.Errorf("clear skip marks: %w", err)
	}
	return nil
}

// RevokeTicket 撤销一张票的服务资格。
//
// 调用方必须传入**本次实际使用的票**的 id，不是「当前票」：并发下请求用 K1 发出、预取的 K2
// 可能已验证通过并接替成当前票，按当前票撤销会作废无辜的 K2，还白搭一段拒服。
// RevokeTicket 把一张票作废。
//
// 条件覆盖 verified **与 unverified**：被撤销的票总是注入过的（所以撤销时通常是 verified），但它
// 可能刚被手工复位成候选、正在重验。只收 verified 的话那次撤销会静默更新 0 行，随后的重验照样能
// 提交出资格——于是一张已经拿到"注入未被接受"证据的票被重新授予。收进 unverified 让重验的条件更新
// 落空，方向是拒服，符合本项目的取舍。
func (r *kongTicketRepository) RevokeTicket(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE kong_ticket_cache SET status = $1
		  WHERE id = $2 AND status IN ($3, $4)`,
		service.KongTicketStatusRejected, id,
		service.KongTicketStatusVerified, service.KongTicketStatusUnverified)
	if err != nil {
		return fmt.Errorf("revoke ticket: %w", err)
	}
	return nil
}

// ReviveRejectedCandidate 把最晚过期的那张未过期已拒票复位成候选。
//
// 同时清 skip_until_new：那张票自己通常没被标（批量跳过只标仍为 unverified 的），但手工触发的
// 语义是"把它重新变成可验证的候选"，留着标记等于复位一半。
//
// 归因列**不清**：它是上一次验证的证据，留到新结论落库时自然被覆盖；提前清掉只会让这段时间的
// 管理面显示不出这张票之前判成了什么。
func (r *kongTicketRepository) ReviveRejectedCandidate(ctx context.Context, accountID int64, model string) (*service.KongTicket, error) {
	// 外层 WHERE 必须重复 status 与期限两个条件，**不能只按 id**：子查询选中之后、外层更新之前，
	// 另一路可能已经把同一张票复位并验证成 verified；只按 id 更新会把那张刚取得资格的票打回
	// unverified，撤掉已经生效的服务资格。零行时调用方按"没有可复位的票"处理即可。
	query := `UPDATE kong_ticket_cache SET status = $1, skip_until_new = FALSE
		  WHERE id = (SELECT id FROM kong_ticket_cache
		               WHERE account_id = $2 AND model = $3 AND status = $4 AND expires_at > now()
		               ORDER BY expires_at DESC LIMIT 1)
		    AND status = $4 AND expires_at > now()
		  RETURNING ` + kongTicketColumns
	t, err := scanKongTicket(r.db.QueryRowContext(ctx, query,
		service.KongTicketStatusUnverified, accountID, model, service.KongTicketStatusRejected))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("revive rejected candidate: %w", err)
	}
	return t, nil
}

func (r *kongTicketRepository) CountTickets(ctx context.Context, accountID int64, model string, status string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM kong_ticket_cache
		 WHERE account_id = $1 AND model = $2 AND status = $3 AND expires_at > now()`,
		accountID, model, status).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count tickets: %w", err)
	}
	return n, nil
}

// DeleteExpiredTickets 按批删除过期票。票过期即无用，state 原值不长期留存。
func (r *kongTicketRepository) DeleteExpiredTickets(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 500
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM kong_ticket_cache WHERE id IN (
			SELECT id FROM kong_ticket_cache WHERE expires_at < $1 ORDER BY expires_at LIMIT $2
		)`, cutoff.UTC(), batchSize)
	if err != nil {
		return 0, fmt.Errorf("delete expired tickets: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired tickets rows: %w", err)
	}
	return affected, nil
}

func (r *kongTicketRepository) InsertEvent(ctx context.Context, e *service.KongTicketEvent) error {
	return insertTicketEventTx(ctx, r.db, e)
}

// kongExecer 抽掉「直连还是事务内」的差别，让事件插入在两种场合共用一份。
type kongExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertTicketEventTx(ctx context.Context, db kongExecer, e *service.KongTicketEvent) error {
	if e == nil {
		return errors.New("nil event")
	}
	createdAt := e.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	outcome := e.Outcome
	if outcome == "" {
		outcome = service.KongOutcomeInfo
	}
	detail := "{}"
	if len(e.Detail) > 0 {
		encoded, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("marshal event detail: %w", err)
		}
		detail = string(encoded)
	}
	query := `INSERT INTO kong_ticket_events
		(created_at, account_id, model, event_type, outcome, status_code,
		 traffic_egress, ticket_egress, state_len, ticket_id, fingerprint_model, idle_seconds, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	_, err := db.ExecContext(ctx, query, createdAt.UTC(), e.AccountID, kongNullString(e.Model),
		e.EventType, outcome, e.StatusCode, kongNullString(e.TrafficEgress), kongNullString(e.TicketEgress),
		e.StateLen, e.TicketID, e.FingerprintModel, e.IdleSeconds, detail)
	if err != nil {
		return fmt.Errorf("insert ticket event: %w", err)
	}
	return nil
}

func (r *kongTicketRepository) ListEvents(ctx context.Context, filter *service.KongTicketEventFilter) ([]*service.KongTicketEvent, int, error) {
	if filter == nil {
		filter = &service.KongTicketEventFilter{}
	}
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if len(filter.AccountIDs) > 0 {
		add("account_id = ANY($%d)", pq.Array(filter.AccountIDs))
	}
	if models := kongNonEmpty(filter.Models); len(models) > 0 {
		add("model = ANY($%d)", pq.Array(models))
	}
	if types := kongNonEmpty(filter.EventTypes); len(types) > 0 {
		add("event_type = ANY($%d)", pq.Array(types))
	}
	if filter.FinalOnly {
		// 只认显式为真的 `final`。缺这个键的历史事件一律不算最终事件——把它们当成最终结论，
		// 等于让一条单份失败事件冒充本次验证的结论。
		conds = append(conds, "detail->>'final' = 'true'")
	}
	if filter.Since != nil {
		add("created_at >= $%d", filter.Since.UTC())
	}
	if filter.Until != nil {
		add("created_at <= $%d", filter.Until.UTC())
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM kong_ticket_events`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count ticket events: %w", err)
	}

	limit := service.KongNormalizeEventLimit(filter.Limit)
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	query := `SELECT id, created_at, account_id, coalesce(model,''), event_type, outcome, status_code,
		coalesce(traffic_egress,''), coalesce(ticket_egress,''), state_len, ticket_id,
		fingerprint_model, idle_seconds, detail
		FROM kong_ticket_events` + where +
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list ticket events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*service.KongTicketEvent
	for rows.Next() {
		var e service.KongTicketEvent
		var detail []byte
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.AccountID, &e.Model, &e.EventType, &e.Outcome,
			&e.StatusCode, &e.TrafficEgress, &e.TicketEgress, &e.StateLen, &e.TicketID,
			&e.FingerprintModel, &e.IdleSeconds, &detail); err != nil {
			return nil, 0, fmt.Errorf("scan ticket event: %w", err)
		}
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				return nil, 0, fmt.Errorf("decode event detail (id=%d): %w", e.ID, err)
			}
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate ticket events: %w", err)
	}
	return out, total, nil
}

// LastEgressActivity 返回该票据出口最后一次被本系统使用的时刻，即调度公式里的 A。
//
// 取票无论成败都算活动——失败的取票同样在出口上留下了痕迹，这正是「预取失败后不能立即重来」
// 的原因。返回 nil 表示本系统从未用过该出口。
func (r *kongTicketRepository) LastEgressActivity(ctx context.Context, ticketEgress string) (*time.Time, error) {
	var at sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT max(created_at) FROM kong_ticket_events
		 WHERE ticket_egress = $1 AND event_type = $2`,
		ticketEgress, service.KongEventFetch).Scan(&at)
	if err != nil {
		return nil, fmt.Errorf("query last egress activity: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := at.Time
	return &t, nil
}

// LastFailure 返回该账号在该票据出口上最近一次**进入冷却**的时刻，即调度公式里的 F。
//
// 冷却从 F 起算，而静默从 A 起算，两者通常不同（取票有耗时、验证更慢），所以下一次尝试的
// 期限是 max(A+M, F+C)——把 C 取成与 M 相同的值并不等于「不额外加时」。
//
// 只看 cooldown 事件，不看所有 failure：observed 候选验证失败应当立即升级为主动取票，若把
// 那种失败也算进 F，升级会白等一个冷却期。denylist 命中、候选未被上游接受同理都不推进 F。
// 「是否进冷却」由写入方判断并落一条 cooldown 事件。
func (r *kongTicketRepository) LastFailure(ctx context.Context, accountID int64, ticketEgress string) (*time.Time, error) {
	var at sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT max(created_at) FROM kong_ticket_events
		 WHERE account_id = $1 AND ticket_egress = $2 AND event_type = $3`,
		accountID, ticketEgress, service.KongEventCooldown).Scan(&at)
	if err != nil {
		return nil, fmt.Errorf("query last failure: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := at.Time
	return &t, nil
}

// LastEventAt 返回该账号该模型最近一次指定类型事件的时刻。
//
// observe 的探测间隔靠它推进：这套设计刻意没有定时器，「上次探测是什么时候」只能从事件里读。
func (r *kongTicketRepository) LastEventAt(ctx context.Context, accountID int64, model, eventType string) (*time.Time, error) {
	var at sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT max(created_at) FROM kong_ticket_events
		 WHERE account_id = $1 AND model = $2 AND event_type = $3`,
		accountID, model, eventType).Scan(&at)
	if err != nil {
		return nil, fmt.Errorf("query last event at: %w", err)
	}
	if !at.Valid {
		return nil, nil
	}
	t := at.Time
	return &t, nil
}

// InsertProbes 写入一次验证的全部探测记录（1~3 份）。
//
// 作废的那几份同样要写：作废率本身是信号，它上升可能意味着上游改了行为或触发了风控，而不只是
// 运气不好。不写就只能看到「没拿到结论」，看不到为什么。
func (r *kongTicketRepository) InsertProbes(ctx context.Context, probes []*service.KongFingerprintProbe) error {
	if len(probes) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin probes tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `INSERT INTO kong_fingerprint_probes
		(created_at, verification_id, part_index, account_id, target_model, ticket_id,
		 ticket_fingerprint, ticket_source, verify_egress, capture_egress, idle_seconds, challenge_id,
		 digits, digit_count, scores, part_attribution, cum_probability, temperature_tier,
		 library_version, parse_valid, counted_in_average, invalid_reason, latency_ms, output_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare probes insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, p := range probes {
		if p == nil {
			continue
		}
		createdAt := p.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		digits, err := json.Marshal(p.Digits)
		if err != nil {
			return fmt.Errorf("marshal digits: %w", err)
		}
		// 编码失败必须让整批回滚，不能写入残缺证据：这些记录的唯一用途是将来离线重算，
		// 少了分数或版本标识就再也解释不了（例如 Scores 含 NaN 时 json.Marshal 必然失败）。
		scores := []byte("null")
		if len(p.Scores) > 0 {
			encoded, err := json.Marshal(p.Scores)
			if err != nil {
				return fmt.Errorf("marshal probe scores (part %d): %w", p.PartIndex, err)
			}
			scores = encoded
		}
		library := []byte("{}")
		if len(p.LibraryVersion) > 0 {
			encoded, err := json.Marshal(p.LibraryVersion)
			if err != nil {
				return fmt.Errorf("marshal probe library version (part %d): %w", p.PartIndex, err)
			}
			library = encoded
		}
		if _, err := stmt.ExecContext(ctx, createdAt.UTC(), p.VerificationID, p.PartIndex,
			p.AccountID, p.TargetModel, p.TicketID, kongNullString(p.TicketFingerprint),
			kongNullString(p.TicketSource), kongNullString(p.VerifyEgress),
			kongNullString(p.CaptureEgress), p.IdleSeconds, p.ChallengeID,
			string(digits), p.DigitCount, string(scores), p.PartAttribution, p.CumProbability,
			p.TemperatureTier, string(library), p.ParseValid, p.CountedInAverage,
			p.InvalidReason, p.LatencyMs, p.OutputTokens); err != nil {
			return fmt.Errorf("insert probe part %d: %w", p.PartIndex, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit probes: %w", err)
	}
	return nil
}

func (r *kongTicketRepository) ListProbesByVerification(ctx context.Context, verificationID string) ([]*service.KongFingerprintProbe, error) {
	query := `SELECT id, created_at, verification_id, part_index, account_id, target_model,
		ticket_id, coalesce(ticket_fingerprint,''), coalesce(ticket_source,''),
		coalesce(verify_egress,''), coalesce(capture_egress,''), idle_seconds, challenge_id, digits, digit_count, scores,
		part_attribution, cum_probability, temperature_tier, library_version,
		parse_valid, counted_in_average, invalid_reason, latency_ms, output_tokens
		FROM kong_fingerprint_probes WHERE verification_id = $1 ORDER BY part_index ASC`
	rows, err := r.db.QueryContext(ctx, query, verificationID)
	if err != nil {
		return nil, fmt.Errorf("list probes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*service.KongFingerprintProbe
	for rows.Next() {
		var p service.KongFingerprintProbe
		var digits, scores, library []byte
		if err := rows.Scan(&p.ID, &p.CreatedAt, &p.VerificationID, &p.PartIndex, &p.AccountID,
			&p.TargetModel, &p.TicketID, &p.TicketFingerprint, &p.TicketSource, &p.VerifyEgress,
			&p.CaptureEgress,
			&p.IdleSeconds, &p.ChallengeID, &digits, &p.DigitCount, &scores, &p.PartAttribution,
			&p.CumProbability, &p.TemperatureTier, &library, &p.ParseValid, &p.CountedInAverage,
			&p.InvalidReason, &p.LatencyMs, &p.OutputTokens); err != nil {
			return nil, fmt.Errorf("scan probe: %w", err)
		}
		// jsonb 保证内容是合法 JSON，不保证它能装进目标 Go 类型。装不进就是证据已经损坏，
		// 必须报出来而不是返回一份看起来正常、实际少了字段的记录。
		if len(digits) > 0 {
			if err := json.Unmarshal(digits, &p.Digits); err != nil {
				return nil, fmt.Errorf("decode probe digits (id=%d): %w", p.ID, err)
			}
		}
		if len(scores) > 0 {
			if err := json.Unmarshal(scores, &p.Scores); err != nil {
				return nil, fmt.Errorf("decode probe scores (id=%d): %w", p.ID, err)
			}
		}
		if len(library) > 0 {
			if err := json.Unmarshal(library, &p.LibraryVersion); err != nil {
				return nil, fmt.Errorf("decode probe library version (id=%d): %w", p.ID, err)
			}
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate probes: %w", err)
	}
	return out, nil
}

func kongNullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
