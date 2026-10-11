//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 降档哨兵的仓储与迁移 910 的触发器：哨兵的两个键只有哨兵能写，人工与其他流程改开关时撤销哨兵的所有权。
// 哨兵的写入自带事务，所以这里写的是已提交的数据，测试结束时清理。

type kongGuardFixture struct {
	repo     *KongDowngradeGuardRepository
	accounts *accountRepository
	groupID  int64
}

func newKongGuardFixture(t *testing.T) *kongGuardFixture {
	t.Helper()
	client := testEntClient(t)
	group := mustCreateGroup(t, client, &service.Group{Name: fmt.Sprintf("kong-guard-%d", time.Now().UnixNano()), Platform: service.PlatformOpenAI})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id = $1", group.ID)
	})
	return &kongGuardFixture{
		repo:     NewKongDowngradeGuardRepository(integrationDB),
		accounts: newAccountRepositoryWithSQL(client, integrationDB, nil),
		groupID:  group.ID,
	}
}

func (f *kongGuardFixture) account(t *testing.T, inGroup bool) int64 {
	t.Helper()
	client := testEntClient(t)
	a := mustCreateAccount(t, client, &service.Account{Name: fmt.Sprintf("kong-guard-acct-%d", time.Now().UnixNano()),
		Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth})
	if inGroup {
		mustBindAccountToGroup(t, client, a.ID, f.groupID, 50)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM kong_downgrade_guard_events WHERE account_id = $1", a.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", a.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id = $1", a.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id = $1", a.ID)
	})
	return a.ID
}

// state 读账号的 schedulable、版本与持有记录的 op（没有持有记录时为空）。
func kongGuardState(t *testing.T, id int64) (bool, int64, string) {
	t.Helper()
	var schedulable bool
	var rev int64
	var op string
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT schedulable, `+kongGuardRevSQL+`, COALESCE(extra -> '`+kongGuardHoldKey+`' ->> 'op', '')
		FROM accounts WHERE id = $1`, id).Scan(&schedulable, &rev, &op))
	return schedulable, rev, op
}

func (f *kongGuardFixture) hold(t *testing.T, id int64, rev int64, op string) bool {
	t.Helper()
	ok, err := f.repo.Hold(context.Background(), service.KongGuardHoldInput{AccountID: id, GroupID: f.groupID, ExpectedRev: rev,
		Hold:  service.KongGuardHold{Op: op, Stage: service.KongGuardStageSuspect, Evidence: "ticket_refused", HeldAt: time.Now().UTC()},
		Event: service.KongGuardEvent{AccountID: id, Op: op, Kind: service.KongGuardEventHold, Detail: map[string]any{"stage": "suspect"}}})
	require.NoError(t, err)
	return ok
}

func TestKongGuardHoldConditions(t *testing.T) {
	f := newKongGuardFixture(t)
	ctx := context.Background()
	outside := f.account(t, false)
	require.False(t, f.hold(t, outside, 0, "op-out"), "不在服务分组不得关闭")

	id := f.account(t, true)
	require.False(t, f.hold(t, id, 5, "op-rev"), "版本不符不得关闭")
	require.True(t, f.hold(t, id, 0, "op-1"))
	schedulable, rev, op := kongGuardState(t, id)
	require.False(t, schedulable)
	require.Equal(t, int64(1), rev, "哨兵关闭时触发器给版本加一")
	require.Equal(t, "op-1", op)
	var outbox int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", id).Scan(&outbox))
	require.Equal(t, 1, outbox, "调度变更通知与关闭同一事务写入")
	require.False(t, f.hold(t, id, 1, "op-2"), "已有持有记录不得再关闭")

	events, err := f.repo.ListEvents(ctx, id, 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, service.KongGuardEventHold, events[0].Kind)
}

// 非哨兵的写入不能改动两个键；新建账号时去掉它们。
func TestKongGuardTriggerProtectsKeys(t *testing.T) {
	f := newKongGuardFixture(t)
	ctx := context.Background()
	id := f.account(t, true)
	require.True(t, f.hold(t, id, 0, "op-1"))

	require.NoError(t, f.accounts.UpdateExtra(ctx, id, map[string]any{kongGuardRevKey: 99, kongGuardHoldKey: map[string]any{"op": "forged"}}))
	_, rev, op := kongGuardState(t, id)
	require.Equal(t, int64(1), rev)
	require.Equal(t, "op-1", op)

	_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET extra = '{}'::jsonb WHERE id = $1`, id)
	require.NoError(t, err)
	_, rev, op = kongGuardState(t, id)
	require.Equal(t, int64(1), rev, "整体覆盖 extra 也保留两个键")
	require.Equal(t, "op-1", op)

	created := mustCreateAccount(t, testEntClient(t), &service.Account{Name: fmt.Sprintf("kong-guard-copy-%d", time.Now().UnixNano()),
		Platform: service.PlatformOpenAI, Extra: map[string]any{kongGuardRevKey: 7, kongGuardHoldKey: map[string]any{"op": "x"}}})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", created.ID)
	})
	_, rev, op = kongGuardState(t, created.ID)
	require.Zero(t, rev, "新建账号不得带入版本")
	require.Empty(t, op, "新建账号不得带入持有记录")
}

