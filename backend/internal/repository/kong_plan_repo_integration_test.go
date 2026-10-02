//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
)

// 计划组件的持久化：迁移写入的初始发布状态、各写入的事务、重启后（新建存储实例读同一个库）的状态、
// 清空与代次。表是全局的，每个用例结束后恢复迁移后的初始状态。

func resetKongPlanTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, err := integrationDB.ExecContext(ctx, `DELETE FROM kong_plan_dispatch WHERE kind <> 'publish'`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE kong_plan_dispatch
		   SET generation = 1, seq = 0, revision = NULL, sha256 = NULL,
		       content = '{"generation": 1, "enabled": true, "last_seq": 0}'::jsonb
		 WHERE kind = 'publish'`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM kong_pool_caller`)
	require.NoError(t, err)
}

func newKongPlanIntegrationStore(t *testing.T) *service.KongPlanStore {
	t.Helper()
	s := service.NewKongPlanStore(NewKongPlanRepository(integrationDB))
	s.Start(context.Background())
	return s
}

func TestKongPlanRepo_MigrationSeedsPublishState(t *testing.T) {
	// 先直接读迁移写下的行（别的用例结束时会恢复成同样的初值），再重放迁移，确认不覆盖已有的发布状态。
	ctx := context.Background()
	var (
		gen, seq int64
		content  string
	)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT generation, seq, content::text FROM kong_plan_dispatch WHERE kind = 'publish'`).Scan(&gen, &seq, &content))
	require.Equal(t, []int64{1, 0}, []int64{gen, seq})
	require.JSONEq(t, `{"generation": 1, "enabled": true, "last_seq": 0}`, content)

	t.Cleanup(func() { resetKongPlanTables(t) })
	_, err := integrationDB.ExecContext(ctx, `UPDATE kong_plan_dispatch SET generation = 7, content = '{"generation": 7, "enabled": false, "last_seq": 3}'::jsonb WHERE kind = 'publish'`)
	require.NoError(t, err)
	sqlBytes, err := migrations.FS.ReadFile("907_kong_plan_dispatch.sql")
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, string(sqlBytes))
	require.NoError(t, err)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT generation FROM kong_plan_dispatch WHERE kind = 'publish'`).Scan(&gen))
	require.Equal(t, int64(7), gen, "重放迁移不覆盖已有的发布状态")

	resetKongPlanTables(t)
	pub, err := newKongPlanIntegrationStore(t).GetPublish(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), pub.Generation)
	require.True(t, pub.Enabled)
	require.Equal(t, int64(0), pub.LastSeq)
	require.Equal(t, service.KongPlanInUseLegacy, pub.InUse)
}

