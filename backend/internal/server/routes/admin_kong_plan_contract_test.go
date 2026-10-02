//go:build unit

package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 计划组件管理接口的契约用例：与边车的假网关共用 service/testdata/kong_plan/contract_cases.json，
// 两边任何一边与契约不符，这里或边车的测试就会失败。走真实的路由注册与 handler。

type kongPlanContractRepo struct {
	mu     sync.Mutex
	rows   map[string]service.KongPlanRow
	caller *service.KongPoolCaller
	kinds  map[int64]service.KongPlanAccountKind
}

func (r *kongPlanContractRepo) LoadKongPlan(context.Context) ([]service.KongPlanRow, *service.KongPoolCaller, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]service.KongPlanRow, 0, len(r.rows))
	for _, row := range r.rows {
		out = append(out, row)
	}
	return out, r.caller, nil
}

func (r *kongPlanContractRepo) SaveKongPlanRows(_ context.Context, rows ...service.KongPlanRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		r.rows[row.Kind] = row
	}
	return nil
}

func (r *kongPlanContractRepo) SaveKongPoolCaller(_ context.Context, c service.KongPoolCaller) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caller = &c
	return nil
}

func (r *kongPlanContractRepo) KongPlanAccountKinds(_ context.Context, ids []int64) (map[int64]service.KongPlanAccountKind, error) {
	out := map[int64]service.KongPlanAccountKind{}
	for _, id := range ids {
		if k, ok := r.kinds[id]; ok {
			out[id] = k
		}
	}
	return out, nil
}

type kongPlanContractFile struct {
	Accounts map[string]struct {
		Platform string `json:"platform"`
		Type     string `json:"type"`
	} `json:"accounts"`
	Cases []struct {
		Name  string `json:"name"`
		Steps []struct {
			Method string          `json:"method"`
			Path   string          `json:"path"`
			Body   json.RawMessage `json:"body"`
			Expect struct {
				Status   int               `json:"status"`
				Reason   string            `json:"reason"`
				Metadata map[string]string `json:"metadata"`
				Data     json.RawMessage   `json:"data"`
			} `json:"expect"`
		} `json:"steps"`
	} `json:"cases"`
}

// kongPlanContractNow 把 $now、$now+135m 这类占位替换成带时区的时刻。
func kongPlanContractNow(v any, now time.Time) any {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			x[k] = kongPlanContractNow(item, now)
		}
	case []any:
		for i, item := range x {
			x[i] = kongPlanContractNow(item, now)
		}
	case string:
		if strings.HasPrefix(x, "$now") {
			at := now
			if rest := strings.TrimPrefix(x, "$now"); rest != "" {
				d, err := time.ParseDuration(strings.TrimPrefix(rest, "+"))
				if err != nil {
					panic(fmt.Sprintf("占位 %q 写错了", x))
				}
				at = now.Add(d)
			}
			return at.In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
		}
	}
	return v
}

func kongPlanContractSubset(t *testing.T, path string, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		require.True(t, ok, "%s 应为对象，实际 %v", path, got)
		for k, wv := range w {
			gv, present := g[k]
			require.True(t, present, "%s.%s 缺失", path, k)
			kongPlanContractSubset(t, path+"."+k, wv, gv)
		}
	case []any:
		g, ok := got.([]any)
		require.True(t, ok, "%s 应为数组，实际 %v", path, got)
		require.Len(t, g, len(w), "%s 长度", path)
		for i := range w {
			kongPlanContractSubset(t, fmt.Sprintf("%s[%d]", path, i), w[i], g[i])
		}
	default:
		require.Equal(t, want, got, path)
	}
}