// 人用"可调度"开关（即便设的是同一个值）：删掉持有记录、版本加一；之后哨兵旧版本的误报撤销不生效。
func TestKongGuardExplicitToggleRevokes(t *testing.T) {
	f := newKongGuardFixture(t)
	ctx := context.Background()
	id := f.account(t, true)
	require.True(t, f.hold(t, id, 0, "op-1"))

	require.NoError(t, f.accounts.SetSchedulable(ctx, id, false))
	schedulable, rev, op := kongGuardState(t, id)
	require.False(t, schedulable)
	require.Equal(t, int64(2), rev)
	require.Empty(t, op)

	ok, err := f.repo.ReleaseFalseAlarm(ctx, id, "op-1", 1, service.KongGuardEvent{AccountID: id, Op: "op-1", Kind: service.KongGuardEventFalseAlarm})
	require.NoError(t, err)
	require.False(t, ok)
	schedulable, _, _ = kongGuardState(t, id)
	require.False(t, schedulable, "旧结论不得重新打开账号")

	// 人打开：前置语句连同开关一起写，版本加一。
	require.NoError(t, f.accounts.SetSchedulable(ctx, id, true))
	schedulable, rev, _ = kongGuardState(t, id)
	require.True(t, schedulable)
	require.Equal(t, int64(3), rev)

	// 人关闭一个可调度的账号：前置语句已经把它关上，哨兵按新版本也关不了（条件要求 schedulable 为真）。
	on := false
	require.NoError(t, kongExplicitSchedulable(ctx, integrationDB, []int64{id}, &on))
	schedulable, rev, _ = kongGuardState(t, id)
	require.False(t, schedulable)
	require.False(t, f.hold(t, id, rev, "op-race"), "人关闭之后哨兵不得再取得所有权")
	var outbox int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", id).Scan(&outbox))
	require.NotZero(t, outbox, "开关与调度变更通知一起提交")
}

// 批量更新带 schedulable、置错误状态：同样撤销所有权。
func TestKongGuardBulkAndErrorRevoke(t *testing.T) {
	f := newKongGuardFixture(t)
	ctx := context.Background()
	a := f.account(t, true)
	b := f.account(t, true)
	require.True(t, f.hold(t, a, 0, "op-a"))
	require.True(t, f.hold(t, b, 0, "op-b"))

	no := false
	_, err := f.accounts.BulkUpdate(ctx, []int64{a}, service.AccountBulkUpdate{Schedulable: &no})
	require.NoError(t, err)
	_, rev, op := kongGuardState(t, a)
	require.Empty(t, op)
	require.Equal(t, int64(2), rev)

	require.NoError(t, f.accounts.SetError(ctx, b, "boom"))
	schedulable, rev, op := kongGuardState(t, b)
	require.False(t, schedulable)
	require.Empty(t, op)
	require.Equal(t, int64(2), rev, "状态变为 error 时版本加一")
}

