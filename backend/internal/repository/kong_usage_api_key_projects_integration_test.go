//go:build integration

package repository

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

func (s *UsageLogRepoSuite) kongCreateProjectUsageLog(user *service.User, apiKey *service.APIKey, account *service.Account, requestedModel string, tier *string, tokens int, createdAt time.Time) {
	_, err := s.repo.Create(s.ctx, &service.UsageLog{
		UserID:          user.ID,
		APIKeyID:        apiKey.ID,
		AccountID:       account.ID,
		RequestID:       uuid.New().String(),
		Model:           "stored-model",
		RequestedModel:  requestedModel,
		ServiceTier:     tier,
		InputTokens:     tokens,
		OutputTokens:    tokens,
		CacheReadTokens: tokens,
		TotalCost:       0.5,
		ActualCost:      0.25,
		CreatedAt:       createdAt,
	})
	s.Require().NoError(err)
}

// 档位归类：Fast、Ultrafast、Flex 之外都算标准档，比较时不分大小写、不看空白。
func (s *UsageLogRepoSuite) TestKongAPIKeyProjectStatsClassifiesTiers() {
	user := mustCreateUser(s.T(), s.client, &service.User{Email: "kong-project-tiers@test.com"})
	apiKey := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-kong-project-tiers", Name: "tiers"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-kong-project-tiers"})

	at := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	want := map[string]int64{"standard": 0, "fast": 0, "ultrafast": 0, "flex": 0}
	for _, tc := range []struct {
		tier *string
		want string
	}{
		{nil, "standard"},
		{kongProjectTier(""), "standard"},
		{kongProjectTier("default"), "standard"},
		{kongProjectTier("auto"), "standard"},
		{kongProjectTier("scale"), "standard"},
		{kongProjectTier("somethingnew"), "standard"},
		{kongProjectTier("priority"), "fast"},
		{kongProjectTier("fast"), "fast"},
		{kongProjectTier(" PRIORITY "), "fast"},
		{kongProjectTier("ultrafast"), "ultrafast"},
		{kongProjectTier("flex"), "flex"},
	} {
		s.kongCreateProjectUsageLog(user, apiKey, account, "gpt-6-astra", tc.tier, 1, at)
		want[tc.want]++
	}

	start := at.Add(-time.Hour)
	end := at.Add(time.Hour)
	stats, err := s.repo.KongGetAPIKeyProjectStats(s.ctx, usagestats.UsageLogFilters{UserID: user.ID, StartTime: &start, EndTime: &end}, "UTC")
	s.Require().NoError(err)

	got := map[string]int64{"standard": 0, "fast": 0, "ultrafast": 0, "flex": 0}
	for _, row := range stats.Rows {
		got[row.Tier] += row.Requests
	}
	s.Require().Equal(want, got)
}

