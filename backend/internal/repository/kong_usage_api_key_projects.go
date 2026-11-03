package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// API Key 项目统计页的查询。fork 专有，见 service/kong_usage_api_key_projects.go。

const kongAPIKeyProjectBucketFormat = "YYYY-MM-DD HH24:00"

// kongAPIKeyProjectTierExpr 把用量记录的 service_tier 归类，分类与 serviceTierCostMultiplier 一致。
const kongAPIKeyProjectTierExpr = `CASE LOWER(TRIM(COALESCE(r.service_tier, '')))
			WHEN 'priority' THEN 'fast'
			WHEN 'fast' THEN 'fast'
			WHEN 'ultrafast' THEN 'ultrafast'
			WHEN 'flex' THEN 'flex'
			ELSE 'standard'
		END`

// KongGetAPIKeyProjectStats 按 key、模型、档位、小时汇总符合筛选条件的用量，并给出可查范围。
// tz 是划分小时桶的时区，作为绑定参数传入。
func (r *usageLogRepository) KongGetAPIKeyProjectStats(ctx context.Context, filters UsageLogFilters, tz string) (*service.KongAPIKeyProjectStats, error) {
	if filters.StartTime == nil || filters.EndTime == nil {
		return nil, errors.New("api key project stats requires a time range")
	}

	stats, err := r.kongQueryAPIKeyProjectRows(ctx, filters, tz)
	if err != nil {
		return nil, err
	}
	// 放在用量查询之后：可查范围的终点不会早于已返回的最后一个桶。
	if err := r.kongQueryAPIKeyProjectCoverage(ctx, filters, tz, &stats.Coverage); err != nil {
		return nil, err
	}
	return stats, nil
}

func (r *usageLogRepository) kongQueryAPIKeyProjectRows(ctx context.Context, filters UsageLogFilters, tz string) (stats *service.KongAPIKeyProjectStats, err error) {
	conditions, args := kongUsageLogFilterConditions(filters)
	tzParam := fmt.Sprintf("$%d::text", len(args)+1)
	args = append(args, tz)

	// 内层只按原始列分组，归类与格式化放到外层，只处理几千行。
	// MATERIALIZED 让内层独立规划：不写时外层的分组需求会传进内层，PostgreSQL 先把全部明细排序再聚合，耗时翻倍。
	query := fmt.Sprintf(`
		WITH r AS MATERIALIZED (
			SELECT
				api_key_id,
				requested_model,
				model,
				service_tier,
				date_trunc('hour', created_at AT TIME ZONE %s) AS hour,
				COUNT(*) AS requests,
				COALESCE(SUM(input_tokens), 0) AS input_tokens,
				COALESCE(SUM(output_tokens), 0) AS output_tokens,
				COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
				COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
				COALESCE(SUM(total_cost), 0) AS cost,
				COALESCE(SUM(actual_cost), 0) AS actual_cost
			FROM usage_logs
			%s
			GROUP BY 1, 2, 3, 4, 5
		)
		SELECT
			r.api_key_id,
			COALESCE(k.name, '') AS api_key_name,
			COALESCE(k.deleted_at IS NOT NULL, FALSE) AS deleted,
			%s AS model,
			%s AS tier,
			TO_CHAR(r.hour, '%s') AS bucket,
			SUM(r.requests)::bigint,
			SUM(r.input_tokens)::bigint,
			SUM(r.output_tokens)::bigint,
			SUM(r.cache_creation_tokens)::bigint,
			SUM(r.cache_read_tokens)::bigint,
			SUM(r.cost),
			SUM(r.actual_cost)
		FROM r
		LEFT JOIN api_keys k ON k.id = r.api_key_id
		GROUP BY 1, 2, 3, 4, 5, 6
		ORDER BY 1, 6, 4, 5
	`, tzParam, buildWhere(conditions),
		resolveModelDimensionExpressionWithAlias(usagestats.ModelSourceRequested, "r"),
		kongAPIKeyProjectTierExpr, kongAPIKeyProjectBucketFormat)

	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			stats = nil
		}
	}()

	stats = &service.KongAPIKeyProjectStats{
		APIKeys: make([]service.KongAPIKeyProjectKey, 0),
		Rows:    make([]service.KongAPIKeyProjectRow, 0),
	}
	seenKeys := make(map[int64]struct{})
	for rows.Next() {
		var key service.KongAPIKeyProjectKey
		var row service.KongAPIKeyProjectRow
		if err := rows.Scan(
			&row.APIKeyID,
			&key.APIKeyName,
			&key.Deleted,
			&row.Model,
			&row.Tier,
			&row.Bucket,
			&row.Requests,
			&row.InputTokens,
			&row.OutputTokens,
			&row.CacheCreationTokens,
			&row.CacheReadTokens,
			&row.Cost,
			&row.ActualCost,
		); err != nil {
			return nil, err
		}
		if _, seen := seenKeys[row.APIKeyID]; !seen {
			seenKeys[row.APIKeyID] = struct{}{}
			key.APIKeyID = row.APIKeyID
			stats.APIKeys = append(stats.APIKeys, key)
		}
		stats.Rows = append(stats.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stats, nil
}

// kongQueryAPIKeyProjectCoverage 把所选范围夹在全表最早一行与当前时刻之间。
// 保留清理是全表的，所以最早一行不按用户过滤。空表时 MIN 为 NULL，GREATEST 忽略 NULL，起点是所选起点。
func (r *usageLogRepository) kongQueryAPIKeyProjectCoverage(ctx context.Context, filters UsageLogFilters, tz string, coverage *service.KongAPIKeyProjectCoverage) error {
	query := fmt.Sprintf(`
		SELECT
			TO_CHAR(GREATEST($1::timestamptz, MIN(created_at)) AT TIME ZONE $3::text, '%[1]s'),
			TO_CHAR(LEAST($2::timestamptz - INTERVAL '1 microsecond', NOW()) AT TIME ZONE $3::text, '%[1]s')
		FROM usage_logs
	`, kongAPIKeyProjectBucketFormat)
	args := []any{*filters.StartTime, *filters.EndTime, tz}
	return scanSingleRow(ctx, r.sql, query, args, &coverage.Start, &coverage.End)
}