func TestKongPlanContractCases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw, err := os.ReadFile("../../service/testdata/kong_plan/contract_cases.json")
	require.NoError(t, err)
	var file kongPlanContractFile
	require.NoError(t, json.Unmarshal(raw, &file))
	require.NotEmpty(t, file.Cases)

	kinds := map[int64]service.KongPlanAccountKind{}
	for id, a := range file.Accounts {
		var n int64
		_, err := fmt.Sscan(id, &n)
		require.NoError(t, err)
		kinds[n] = service.KongPlanAccountKind{Platform: a.Platform, Type: a.Type}
	}

	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			gen, seq := int64(1), int64(0)
			repo := &kongPlanContractRepo{
				rows: map[string]service.KongPlanRow{"publish": {
					Kind: "publish", Generation: &gen, Seq: &seq,
					Content: []byte(`{"generation": 1, "enabled": true, "last_seq": 0}`),
				}},
				kinds: kinds,
			}
			store := service.NewKongPlanStore(repo)
			store.Start(context.Background())
			h := &handler.Handlers{Admin: &handler.AdminHandlers{KongPlan: admin.NewKongPlanHandler(store, nil, nil, nil)}}
			r := gin.New()
			registerKongPlanRoutes(r.Group("/api/v1/admin"), h)
			now := time.Now() // 每个用例取一次，全部步骤共用：同号重试的内容必须逐字相同

			for i, step := range tc.Steps {
				var body []byte
				if len(step.Body) > 0 {
					var v any
					require.NoError(t, json.Unmarshal(step.Body, &v))
					body, err = json.Marshal(kongPlanContractNow(v, now))
					require.NoError(t, err)
				}
				req := httptest.NewRequest(step.Method, "/api/v1/admin/kong-plan"+step.Path, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				where := fmt.Sprintf("第 %d 步 %s %s：%s", i+1, step.Method, step.Path, w.Body.String())
				require.Equal(t, step.Expect.Status, w.Code, where)

				var resp struct {
					Reason   string            `json:"reason"`
					Metadata map[string]string `json:"metadata"`
					Data     any               `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), where)
				if step.Expect.Reason != "" {
					require.Equal(t, step.Expect.Reason, resp.Reason, where)
				}
				for k, v := range step.Expect.Metadata {
					require.Equal(t, v, resp.Metadata[k], where)
				}
				if len(step.Expect.Data) > 0 {
					var want any
					require.NoError(t, json.Unmarshal(step.Expect.Data, &want))
					kongPlanContractSubset(t, where+" data", want, resp.Data)
				}
			}
		})
	}
}

func TestKongPlanStatsAndCapacityRoutes(t *testing.T) {
	gen, seq := int64(1), int64(0)
	repo := &kongPlanContractRepo{rows: map[string]service.KongPlanRow{"publish": {
		Kind: "publish", Generation: &gen, Seq: &seq, Content: []byte(`{"generation": 1, "enabled": true, "last_seq": 0}`),
	}}}
	store := service.NewKongPlanStore(repo)
	store.Start(context.Background())
	h := &handler.Handlers{Admin: &handler.AdminHandlers{KongPlan: admin.NewKongPlanHandler(store, nil, nil, nil)}}
	r := gin.New()
	registerKongPlanRoutes(r.Group("/api/v1/admin"), h)
	RegisterKongPoolRoutes(r.Group("/api/v1"), h, func(c *gin.Context) { c.Next() })
	get := func(path string) (int, string) {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code, w.Body.String()
	}

	code, body := get("/api/v1/admin/kong-plan/stats")
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"data":[]`, "没有决策记录时返回空表")
	code, body = get("/api/v1/admin/kong-plan/stats?from=2026-10-02T00:00:00%2B08:00&to=2026-10-02T12:00:00%2B08:00")
	require.Equal(t, 200, code, body)
	code, body = get("/api/v1/admin/kong-plan/stats?from=yesterday")
	require.Equal(t, 400, code, body)
	require.Contains(t, body, "KONG_PLAN_INVALID")
	code, body = get("/api/v1/admin/kong-plan/stats?from=2026-09-01T00:00:00Z&to=2026-10-03T00:00:00Z")
	require.Equal(t, 400, code, "跨度超过 31 天："+body)
	code, body = get("/api/v1/pool/capacity")
	require.Equal(t, 503, code, "没有网关服务时不可用："+body)
}
