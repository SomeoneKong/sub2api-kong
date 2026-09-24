package repository

import (
	"database/sql"
	"encoding/json"
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// usage_logs 的 kong_request_features（JSONB）读写。fork 专有，见
// service/kong_request_features.go。
//
// 单独一个文件：这一列的编解码是 fork 加的，混进上游那两个每周都在改的大文件里只会增加 rebase 面。

// nullKongRequestFeaturesJSON 把特征序列化成 JSONB 参数。没有特征时写 NULL——**不写 `{}`**：
// 空对象会让"这条请求没采到任何特征"看起来像"采了，只是每项都空"。
func nullKongRequestFeaturesJSON(f *service.KongRequestFeatures) any {
	if f.IsEmpty() {
		return nil
	}
	encoded, err := json.Marshal(f)
	if err != nil {
		// 特征是观测数据，序列化失败不该拖垮这条用量行（它还承载计费）。丢掉这一列并留一条日志。
		slog.Error("kong request features: 序列化失败，该列按 NULL 写入", "error", err)
		return nil
	}
	return string(encoded)
}

// kongRequestFeaturesFromNullJSON 反序列化。解不动就当没有——历史行、以及将来某个版本写进新键
// 都不该让整页用量明细查询失败。
func kongRequestFeaturesFromNullJSON(v sql.NullString) *service.KongRequestFeatures {
	if !v.Valid || v.String == "" {
		return nil
	}
	var out service.KongRequestFeatures
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		slog.Warn("kong request features: 反序列化失败，该行按无特征处理", "error", err)
		return nil
	}
	if out.IsEmpty() {
		return nil
	}
	return &out
}