// 按模型、档位、小时分组：只算本用户、只算时间范围内，软删除的 key 仍有名字，合计与 API Key 使用分布对得上。
func (s *UsageLogRepoSuite) TestKongAPIKeyProjectStatsGroupsAndMatchesKeyStats() {
	user := mustCreateUser(s.T(), s.client, &service.User{Email: "kong-project-group@test.com"})
	other := mustCreateUser(s.T(), s.client, &service.User{Email: "kong-project-group-other@test.com"})
	laptop := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-kong-project-laptop", Name: "laptop"})
	retired := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-kong-project-retired", Name: "retired"})
	otherKey := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: other.ID, Key: "sk-kong-project-other", Name: "other"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-kong-project-group"})

	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	priority := kongProjectTier("priority")
	s.kongCreateProjectUsageLog(user, laptop, account, "gpt-6-astra", nil, 100, base.Add(10*time.Minute))
	s.kongCreateProjectUsageLog(user, laptop, account, "gpt-6-astra", nil, 200, base.Add(20*time.Minute))
	s.kongCreateProjectUsageLog(user, laptop, account, "gpt-6-astra", priority, 300, base.Add(30*time.Minute))
	s.kongCreateProjectUsageLog(user, laptop, account, "gpt-6.1-sol", nil, 400, base.Add(40*time.Minute))
	s.kongCreateProjectUsageLog(user, laptop, account, "gpt-6-astra", nil, 500, base.Add(90*time.Minute))
	s.kongCreateProjectUsageLog(user, retired, account, "gpt-6-astra", nil, 600, base.Add(time.Minute))
	s.kongCreateProjectUsageLog(other, otherKey, account, "gpt-6-astra", nil, 9000, base.Add(time.Minute))
	s.kongCreateProjectUsageLog(user, laptop, account, "gpt-6-astra", nil, 9000, base.Add(-48*time.Hour))

	_, err := s.tx.ExecContext(s.ctx, "UPDATE api_keys SET deleted_at = NOW() WHERE id = $1", retired.ID)
	s.Require().NoError(err)

	start := base.Add(-time.Hour)
	end := base.Add(24 * time.Hour)
	filters := usagestats.UsageLogFilters{UserID: user.ID, StartTime: &start, EndTime: &end}
	stats, err := s.repo.KongGetAPIKeyProjectStats(s.ctx, filters, "UTC")
	s.Require().NoError(err)

	s.Require().Len(stats.APIKeys, 2)
	names := map[int64]service.KongAPIKeyProjectKey{}
	for _, key := range stats.APIKeys {
		names[key.APIKeyID] = key
	}
	s.Require().Equal("laptop", names[laptop.ID].APIKeyName)
	s.Require().False(names[laptop.ID].Deleted)
	s.Require().Equal("retired", names[retired.ID].APIKeyName)
	s.Require().True(names[retired.ID].Deleted)

	type groupKey struct {
		keyID              int64
		model, tier, label string
	}
	got := map[groupKey]service.KongAPIKeyProjectRow{}
	for _, row := range stats.Rows {
		got[groupKey{row.APIKeyID, row.Model, row.Tier, row.Bucket}] = row
	}
	s.Require().Len(got, 5)
	merged := got[groupKey{laptop.ID, "gpt-6-astra", "standard", "2026-10-01 12:00"}]
	s.Require().Equal(int64(2), merged.Requests)
	s.Require().Equal(int64(300), merged.InputTokens)
	s.Require().Equal(int64(300), merged.CacheReadTokens)
	s.Require().InDelta(1.0, merged.Cost, 1e-9)
	s.Require().InDelta(0.5, merged.ActualCost, 1e-9)
	s.Require().Equal(int64(1), got[groupKey{laptop.ID, "gpt-6-astra", "fast", "2026-10-01 12:00"}].Requests)
	s.Require().Equal(int64(1), got[groupKey{laptop.ID, "gpt-6.1-sol", "standard", "2026-10-01 12:00"}].Requests)
	s.Require().Equal(int64(1), got[groupKey{laptop.ID, "gpt-6-astra", "standard", "2026-10-01 13:00"}].Requests)
	s.Require().Equal(int64(1), got[groupKey{retired.ID, "gpt-6-astra", "standard", "2026-10-01 12:00"}].Requests)

	keyStats, err := s.repo.KongGetAPIKeyStatsWithUsageFilters(s.ctx, filters)
	s.Require().NoError(err)
	s.Require().Len(keyStats, 2)
	for _, keyStat := range keyStats {
		var requests, tokens int64
		var cost, actualCost float64
		for _, row := range stats.Rows {
			if row.APIKeyID != keyStat.APIKeyID {
				continue
			}
			requests += row.Requests
			tokens += row.InputTokens + row.OutputTokens + row.CacheCreationTokens + row.CacheReadTokens
			cost += row.Cost
			actualCost += row.ActualCost
		}
		s.Require().Equal(keyStat.Requests, requests)
		s.Require().Equal(keyStat.TotalTokens, tokens)
		s.Require().InDelta(keyStat.Cost, cost, 1e-9)
		s.Require().InDelta(keyStat.ActualCost, actualCost, 1e-9)
	}
}

