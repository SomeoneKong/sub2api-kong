//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 目录存档在真实库上的行为：版本按 sha256 去重；采样同版本同套餐续上最新一段，套餐或版本变了开新段；
// 按客户端版本取每个账号的最新一段。用独有的客户端版本与 sha256 隔离，结束时删掉。
func TestKongCodexCatalogRepositoryIntegration(t *testing.T) {
	ctx := context.Background()
	repo := NewKongCodexCatalogRepository(integrationDB)
	const clientVersion = "9.9.9-kong-catalog-test"
	shaFull, shaFree := "kong-catalog-test-full", "kong-catalog-test-free"
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM kong_codex_catalog_observations WHERE client_version = $1`, clientVersion)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM kong_codex_catalogs WHERE sha256 IN ($1, $2)`, shaFull, shaFree)
	})

	full := service.KongCodexCatalogRecord{SHA256: shaFull, Slugs: []string{"gpt-6.1-sol", "gpt-6-luna"}, Body: []byte(`{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6-luna"}]}`)}
	fullID, err := repo.EnsureKongCodexCatalog(ctx, full)
	require.NoError(t, err)
	again, err := repo.EnsureKongCodexCatalog(ctx, full)
	require.NoError(t, err)
	require.Equal(t, fullID, again, "同一 sha256 复用已有行")
	freeID, err := repo.EnsureKongCodexCatalog(ctx, service.KongCodexCatalogRecord{SHA256: shaFree, Slugs: []string{"gpt-6-luna"}, Body: []byte(`{"models":[{"slug":"gpt-6-luna"}]}`)})
	require.NoError(t, err)
	require.NotEqual(t, fullID, freeID)

	records, err := repo.LoadKongCodexCatalogs(ctx, []int64{fullID})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, full.Slugs, records[0].Slugs)
	require.Equal(t, full.Body, records[0].Body)

	base := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	record := func(accountID, catalogID int64, plan string, at time.Time) {
		t.Helper()
		require.NoError(t, repo.RecordKongCodexCatalogSample(ctx, service.KongCodexCatalogObservation{
			AccountID: accountID, ClientVersion: clientVersion, CatalogID: catalogID, PlanType: plan, FirstSeenAt: at, LastSeenAt: at,
		}))
	}
	record(901, fullID, "pro", base)
	record(901, fullID, "pro", base.Add(10*time.Minute)) // 续上
	record(902, fullID, "pro", base)
	record(902, fullID, "free", base.Add(10*time.Minute)) // 套餐变了：新段
	record(902, freeID, "free", base.Add(20*time.Minute)) // 版本变了：新段

	var segments, samples int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(samples), 0) FROM kong_codex_catalog_observations WHERE client_version = $1 AND account_id = 901`,
		clientVersion).Scan(&segments, &samples))
	require.Equal(t, 1, segments)
	require.Equal(t, 2, samples)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT count(*) FROM kong_codex_catalog_observations WHERE client_version = $1 AND account_id = 902`,
		clientVersion).Scan(&segments))
	require.Equal(t, 3, segments)

	latest, err := repo.LatestKongCodexCatalogObservations(ctx, clientVersion)
	require.NoError(t, err)
	byAccount := map[int64]service.KongCodexCatalogObservation{}
	for _, o := range latest {
		byAccount[o.AccountID] = o
	}
	require.Len(t, byAccount, 2)
	require.Equal(t, fullID, byAccount[901].CatalogID)
	require.True(t, byAccount[901].FirstSeenAt.Equal(base))
	require.True(t, byAccount[901].LastSeenAt.Equal(base.Add(10*time.Minute)))
	require.Equal(t, freeID, byAccount[902].CatalogID)
	require.Equal(t, "free", byAccount[902].PlanType)
}