func TestKongPlanRepo_PersistAcrossRestart(t *testing.T) {
	resetKongPlanTables(t)
	t.Cleanup(func() { resetKongPlanTables(t) })
	ctx := context.Background()
	client := testEntClient(t)
	oauth := mustCreateAccount(t, client, &service.Account{Name: "kong-plan-oauth", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth})
	apikey := mustCreateAccount(t, client, &service.Account{Name: "kong-plan-apikey", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey})
	deleted := mustCreateAccount(t, client, &service.Account{Name: "kong-plan-deleted", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth})
	_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET deleted_at = now() WHERE id = $1`, deleted.ID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id = ANY(ARRAY[$1, $2, $3]::bigint[])`, oauth.ID, apikey.ID, deleted.ID)
	})

	s := newKongPlanIntegrationStore(t)
	expires := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	// credsOn 是快照里带 credits 的账号；另一个账号按备用档列出，同一列表里不重复
	dispatch := func(gen, seq int64, credsOn int64) (*service.KongPlanDispatchResult, error) {
		other := apikey.ID
		if credsOn == apikey.ID {
			other = oauth.ID
		}
		body := fmt.Sprintf(`{"generation": %d, "seq": %d, "plan_version": 5, "cycle_id": "c%d",
			"fallback": [{"id": %d, "tier": "normal", "credits": {"level": 1, "expires_at": "2026-12-31T23:59:00+08:00"}}],
			"snapshot": {"expires_at": %q, "accounts": [{"id": %d, "tier": "normal", "credits": {"level": 1, "expires_at": "2026-12-31T23:59:00+08:00"}},
			                                            {"id": %d, "tier": "standby"}, {"id": %d, "tier": "normal"}]}}`,
			gen, seq, seq, oauth.ID, expires, credsOn, other, deleted.ID)
		req, err := service.ParseKongPlanDispatch(strings.NewReader(body))
		require.NoError(t, err)
		return s.PutDispatch(ctx, req)
	}

	_, err = dispatch(1, 1, apikey.ID)
	require.ErrorContains(t, err, service.KongPlanReasonCreditsNotOAuth, "中转账号带 credits 被拒")
	res, err := dispatch(1, 1, oauth.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{deleted.ID}, res.IgnoredIDs, "已删除的账号照常接受、列为忽略")

	ctl, err := service.ParseKongPlanControl(strings.NewReader(fmt.Sprintf(`{"revision": 2, "credits_mode": "on", "allow": [%d],
		"floor": 10, "per_point": null, "overflow_margin": {"default": 1}, "file_hold": null}`, oauth.ID)))
	require.NoError(t, err)
	_, err = s.PutControl(ctx, ctl)
	require.NoError(t, err)
	hold, err := service.ParseKongPlanHold(strings.NewReader(`{"trigger": "manual:it", "reason": "manual", "note": "集成测试"}`))
	require.NoError(t, err)
	_, err = s.Hold(ctx, hold)
	require.NoError(t, err)
	rel, err := service.ParseKongPlanRelease(strings.NewReader(`{"triggers": ["daily_cap:old"], "note": "预先作废"}`))
	require.NoError(t, err)
	_, err = s.Release(ctx, rel)
	require.NoError(t, err)
	caller, err := service.ParseKongPoolCaller(strings.NewReader(`{"state": "throttled", "desired_pp_per_hour": 4.5}`))
	require.NoError(t, err)
	keyID := int64(7)
	_, err = s.PutCaller(ctx, caller, &keyID)
	require.NoError(t, err)

	views := func(s *service.KongPlanStore) string {
		d, err := s.GetDispatch(ctx)
		require.NoError(t, err)
		p, err := s.GetPublish(ctx)
		require.NoError(t, err)
		c, err := s.GetControl(ctx)
		require.NoError(t, err)
		cl, err := s.GetCaller(ctx)
		require.NoError(t, err)
		b, err := json.Marshal([]any{d, p, c, cl})
		require.NoError(t, err)
		return string(b)
	}
	before := views(s)
	restarted := newKongPlanIntegrationStore(t)
	require.Equal(t, before, views(restarted))

	// 清空并恢复后重启：代次与清空都持久，清空前的序号仍在，旧代次的发布被拒。
	pub, err := service.ParseKongPlanPublish(strings.NewReader(`{"action": "disable", "clear": "all", "note": "回到现状调度"}`))
	require.NoError(t, err)
	_, err = restarted.PutPublish(ctx, pub)
	require.NoError(t, err)
	en, err := service.ParseKongPlanPublish(strings.NewReader(`{"action": "enable", "note": "恢复"}`))
	require.NoError(t, err)
	_, err = restarted.PutPublish(ctx, en)
	require.NoError(t, err)

	s = newKongPlanIntegrationStore(t)
	p, err := s.GetPublish(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), p.Generation)
	require.Equal(t, int64(1), p.LastSeq)
	require.Equal(t, service.KongPlanInUseLegacy, p.InUse)
	_, err = dispatch(1, 9, oauth.ID)
	require.Error(t, err)
	_, err = dispatch(3, 2, oauth.ID)
	require.NoError(t, err)
	d, err := newKongPlanIntegrationStore(t).GetDispatch(ctx)
	require.NoError(t, err)
	require.Equal(t, service.KongPlanInUseSnapshot, d.InUse)

	var gen, seq int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT generation, seq FROM kong_plan_dispatch WHERE kind = 'publish'`).Scan(&gen, &seq))
	require.Equal(t, []int64{3, 2}, []int64{gen, seq}, "标量列与内容一致")
}