// 小时桶按传入的时区划分，与连接时区无关；可查范围用同一个时区。
func (s *UsageLogRepoSuite) TestKongAPIKeyProjectStatsBucketsInGivenTimezone() {
	user := mustCreateUser(s.T(), s.client, &service.User{Email: "kong-project-tz@test.com"})
	apiKey := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-kong-project-tz", Name: "tz"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-kong-project-tz"})

	at := time.Date(2026, 10, 1, 23, 30, 0, 0, time.UTC)
	s.kongCreateProjectUsageLog(user, apiKey, account, "gpt-6-astra", nil, 1, at)

	start := at.Add(-time.Hour)
	end := at.Add(time.Hour)
	for _, conn := range []string{"Asia/Shanghai", "UTC"} {
		_, err := s.tx.ExecContext(s.ctx, "SELECT set_config('TimeZone', $1, true)", conn)
		s.Require().NoError(err)
		for _, tc := range []struct{ tz, bucket, coverageEnd string }{
			{"UTC", "2026-10-01 23:00", "2026-10-02 00:00"},
			{"Asia/Shanghai", "2026-10-02 07:00", "2026-10-02 08:00"},
		} {
			stats, err := s.repo.KongGetAPIKeyProjectStats(s.ctx, usagestats.UsageLogFilters{UserID: user.ID, StartTime: &start, EndTime: &end}, tc.tz)
			s.Require().NoError(err)
			s.Require().Len(stats.Rows, 1)
			s.Require().Equal(tc.bucket, stats.Rows[0].Bucket, "conn=%s tz=%s", conn, tc.tz)
			s.Require().Equal(tc.coverageEnd, stats.Coverage.End, "conn=%s tz=%s", conn, tc.tz)
		}
	}
}

// 可查范围：起点被最早一行夹住，终点被当前时刻夹住；所选日期全部早于最早一行时起点晚于终点；空表时起点是所选起点。
func (s *UsageLogRepoSuite) TestKongAPIKeyProjectStatsCoverage() {
	user := mustCreateUser(s.T(), s.client, &service.User{Email: "kong-project-coverage@test.com"})
	apiKey := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-kong-project-coverage", Name: "coverage"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-kong-project-coverage"})

	// 用很早的日期，这样这一行就是表上最早的一行，不受其他测试数据影响。
	earliest := time.Date(2001, 3, 4, 5, 30, 0, 0, time.UTC)
	s.kongCreateProjectUsageLog(user, apiKey, account, "gpt-6-astra", nil, 1, earliest)

	clamped := func(start, end time.Time) service.KongAPIKeyProjectCoverage {
		stats, err := s.repo.KongGetAPIKeyProjectStats(s.ctx, usagestats.UsageLogFilters{UserID: user.ID, StartTime: &start, EndTime: &end}, "UTC")
		s.Require().NoError(err)
		return stats.Coverage
	}

	coverage := clamped(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC))
	s.Require().Equal("2001-03-04 05:00", coverage.Start)
	s.Require().Equal("2001-12-31 23:00", coverage.End)

	// NOW() 在事务内固定，所以从同一个事务取，比较到小时。
	var currentHour string
	s.Require().NoError(scanSingleRow(s.ctx, s.tx, "SELECT TO_CHAR(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:00')", nil, &currentHour))
	coverage = clamped(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().AddDate(1, 0, 0))
	s.Require().Equal(currentHour, coverage.End)

	coverage = clamped(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1999, 1, 2, 0, 0, 0, 0, time.UTC))
	s.Require().Greater(coverage.Start, coverage.End)

	_, err := s.tx.ExecContext(s.ctx, "DELETE FROM usage_logs")
	s.Require().NoError(err)
	coverage = clamped(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC))
	s.Require().Equal("2000-01-01 00:00", coverage.Start)
	s.Require().Equal("2000-01-01 23:00", coverage.End)
}

func kongProjectTier(tier string) *string {
	return &tier
}