// 普通编辑：编辑开始时读到的账号是关闭前的，保存时沿用库里的 schedulable，也不丢两个键。
func TestKongGuardEditKeepsDBSchedulable(t *testing.T) {
	f := newKongGuardFixture(t)
	ctx := context.Background()
	id := f.account(t, true)
	stale, err := f.accounts.GetByID(ctx, id)
	require.NoError(t, err)
	require.True(t, f.hold(t, id, 0, "op-1"))

	stale.Name = stale.Name + "-edited"
	stale.Extra = map[string]any{}
	require.NoError(t, f.accounts.Update(ctx, stale))
	schedulable, rev, op := kongGuardState(t, id)
	require.False(t, schedulable, "编辑不得用旧值重新打开账号")
	require.Equal(t, int64(1), rev)
	require.Equal(t, "op-1", op)

	// 版本一致时照常用编辑提交的值。
	fresh, err := f.accounts.GetByID(ctx, id)
	require.NoError(t, err)
	fresh.Schedulable = true
	require.NoError(t, f.accounts.Update(ctx, fresh))
	schedulable, rev, op = kongGuardState(t, id)
	require.True(t, schedulable)
	require.Equal(t, int64(2), rev)
	require.Empty(t, op, "人通过编辑打开账号时撤销哨兵的所有权")
}

// 误报撤销、确认与到期接管的条件。
func TestKongGuardReleaseConfirmTakeover(t *testing.T) {
	f := newKongGuardFixture(t)
	ctx := context.Background()
	id := f.account(t, true)
	require.True(t, f.hold(t, id, 0, "op-1"))

	ok, err := f.repo.UpdateHold(ctx, id, "op-other", 1, map[string]any{"stage": service.KongGuardStageConfirmed})
	require.NoError(t, err)
	require.False(t, ok, "op 不符不得改持有记录")
	ok, err = f.repo.UpdateHold(ctx, id, "op-1", 1, map[string]any{"fp_attempts": 1})
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = f.repo.ReleaseFalseAlarm(ctx, id, "op-1", 1, service.KongGuardEvent{AccountID: id, Op: "op-1", Kind: service.KongGuardEventFalseAlarm,
		Detail: map[string]any{"cooldown_until": time.Now().Add(24 * time.Hour).UTC()}})
	require.NoError(t, err)
	require.True(t, ok)
	schedulable, rev, op := kongGuardState(t, id)
	require.True(t, schedulable)
	require.Equal(t, int64(2), rev)
	require.Empty(t, op)
	recent, err := f.repo.RecentEvents(ctx, []string{service.KongGuardEventFalseAlarm}, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NotEmpty(t, recent)

	// 到期：不撤销，到期接管删掉持有记录、版本加一，再扫不重复加。
	require.True(t, f.hold(t, id, 2, "op-2"))
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET auto_pause_on_expired = TRUE, expires_at = now() - interval '1 minute' WHERE id = $1`, id)
	require.NoError(t, err)
	ok, err = f.repo.ReleaseFalseAlarm(ctx, id, "op-2", 3, service.KongGuardEvent{AccountID: id, Op: "op-2", Kind: service.KongGuardEventFalseAlarm})
	require.NoError(t, err)
	require.False(t, ok, "已到期不得撤销")
	ids, err := f.repo.TakeOverExpired(ctx)
	require.NoError(t, err)
	require.Contains(t, ids, id)
	schedulable, rev, op = kongGuardState(t, id)
	require.False(t, schedulable)
	require.Equal(t, int64(4), rev)
	require.Empty(t, op)
	ids, err = f.repo.TakeOverExpired(ctx)
	require.NoError(t, err)
	require.NotContains(t, ids, id)
}

// 显式前置在调用方的事务里执行时，哨兵写入标记不能留给同一事务里的后续语句。
func TestKongGuardExplicitFlagDoesNotLeak(t *testing.T) {
	f := newKongGuardFixture(t)
	id := f.account(t, true)
	require.True(t, f.hold(t, id, 0, "op-1"))

	tx := testTx(t)
	ctx := context.Background()
	off := false
	require.NoError(t, kongExplicitSchedulable(ctx, tx, []int64{id}, &off))
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET extra = extra || '{"kong_schedulable_rev": 99}'::jsonb WHERE id = $1`, id)
	require.NoError(t, err)
	var rev int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT `+kongGuardRevSQL+` FROM accounts WHERE id = $1`, id).Scan(&rev))
	require.Equal(t, int64(2), rev, "前置之后的普通写入仍受触发器保护")
}

// 影响汇总按 op 唯一：未定稿时覆盖，定稿后不再覆盖。
func TestKongGuardUpsertImpact(t *testing.T) {
	f := newKongGuardFixture(t)
	ctx := context.Background()
	id := f.account(t, true)
	op := fmt.Sprintf("op-impact-%d", time.Now().UnixNano())
	require.NoError(t, f.repo.UpsertImpact(ctx, id, op, map[string]any{"total": 1, "finalized": false}))
	require.NoError(t, f.repo.UpsertImpact(ctx, id, op, map[string]any{"total": 2, "finalized": true}))
	require.NoError(t, f.repo.UpsertImpact(ctx, id, op, map[string]any{"total": 3, "finalized": false}))
	var n int
	var total float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*), MAX((detail ->> 'total')::float8) FROM kong_downgrade_guard_events
		WHERE kind = 'impact' AND op = $1`, op).Scan(&n, &total))
	require.Equal(t, 1, n)
	require.Equal(t, float64(2), total)
}

