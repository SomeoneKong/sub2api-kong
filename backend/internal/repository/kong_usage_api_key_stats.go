package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 用量页「API Key 使用分布」的查询。fork 专有，见 service/kong_usage_api_key_stats.go。

// KongGetAPIKeyStatsWithUsageFilters 按 API key 汇总符合筛选条件的用量，按 token 总数降序。
// 筛选条件与 GetStatsWithFilters 同一套写法，各 key 的合计因此与用量页统计卡片对得上。
// 先在 usage_logs 上聚合再取 key 名：已删除的 key 是软删除，名字仍在，用 deleted 标出。
func (r *usageLogRepository) KongGetAPIKeyStatsWithUsageFilters(ctx context.Context, filters UsageLogFilters) (results []service.KongAPIKeyStat, err error) {
	conditions, args := kongUsageLogFilterConditions(filters)
	query := fmt.Sprintf(`
		SELECT
			s.api_key_id,
			COALESCE(k.name, '') AS api_key_name,
			COALESCE(k.deleted_at IS NOT NULL, FALSE) AS deleted,
			s.requests,
			s.total_tokens,
			s.cost,
			s.actual_cost
		FROM (
			SELECT
				api_key_id,
				COUNT(*) AS requests,
				COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0) AS total_tokens,
				COALESCE(SUM(total_cost), 0) AS cost,
				COALESCE(SUM(actual_cost), 0) AS actual_cost
			FROM usage_logs
			%s
			GROUP BY api_key_id
		) s
		LEFT JOIN api_keys k ON k.id = s.api_key_id
		ORDER BY s.total_tokens DESC, s.api_key_id
	`, buildWhere(conditions))

	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			results = nil
		}
	}()

	results = make([]service.KongAPIKeyStat, 0)
	for rows.Next() {
		var row service.KongAPIKeyStat
		if err := rows.Scan(
			&row.APIKeyID,
			&row.APIKeyName,
			&row.Deleted,
			&row.Requests,
			&row.TotalTokens,
			&row.Cost,
			&row.ActualCost,
		); err != nil {
			return nil, err
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// kongUsageLogFilterConditions 照 GetStatsWithFilters 的条件构造（上游在各查询里各写一份，没有可复用的函数）。
func kongUsageLogFilterConditions(filters UsageLogFilters) ([]string, []any) {
	conditions := make([]string, 0, 9)
	args := make([]any, 0, 9)

	if filters.UserID > 0 {
		conditions = append(conditions, fmt.Sprintf("user_id = $%d", len(args)+1))
		args = append(args, filters.UserID)
	}
	if filters.APIKeyID > 0 {
		conditions = append(conditions, fmt.Sprintf("api_key_id = $%d", len(args)+1))
		args = append(args, filters.APIKeyID)
	}
	if filters.AccountID > 0 {
		conditions = append(conditions, fmt.Sprintf("account_id = $%d", len(args)+1))
		args = append(args, filters.AccountID)
	}
	if filters.GroupID > 0 {
		conditions = append(conditions, fmt.Sprintf("group_id = $%d", len(args)+1))
		args = append(args, filters.GroupID)
	}
	conditions, args = appendUsageLogModelWhereCondition(conditions, args, filters.Model, filters.ModelFilterSource)
	conditions, args = appendRequestTypeOrStreamWhereCondition(conditions, args, filters.RequestType, filters.Stream)
	conditions, args = appendNativeCompactionV2WhereCondition(conditions, args, filters.NativeCompactionV2, "")
	if filters.BillingType != nil {
		conditions = append(conditions, fmt.Sprintf("billing_type = $%d", len(args)+1))
		args = append(args, int16(*filters.BillingType))
	}
	conditions, args = appendUsageLogBillingModeWhereCondition(conditions, args, filters.BillingMode)
	if filters.UpstreamModelMismatch != nil {
		conditions = append(conditions, upstreamModelMismatchCondition("upstream_model_mismatch", *filters.UpstreamModelMismatch))
	}
	if filters.StartTime != nil {
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", len(args)+1))
		args = append(args, *filters.StartTime)
	}
	if filters.EndTime != nil {
		conditions = append(conditions, fmt.Sprintf("created_at < $%d", len(args)+1))
		args = append(args, *filters.EndTime)
	}
	return conditions, args
}
