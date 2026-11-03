package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// API Key 项目统计页的查询。fork 专有，设计见 DESIGN-api-key-project-stats.md。
//
// 查询是仓库的可选方法，经接口断言取得，不加进上游的 UsageLogRepository 接口。

// KongAPIKeyProjectCoverage 是所选范围内明细可查的时段，起止都是小时桶标签。
// 范围内没有记录的时段确实没有用量；范围之外的日期明细已清理或还没发生。Start 晚于 End 表示所选日期完全不可查。
type KongAPIKeyProjectCoverage struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// KongAPIKeyProjectKey 是响应里出现过的 API key。Deleted 表示已被删除（软删除，名字仍在）。
type KongAPIKeyProjectKey struct {
	APIKeyID   int64  `json:"api_key_id"`
	APIKeyName string `json:"api_key_name"`
	Deleted    bool   `json:"deleted"`
}

// KongAPIKeyProjectRow 是一个 key、模型、档位在一个小时桶里的用量合计。
// Tier 是用量记录的档位归类：standard、fast、ultrafast、flex。Requests 是用量记录数。
type KongAPIKeyProjectRow struct {
	APIKeyID            int64   `json:"api_key_id"`
	Model               string  `json:"model"`
	Tier                string  `json:"tier"`
	Bucket              string  `json:"bucket"`
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	Cost                float64 `json:"cost"`
	ActualCost          float64 `json:"actual_cost"`
}

// KongAPIKeyProjectStats 是项目统计页一次查询的全部数据，由前端切分展示。
type KongAPIKeyProjectStats struct {
	Coverage KongAPIKeyProjectCoverage `json:"coverage"`
	APIKeys  []KongAPIKeyProjectKey    `json:"api_keys"`
	Rows     []KongAPIKeyProjectRow    `json:"rows"`
}

type kongAPIKeyProjectStatsRepository interface {
	KongGetAPIKeyProjectStats(ctx context.Context, filters usagestats.UsageLogFilters, tz string) (*KongAPIKeyProjectStats, error)
}

var errKongAPIKeyProjectStatsUnsupported = errors.New("usage repository does not support api key project stats")

// KongGetAPIKeyProjectStats 返回筛选范围内各 API key 按模型、档位、小时的用量。
// tz 是划分小时桶用的时区，要与解释 filters 里日期范围的时区相同。
func (s *UsageService) KongGetAPIKeyProjectStats(ctx context.Context, filters usagestats.UsageLogFilters, tz string) (*KongAPIKeyProjectStats, error) {
	repo, ok := s.usageRepo.(kongAPIKeyProjectStatsRepository)
	if !ok {
		return nil, errKongAPIKeyProjectStatsUnsupported
	}
	stats, err := repo.KongGetAPIKeyProjectStats(ctx, filters, tz)
	if err != nil {
		return nil, fmt.Errorf("get api key project stats: %w", err)
	}
	return stats, nil
}
