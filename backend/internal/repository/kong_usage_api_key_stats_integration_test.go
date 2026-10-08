//go:build integration

package repository

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 按 API key 汇总：只算本用户、只算时间范围内，软删除的 key 仍按名字列出并标 deleted，按 token 降序。
func (s *UsageLogRepoSuite) TestKongAPIKeyStatsWithUsageFilters() {
	user := mustCreateUser(s.T(), s.client, &service.User{Email: "kong-key-stats@test.com"})
	other := mustCreateUser(s.T(), s.client, &service.User{Email: "kong-key-stats-other@test.com"})
	laptop := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-kong-key-stats-laptop", Name: "laptop"})
	server := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-kong-key-stats-server", Name: "server"})
	retired := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-kong-key-stats-retired", Name: "retired"})
	otherKey := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: other.ID, Key: "sk-kong-key-stats-other", Name: "other"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "acc-kong-key-stats"})

	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	s.createUsageLog(user, laptop, account, 100, 50, 0.5, base)
	s.createUsageLog(user, laptop, account, 200, 50, 1.0, base.Add(time.Hour))
	s.createUsageLog(user, server, account, 1000, 500, 3.0, base.Add(2*time.Hour))
	s.createUsageLog(user, retired, account, 10, 5, 0.1, base.Add(3*time.Hour))
	s.createUsageLog(other, otherKey, account, 9000, 9000, 9.0, base)
	s.createUsageLog(user, laptop, account, 5000, 5000, 5.0, base.Add(-48*time.Hour))

	_, err := s.tx.ExecContext(s.ctx, "UPDATE api_keys SET deleted_at = NOW() WHERE id = $1", retired.ID)
	s.Require().NoError(err)

	start := base.Add(-time.Hour)
	end := base.Add(24 * time.Hour)
	stats, err := s.repo.KongGetAPIKeyStatsWithUsageFilters(s.ctx, usagestats.UsageLogFilters{
		UserID:    user.ID,
		StartTime: &start,
		EndTime:   &end,
	})
	s.Require().NoError(err)
	s.Require().Len(stats, 3)

	s.Require().Equal(server.ID, stats[0].APIKeyID)
	s.Require().Equal("server", stats[0].APIKeyName)
	s.Require().Equal(int64(1), stats[0].Requests)
	s.Require().Equal(int64(1500), stats[0].TotalTokens)

	s.Require().Equal(laptop.ID, stats[1].APIKeyID)
	s.Require().Equal(int64(2), stats[1].Requests)
	s.Require().Equal(int64(400), stats[1].TotalTokens)
	s.Require().InDelta(1.5, stats[1].ActualCost, 1e-9)
	s.Require().False(stats[1].Deleted)

	s.Require().Equal(retired.ID, stats[2].APIKeyID)
	s.Require().Equal("retired", stats[2].APIKeyName)
	s.Require().True(stats[2].Deleted)

	only, err := s.repo.KongGetAPIKeyStatsWithUsageFilters(s.ctx, usagestats.UsageLogFilters{
		UserID:    user.ID,
		APIKeyID:  laptop.ID,
		StartTime: &start,
		EndTime:   &end,
	})
	s.Require().NoError(err)
	s.Require().Len(only, 1)
	s.Require().Equal(laptop.ID, only[0].APIKeyID)
}