// 过期清理：hold 事件要等影响汇总定稿才删，没有汇总或汇总未定稿的都留着。
func TestKongGuardDeleteKeepsUnfinalizedHolds(t *testing.T) {
	f := newKongGuardFixture(t)
	ctx := context.Background()
	id := f.account(t, true)
	suffix := time.Now().UnixNano()
	ops := map[string]string{"none": fmt.Sprintf("op-none-%d", suffix), "open": fmt.Sprintf("op-open-%d", suffix), "done": fmt.Sprintf("op-done-%d", suffix)}
	for _, op := range ops {
		_, err := integrationDB.ExecContext(ctx, `INSERT INTO kong_downgrade_guard_events (created_at, account_id, op, kind)
			VALUES (now() - interval '100 days', $1, $2, 'hold')`, id, op)
		require.NoError(t, err)
	}
	for name, finalized := range map[string]bool{"open": false, "done": true} {
		_, err := integrationDB.ExecContext(ctx, `INSERT INTO kong_downgrade_guard_events (created_at, account_id, op, kind, detail)
			VALUES (now() - interval '99 days', $1, $2, 'impact', jsonb_build_object('finalized', $3::boolean))`, id, ops[name], finalized)
		require.NoError(t, err)
	}
	_, err := f.repo.DeleteEventsBefore(ctx, time.Now().Add(-90*24*time.Hour), 1000)
	require.NoError(t, err)
	left := map[string]int{}
	rows, err := integrationDB.QueryContext(ctx, `SELECT op, COUNT(*) FROM kong_downgrade_guard_events WHERE account_id = $1 GROUP BY op`, id)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var op string
		var n int
		require.NoError(t, rows.Scan(&op, &n))
		left[op] = n
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 1, left[ops["none"]], "没有汇总的 hold 要留着")
	require.Equal(t, 2, left[ops["open"]], "汇总未定稿的 hold 与汇总都留着")
	require.Zero(t, left[ops["done"]], "汇总已定稿的可以清理")
}
