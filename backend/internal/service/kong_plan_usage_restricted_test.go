//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 范围化管理员密钥的用量读取：与额度刷新共用账号锁（持锁期间不查询），读数经窗口转换（记过 7d 结构的 Pro 账号，单主
// 窗口长度为 0 时记到 7d 而不是 5h），返回前已同步落库。
func TestKongRestrictedOpenAIUsageProbe(t *testing.T) {
	const id = 901
	a := &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: []int64{1},
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
		Extra:       map[string]any{kongCodexWindowShapeKey: kongCodexShape7d, "codex_7d_used_percent": 99.0}}
	updates := make(chan map[string]any, 1)
	repo := &sparkShadowUsageTestRepo{accounts: map[int64]*Account{id: a}, updateExtraCh: updates}
	hits := make(chan struct{}, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/usage") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		hits <- struct{}{}
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":0,"reset_after_seconds":0}},"rate_limit_reset_credits":{"available_count":0}}`))
	}))
	defer upstream.Close()
	quota := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, nil, nil), newQuotaRedirectingFactory(upstream), nil)
	svc := &AccountUsageService{accountRepo: repo, openAIQuotaService: quota, cache: NewUsageCache()}

	unlock := KongQuotaAccountLock(context.Background(), id)
	done := make(chan error, 1)
	go func() {
		_, err := svc.GetUsageForAccount(scopedStatisticsServiceContext("accounts.usage.read"), a, true)
		done <- err
	}()
	select {
	case <-hits:
		t.Fatal("持锁期间不应查询")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	require.NoError(t, <-done)
	require.Len(t, hits, 1)
	select {
	case got := <-updates:
		require.Contains(t, got, "codex_7d_used_percent")
		require.EqualValues(t, 0, got["codex_7d_used_percent"])
		require.NotContains(t, got, "codex_5h_used_percent")
	default:
		t.Fatal("返回前应已同步落库")
	}
}
