package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// 用量页「API Key 使用分布」：按 API key 汇总当前筛选条件下的用量。fork 专有。
//
// 查询是仓库的可选方法，经接口断言取得，不加进上游的 UsageLogRepository 接口——那会连带改上游的全部测试桩。

// KongAPIKeyStat 是一个 API key 在所选范围内的用量合计。Deleted 表示该 key 已被删除（软删除，名字仍在）。
type KongAPIKeyStat struct {
	APIKeyID    int64   `json:"api_key_id"`
	APIKeyName  string  `json:"api_key_name"`
	Deleted     bool    `json:"deleted"`
	Requests    int64   `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	Cost        float64 `json:"cost"`
	ActualCost  float64 `json:"actual_cost"`
}

type kongAPIKeyStatsRepository interface {
	KongGetAPIKeyStatsWithUsageFilters(ctx context.Context, filters usagestats.UsageLogFilters) ([]KongAPIKeyStat, error)
}

var errKongAPIKeyStatsUnsupported = errors.New("usage repository does not support api key stats")

// KongGetAPIKeyStatsWithFilters 返回按 API key 汇总的用量，按 token 总数降序。时间范围取 filters 里的 StartTime / EndTime。
func (s *UsageService) KongGetAPIKeyStatsWithFilters(ctx context.Context, filters usagestats.UsageLogFilters) ([]KongAPIKeyStat, error) {
	repo, ok := s.usageRepo.(kongAPIKeyStatsRepository)
	if !ok {
		return nil, errKongAPIKeyStatsUnsupported
	}
	stats, err := repo.KongGetAPIKeyStatsWithUsageFilters(ctx, filters)
	if err != nil {
		return nil, fmt.Errorf("get api key stats with filters: %w", err)
	}
	return stats, nil
}
