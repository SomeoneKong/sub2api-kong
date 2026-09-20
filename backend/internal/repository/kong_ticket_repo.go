package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// kongTicketListMax 是票据列表的行数上限。票的 TTL 是一小时、且有留存清理，正常账号远到不了；
// 这个值只为挡住"清理停摆后一次查出几万行"。
const kongTicketListMax = 200

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

// NewestCandidate 返回最新的一张可验证候选（人工「立即验票」用，顺序与上面相反的理由见接口注释）。
// 同样不选 skip_until_new 的：那些是「未被上游接受」留下的，重选只会重复同一个无效尝试。
func (r *kongTicketRepository) NewestCandidate(ctx context.Context, accountID int64, model string, now time.Time) (*service.KongTicket, error) {
	query := `SELECT ` + kongTicketColumns + ` FROM kong_ticket_cache
		WHERE account_id = $1 AND model = $2 AND status = $3
		  AND expires_at > $4 AND NOT skip_until_new
		ORDER BY captured_at DESC LIMIT 1`
	t, err := scanKongTicket(r.db.QueryRowContext(ctx, query,
		accountID, model, service.KongTicketStatusUnverified, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query newest candidate: %w", err)
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
// VerifiedTicketsElsewhere 列别的账号在同一模型上此刻可用的票，每个账号取最晚过期的那一张。
//
// 条件与 VerifiedTickets 一致（verified、未过期、未被 skip），只是把本账号排除掉，并且**带回归因
// 分布**——调用方要按当前白名单重判，`status = verified` 只代表"按当时的白名单判过"。
//
// 每账号一张就够：调用方只需要知道"这个账号能不能接手"，不需要它的全部票。
func (r *kongTicketRepository) VerifiedTicketsElsewhere(ctx context.Context, excludeAccountID int64, model string) ([]*service.KongTicket, error) {
	query := `SELECT DISTINCT ON (account_id) ` + kongTicketColumns + ` FROM kong_ticket_cache
		WHERE account_id <> $1 AND model = $2 AND status = $3
		  AND expires_at > now() AND skip_until_new = FALSE
		ORDER BY account_id, expires_at DESC`
	rows, err := r.db.QueryContext(ctx, query, excludeAccountID, model, service.KongTicketStatusVerified)
	if err != nil {
		return nil, fmt.Errorf("query verified tickets elsewhere: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*service.KongTicket
	for rows.Next() {
		t, err := scanKongTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verified tickets elsewhere: %w", err)
	}
	return out, nil
}

// TicketByID 按 (账号, 票 id) 读一张票。account_id 进 WHERE 是**权限边界**，不是优化。
func (r *kongTicketRepository) TicketByID(ctx context.Context, accountID, id int64) (*service.KongTicket, error) {
	query := `SELECT ` + kongTicketColumns + ` FROM kong_ticket_cache WHERE id = $1 AND account_id = $2`
	t, err := scanKongTicket(r.db.QueryRowContext(ctx, query, id, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query ticket by id: %w", err)
	}
	return t, nil
}

// PrepareTicketForManualVerify 把一张票准备成可重验状态：已拒的复位成候选，并清掉跳过标记。
//
// **期限条件必须留在 WHERE 里**：过期的票不该被复活。status 用 CASE 而不是无条件写 unverified
// ——一张 verified 的票（比如注入被拒后被标了 skip 的那张）不能被打回 unverified，那会撤掉它已经
// 生效的服务资格。
func (r *kongTicketRepository) PrepareTicketForManualVerify(ctx context.Context, id int64) (*service.KongTicket, error) {
	query := `UPDATE kong_ticket_cache
		  SET status = CASE WHEN status = $1 THEN $2 ELSE status END,
		      skip_until_new = FALSE
		  WHERE id = $3 AND expires_at > now()
		  RETURNING ` + kongTicketColumns
	t, err := scanKongTicket(r.db.QueryRowContext(ctx, query,
		service.KongTicketStatusRejected, service.KongTicketStatusUnverified, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("prepare ticket for manual verify: %w", err)
	}
	return t, nil
}

// ListTickets 列一个账号名下的票。含已过期与已拒的——管理页面要能看见"为什么现在没票可用"，
// 只列可用的那些等于把答案藏起来。
func (r *kongTicketRepository) ListTickets(ctx context.Context, accountID int64, limit int) ([]*service.KongTicket, error) {
	// 归一化必须在这里而不是交给调用方：0、负数、超大值都要落到同一个上限，否则测试替身与生产
	// 在边界上各行其是。
	if limit <= 0 || limit > kongTicketListMax {
		limit = kongTicketListMax
	}
	query := `SELECT ` + kongTicketColumns + ` FROM kong_ticket_cache
		WHERE account_id = $1
		ORDER BY captured_at DESC
		LIMIT $2`
	rows, err := r.db.QueryContext(ctx, query, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*service.KongTicket
	for rows.Next() {
		t, err := scanKongTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tickets: %w", err)
	}
	return out, nil
}

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
	//
	// **只淘汰 observed 候选。** 这条限制是批量取票之后必须有的：批内非触发模型的 fetch 票会常态
	// 留在候选池里，而它们是用一整段出口静默换来的——旧 observed 候选验证失败时把它们一起标掉，
	// 等于白扔那次取票，而出口刚被用过、约 34 分钟内补不回来。本函数的用途本来就是"这一段无票期
	// 里的 observed 候选机会已经用掉"，fetch 票不在其语义内。
	//
	// 正在验证的那张 fetch 候选由调用方单独标（见 verifyTicket 的 no_valid_answer 分支），否则
	// 下一个请求会把它再验一遍。
	_, err := r.db.ExecContext(ctx,
		`UPDATE kong_ticket_cache SET skip_until_new = TRUE
		 WHERE account_id = $1 AND model = $2 AND status = $3 AND skip_until_new = FALSE
		   AND source = $5
		   AND captured_at <= $4`,
		accountID, model, service.KongTicketStatusUnverified, capturedBefore.UTC(),
		service.KongTicketSourceObserved)
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

// TicketConclusions 取这些票各自最后一次**已提交归因**的结论层级。
//
// 三件事都在 SQL 里做完：只认提交了归因的最终事件（`fingerprint_model` 非空——stg0 填回报值、stg1 填
// argmax，未提交的收尾不填）、每个 `ticket_id` 只取最新一条（`DISTINCT ON`）、缺 `stg` 键的历史事件
// 算 stg1。放到应用层筛就要先按行数上限取一批，而一张票被反复重验时那批里可能一条已提交的都不剩。
//
// `stg` 只接受纯数字文本再转 int：那个键由本系统写入，但库里的历史数据不该让一次展示查询报错。
func (r *kongTicketRepository) TicketConclusions(
	ctx context.Context, accountID int64, ticketIDs []int64,
) (map[int64]int, error) {
	if len(ticketIDs) == 0 {
		return map[int64]int{}, nil
	}
	const query = `SELECT DISTINCT ON (ticket_id) ticket_id,
		CASE WHEN detail->>'stg' ~ '^[0-9]+$' THEN (detail->>'stg')::int ELSE 1 END AS stg
		FROM kong_ticket_events
		WHERE account_id = $1
		  AND ticket_id = ANY($2)
		  AND event_type = $3
		  AND detail->>'final' = 'true'
		  AND fingerprint_model IS NOT NULL
		  AND fingerprint_model <> ''
		ORDER BY ticket_id, created_at DESC, id DESC`
	rows, err := r.db.QueryContext(ctx, query, accountID, pq.Array(ticketIDs), service.KongEventVerify)
	if err != nil {
		return nil, fmt.Errorf("list ticket conclusions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int64]int, len(ticketIDs))
	for rows.Next() {
		var ticketID int64
		var stg int
		if err := rows.Scan(&ticketID, &stg); err != nil {
			return nil, fmt.Errorf("scan ticket conclusion: %w", err)
		}
		out[ticketID] = stg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ticket conclusions: %w", err)
	}
	return out, nil
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
		 library_version, parse_valid, counted_in_average, invalid_reason, latency_ms, output_tokens,
		 fused, discarded_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,
		        $25,$26)`
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
			p.InvalidReason, p.LatencyMs, p.OutputTokens,
			p.Fused, kongNullString(p.DiscardedReason)); err != nil {
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
		parse_valid, counted_in_average, invalid_reason, latency_ms, output_tokens,
		fused, coalesce(discarded_reason,'')
		FROM kong_fingerprint_probes WHERE verification_id = $1
		-- 按 part_index 再按 id：融合样本与常规首份都可能占同一个序号（融合被丢弃后 leader 重跑），
		-- 只按 part_index 排的话两条的先后不确定，而读的人要按发生顺序理解证据。
		ORDER BY part_index ASC, id ASC`
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
			&p.InvalidReason, &p.LatencyMs, &p.OutputTokens,
			&p.Fused, &p.DiscardedReason); err != nil {
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

// kongStg0EffectiveModel 是"发给上游的模型名"的 SQL 表达式。
//
// **必须用它而不是 `model`**：存在 model mapping 时客户端请求的名字与发给上游的名字不同，而门控是按
// 后者判的，按 `model` 聚合会漏掉被映射过来的请求。写法照抄上游同口径的表达式索引
// （idx_usage_logs_effective_upstream_model_created），保持两边对"有效上游模型"的定义一致。
//
// 实测（生产库、3 天窗口 1.5 万行）：规划器走的是 `idx_usage_logs_created_at` 再按模型过滤，约
// 120ms，而不是那个表达式索引——因为窗口内几乎所有请求都是受控模型，过滤掉的只有几十行。这个量级对
// 管理页面可接受；若将来窗口内行数涨到百万级，这里需要重新看计划。
const kongStg0EffectiveModel = `COALESCE(NULLIF(BTRIM(upstream_model), ''), model)`

// kongStg0TopReportedMax 是每个 (账号, 模型) 保留的回报值种数上限。
//
// 它只为挡住"上游大面积回报各种模型时查出过多行"。取 5：运维需要的是"该往 stg0_accept 加哪一条"，
// 前几名足以回答，而列全部只会把那个答案埋掉。
const kongStg0TopReportedMax = 5

// Stg0Stats 汇总近期业务请求里上游回报 model 的一致性，按 (账号, 有效模型) 分组。
//
// 数据全部来自上游既有的两列（upstream_response_model / upstream_model_mismatch），本功能不写入
// 任何东西——业务路径的 stg0 是纯观测。**分子分母同在一行**，所以比例可解释；分母若另取一处
// （例如只数请求数的计数器），两侧口径不同会让比例变成两个不同总体的商。
//
// mismatch 判据用的是**上游的审计口径**（字面相等），比验票路径的 stg0 判据更严——它会把
// `gpt-5.4-mini-2026-03-17` 这种快照后缀也算成不一致。这里刻意不做二次收窄：这一侧只用于观测与告警，
// 宁可多报几条让人看见，也不要在统计里悄悄抹掉上游确实回报过别的字符串这个事实。
func (r *kongTicketRepository) Stg0Stats(ctx context.Context, models []string, since time.Time) ([]*service.KongStg0Stats, error) {
	models = kongNonEmpty(models)
	if len(models) == 0 {
		return nil, nil
	}

	query := `SELECT account_id, ` + kongStg0EffectiveModel + ` AS eff_model,
		count(*) AS total,
		count(*) FILTER (WHERE upstream_model_mismatch) AS mismatch,
		count(*) FILTER (WHERE upstream_model_mismatch IS NULL) AS unknown
		FROM usage_logs
		WHERE created_at >= $1 AND ` + kongStg0EffectiveModel + ` = ANY($2)
		GROUP BY 1, 2`
	rows, err := r.db.QueryContext(ctx, query, since.UTC(), pq.Array(models))
	if err != nil {
		return nil, fmt.Errorf("query stg0 stats: %w", err)
	}
	byKey := map[string]*service.KongStg0Stats{}
	out := make([]*service.KongStg0Stats, 0, len(models))
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			// TopReported 显式给空切片，**不能让它留成 nil**：nil 切片序列化成 JSON `null`，而
			// 前端按数组读 `.length` / `.map`。零 mismatch 是最常见的正常情况，留 nil 等于让每个
			// 正常账号都把页面打崩，而 Go 侧与 TS 侧的类型检查都看不出来。
			s := &service.KongStg0Stats{TopReported: []service.KongStg0Reported{}}
			if err = rows.Scan(&s.AccountID, &s.Model, &s.Total, &s.Mismatch, &s.Unknown); err != nil {
				return
			}
			byKey[kongStg0Key(s.AccountID, s.Model)] = s
			out = append(out, s)
		}
		err = rows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("scan stg0 stats: %w", err)
	}

	// 第二趟取回报值明细。只在确实有不一致时才查——绝大多数时候上一趟的 mismatch 全是 0，那时这次
	// 查询纯属浪费。
	var hasMismatch bool
	for _, s := range out {
		if s.Mismatch > 0 {
			hasMismatch = true
			break
		}
	}
	if !hasMismatch {
		return out, nil
	}

	detail := `SELECT account_id, ` + kongStg0EffectiveModel + ` AS eff_model,
		COALESCE(upstream_response_model, '') AS reported, count(*) AS n
		FROM usage_logs
		WHERE created_at >= $1 AND ` + kongStg0EffectiveModel + ` = ANY($2)
		  AND upstream_model_mismatch
		GROUP BY 1, 2, 3
		ORDER BY 1, 2, 4 DESC`
	drows, err := r.db.QueryContext(ctx, detail, since.UTC(), pq.Array(models))
	if err != nil {
		// 明细查不到不该让整个统计失败：总数与比例已经拿到了，那是页面上的主要信息。
		return out, nil
	}
	defer func() { _ = drows.Close() }()
	for drows.Next() {
		var accountID int64
		var model, reported string
		var n int64
		if err := drows.Scan(&accountID, &model, &reported, &n); err != nil {
			return out, nil
		}
		s := byKey[kongStg0Key(accountID, model)]
		if s == nil || len(s.TopReported) >= kongStg0TopReportedMax {
			continue
		}
		s.TopReported = append(s.TopReported, service.KongStg0Reported{Model: reported, Count: n})
	}
	return out, nil
}

func kongStg0Key(accountID int64, model string) string {
	return strconv.FormatInt(accountID, 10) + "\x00" + model
}
