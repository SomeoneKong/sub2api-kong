package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newTurnStateTestContext(t *testing.T, apiKeyID int64, sessionID string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if sessionID != "" {
		c.Request.Header.Set("session_id", sessionID)
	}
	if apiKeyID > 0 {
		c.Set("api_key", &APIKey{ID: apiKeyID})
	}
	return c, rec
}

func TestOpenAICodexTurnStateSeed(t *testing.T) {
	c, _ := newTurnStateTestContext(t, 7, "sess-1")
	require.Equal(t, "7\x00sess-1", openAICodexTurnStateSeed(c))

	// 连字符形式优先（Codex CLI 标准头）
	c.Request.Header.Set("session-id", "sess-hyphen")
	require.Equal(t, "7\x00sess-hyphen", openAICodexTurnStateSeed(c))

	// 无会话标识 → 不跟踪
	cNoSession, _ := newTurnStateTestContext(t, 7, "")
	require.Empty(t, openAICodexTurnStateSeed(cNoSession))

	require.Empty(t, openAICodexTurnStateSeed(nil))
}

func TestRelayOpenAICodexTurnState_SetsHeaderAndRecordsProvenance(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42}
	c, _ := newTurnStateTestContext(t, 7, "sess-relay")

	upstream := http.Header{}
	upstream.Set("x-codex-turn-state", "blob-A")
	svc.relayOpenAICodexTurnState(c, account, upstream)

	require.Equal(t, "blob-A", c.Writer.Header().Get("X-Codex-Turn-State"))

	raw, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey("7\x00sess-relay", "blob-A"))
	require.True(t, ok)
	origin, ok := raw.(openAICodexTurnStateOrigin)
	require.True(t, ok)
	require.Equal(t, int64(42), origin.accountID)
	require.True(t, origin.expiresAt.After(time.Now()))
}

func TestRelayOpenAICodexTurnState_ClearsStaleValueWhenUpstreamAbsent(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 7, "sess-stale")
	// 模拟上一 failover attempt 残留的值
	c.Writer.Header().Set("X-Codex-Turn-State", "blob-old")

	svc.relayOpenAICodexTurnState(c, &Account{ID: 43}, http.Header{})

	require.Empty(t, c.Writer.Header().Get("X-Codex-Turn-State"))
	// 断言整表为空，而不是查某个键：按 blob 记之后键里含 blob 指纹，查一个猜出来的键永远命中不了，
	// 那种断言即使真的发生了错误登记也照样通过。
	require.Zero(t, countOpenAICodexTurnStateOrigins(svc))
}

func TestStageOpenAICodexTurnState_StagedHeaders(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 9, "sess-staged")

	// nil 集合 + 上游有值 → 创建集合并写入，但此刻还不记录溯源
	var staged http.Header
	upstream := http.Header{}
	upstream.Set("x-codex-turn-state", "blob-B")
	stageOpenAICodexTurnState(&staged, upstream)
	require.NotNil(t, staged)
	require.Equal(t, "blob-B", staged.Get("X-Codex-Turn-State"))
	require.Zero(t, countOpenAICodexTurnStateOrigins(svc), "暂存阶段不得记录溯源：该 attempt 仍可能 failover 丢弃")

	// 真正提交时才记录
	svc.noteStagedOpenAICodexTurnStateCommitted(c, &Account{ID: 44}, staged)
	raw, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey("9\x00sess-staged", "blob-B"))
	require.True(t, ok)
	origin, ok := raw.(openAICodexTurnStateOrigin)
	require.True(t, ok)
	require.Equal(t, int64(44), origin.accountID)

	// 上游无值 → 清除已暂存的值；nil 集合保持 nil
	stageOpenAICodexTurnState(&staged, http.Header{})
	require.Empty(t, staged.Get("X-Codex-Turn-State"))
	var nilStaged http.Header
	stageOpenAICodexTurnState(&nilStaged, http.Header{})
	require.Nil(t, nilStaged)
}

// 首输出超时导致 attempt 被丢弃时，溯源不得被该 attempt 污染——否则后续
// 请求会把客户端持有的合法 blob 误判成跨账号回带而剥离。
func TestStagedTurnState_AbandonedAttemptDoesNotPoisonProvenance(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 11, "sess-abandoned")

	// 账号 A 的 attempt 暂存了 blob，但从未提交（首输出超时 → failover）
	var staged http.Header
	upstreamA := http.Header{}
	upstreamA.Set("x-codex-turn-state", "blob-A")
	stageOpenAICodexTurnState(&staged, upstreamA)

	// 账号 B 接手并真正提交
	svc.relayOpenAICodexTurnState(c, &Account{ID: 52}, upstreamA)

	// 客户端回带的 blob 来自 B，出站到 B 时不得被剥离
	h := http.Header{}
	h.Set("x-codex-turn-state", "blob-A")
	svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 52}, h)
	require.Equal(t, "blob-A", h.Get("x-codex-turn-state"))

	raw, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey("11\x00sess-abandoned", "blob-A"))
	require.True(t, ok)
	origin, ok := raw.(openAICodexTurnStateOrigin)
	require.True(t, ok)
	require.Equal(t, int64(52), origin.accountID)
}

func TestNoteStagedOpenAICodexTurnStateCommitted_NoopWithoutState(t *testing.T) {
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 12, "sess-nostate")

	svc.noteStagedOpenAICodexTurnStateCommitted(c, &Account{ID: 60}, nil)
	svc.noteStagedOpenAICodexTurnStateCommitted(c, &Account{ID: 60}, http.Header{"X-Request-Id": []string{"rid"}})

	// 断言整表为空，而不是查某个键：按 blob 记之后键里含 blob 指纹，查一个猜出来的键永远命中不了，
	// 那种断言即使真的发生了错误登记也照样通过。
	require.Zero(t, countOpenAICodexTurnStateOrigins(svc))
}

func TestGuardOpenAICodexTurnStateEcho(t *testing.T) {
	newOutbound := func(state string) http.Header {
		h := http.Header{}
		if state != "" {
			h.Set("x-codex-turn-state", state)
		}
		return h
	}

	t.Run("same_account_keeps_echo", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g1")
		upstream := http.Header{}
		upstream.Set("x-codex-turn-state", "blob-A")
		svc.relayOpenAICodexTurnState(c, &Account{ID: 42}, upstream)

		h := newOutbound("blob-A")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 42}, h)
		require.Equal(t, "blob-A", h.Get("x-codex-turn-state"))
	})

	t.Run("foreign_account_strips_echo", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g2")
		upstream := http.Header{}
		upstream.Set("x-codex-turn-state", "blob-A")
		svc.relayOpenAICodexTurnState(c, &Account{ID: 42}, upstream)

		// failover 换到账号 43：blob 由 42 铸造，必须剥离
		h := newOutbound("blob-A")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h)
		require.Empty(t, h.Get("x-codex-turn-state"))
	})

	t.Run("no_provenance_passthrough", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g3")
		h := newOutbound("blob-unknown")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h)
		require.Equal(t, "blob-unknown", h.Get("x-codex-turn-state"))
	})

	t.Run("expired_provenance_passthrough_and_pruned", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g4")
		svc.openaiCodexTurnStateOrigins.Store(openAICodexTurnStateKey("7\x00sess-g4", "blob-A"), openAICodexTurnStateOrigin{
			accountID: 42,
			expiresAt: time.Now().Add(-time.Minute),
		})
		h := newOutbound("blob-A")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h)
		require.Equal(t, "blob-A", h.Get("x-codex-turn-state"))
		_, ok := svc.openaiCodexTurnStateOrigins.Load(openAICodexTurnStateKey("7\x00sess-g4", "blob-A"))
		require.False(t, ok)
	})

	t.Run("no_session_seed_noop", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "")
		h := newOutbound("blob-A")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h)
		require.Equal(t, "blob-A", h.Get("x-codex-turn-state"))
	})

	t.Run("no_echo_noop", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "sess-g5")
		h := newOutbound("")
		svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 43}, h)
		require.Empty(t, h.Get("x-codex-turn-state"))
	})
}

func TestSweepOpenAICodexTurnStateOrigins_PrunesExpiredEntries(t *testing.T) {
	svc := &OpenAIGatewayService{}
	svc.openaiCodexTurnStateOrigins.Store("expired", openAICodexTurnStateOrigin{
		accountID: 1,
		expiresAt: time.Now().Add(-time.Minute),
	})
	svc.openaiCodexTurnStateOrigins.Store("alive", openAICodexTurnStateOrigin{
		accountID: 2,
		expiresAt: time.Now().Add(time.Hour),
	})

	// 清扫逻辑本身：过期的清掉、未过期的留着。
	svc.sweepOpenAICodexTurnStateOriginsNow()

	_, expiredOK := svc.openaiCodexTurnStateOrigins.Load("expired")
	require.False(t, expiredOK)
	_, aliveOK := svc.openaiCodexTurnStateOrigins.Load("alive")
	require.True(t, aliveOK)
}

// 触发点只负责调度：全表遍历不许占住调用方那条 goroutine——登记发生在下行交付路径上（WS 的上游 reader、
// SSE 的写出点），条目又是每个已交付 blob 一条，同步扫会挤掉终帧与 usage。
//
// 用可阻塞的清扫替身来断言，而不是"最终一致 + 读表"：后者在把 go 去掉、改成同步执行时照样通过，测不出回归。
func TestSweepOpenAICodexTurnStateOrigins_RunsOffCallerGoroutine(t *testing.T) {
	svc := &OpenAIGatewayService{}
	// 带缓冲：替身的阻塞有 2s 上限，测试若被调度延迟超过它，替身已经退出，无缓冲的送值就会挂死。
	release := make(chan struct{}, 1)
	started := make(chan struct{}, 4)
	var runs atomic.Int32
	svc.setTurnStateSweepForTest(func() {
		runs.Add(1)
		started <- struct{}{}
		// 有上限地阻塞：清扫若被改回同步执行，触发点会卡在这里，超时让它以断言失败收场而不是挂死。
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
	})
	t.Cleanup(func() {
		close(release)
		svc.setTurnStateSweepForTest(nil)
	})

	// 触发点必须在清扫结束**之前**就返回。
	svc.openaiCodexTurnStateWrites.Store(255)
	svc.sweepOpenAICodexTurnStateOrigins()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("后台清扫没有被调度起来")
	}
	require.Equal(t, int32(1), runs.Load(), "触发点返回时清扫还阻塞着——说明它没有占住调用方 goroutine")

	// 单飞：上一轮还没结束时再触发，不许叠加第二轮。
	//
	// 这里必须等一小会儿再判定"没有第二轮"：goroutine 是异步起的，紧跟着读计数器只会读到"还没来得及跑"，
	// 把单飞判据删掉照样通过。
	svc.openaiCodexTurnStateWrites.Store(255)
	svc.sweepOpenAICodexTurnStateOrigins()
	select {
	case <-started:
		t.Fatal("已有一轮在扫时不该再起一轮")
	case <-time.After(200 * time.Millisecond):
	}

	// 放开之后，下一次触发能正常起来。
	release <- struct{}{}
	require.Eventually(t, func() bool { return !svc.openaiCodexTurnStateSweeping.Load() }, time.Second, 5*time.Millisecond,
		"单飞标记要在清扫结束后复位")
	svc.openaiCodexTurnStateWrites.Store(255)
	svc.sweepOpenAICodexTurnStateOrigins()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("复位之后应当能再起一轮")
	}
	require.Equal(t, int32(2), runs.Load())
}

func TestWriteOpenAIPassthroughResponseHeaders_RelaysAndClearsTurnState(t *testing.T) {
	// filter=nil 走 content-type 兜底分支；turn-state 强制放行不依赖 filter。
	dst := http.Header{}
	src := http.Header{}
	src.Set("X-Codex-Turn-State", "blob-P")
	writeOpenAIPassthroughResponseHeaders(dst, src, nil)
	require.Equal(t, "blob-P", dst.Get("X-Codex-Turn-State"))

	// 上游缺失时清除残留（failover 换号防串扰）
	writeOpenAIPassthroughResponseHeaders(dst, http.Header{"Content-Type": []string{"application/json"}}, nil)
	require.Empty(t, dst.Get("X-Codex-Turn-State"))
}

func TestWriteOpenAIPassthroughResponseHeaders_RelaysReasoningIncluded(t *testing.T) {
	dst := http.Header{}
	src := http.Header{}
	src.Set("X-Reasoning-Included", "1")

	writeOpenAIPassthroughResponseHeaders(
		dst,
		src,
		responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{}),
	)
	require.Equal(t, "1", dst.Get("X-Reasoning-Included"))
}

func TestEnsureOpenAIRemoteCompactionV2BetaFeature(t *testing.T) {
	t.Run("absent_sets_feature", func(t *testing.T) {
		h := http.Header{}
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		require.Equal(t, "remote_compaction_v2", h.Get("x-codex-beta-features"))
	})

	t.Run("present_unchanged", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-codex-beta-features", "responses_websockets_v2, remote_compaction_v2")
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		require.Equal(t, "responses_websockets_v2, remote_compaction_v2", h.Get("x-codex-beta-features"))
	})

	t.Run("other_tokens_merged", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-codex-beta-features", "responses_websockets_v2")
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		require.Equal(t, "responses_websockets_v2,remote_compaction_v2", h.Get("x-codex-beta-features"))
	})

	t.Run("multi_line_values_merged_single_line", func(t *testing.T) {
		h := http.Header{}
		h.Add("x-codex-beta-features", "feature_a")
		h.Add("x-codex-beta-features", "feature_b")
		ensureOpenAIRemoteCompactionV2BetaFeature(h)
		require.Equal(t, []string{"feature_a,feature_b,remote_compaction_v2"}, h.Values("x-codex-beta-features"))
	})
}

// 对齐真实 Codex：该头是会话级常量，挂在 OAuth 的每个请求上，而不是只在
// 压缩回合出现（codex-rs build_model_client_beta_features_header）。
func TestApplyOpenAICodexBetaFeatures(t *testing.T) {
	oauthAccount := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKeyAccount := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	t.Run("oauth_plain_request_gets_default_codex_shape", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		h := http.Header{}
		applyOpenAICodexBetaFeatures(c, oauthAccount, h)
		require.Equal(t, "remote_compaction_v2", h.Get("x-codex-beta-features"),
			"OAuth 的普通请求也必须带会话级 beta 头")
	})

	t.Run("client_declared_header_preserved", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		h := http.Header{}
		h.Set("x-codex-beta-features", "some_other_feature")
		applyOpenAICodexBetaFeatures(c, oauthAccount, h)
		require.Equal(t, "some_other_feature", h.Get("x-codex-beta-features"),
			"客户端显式声明的能力集不得被网关改写（非空即视为用户已关闭 v2）")
	})

	t.Run("native_v2_forces_feature_even_when_client_trimmed_it", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		MarkOpenAINativeCompactionV2(c)
		h := http.Header{}
		h.Set("x-codex-beta-features", "some_other_feature")
		applyOpenAICodexBetaFeatures(c, oauthAccount, h)
		require.Contains(t, h.Get("x-codex-beta-features"), "remote_compaction_v2",
			"body 带 compaction_trigger 是实锤，必须确保 v2 在列")
		require.Contains(t, h.Get("x-codex-beta-features"), "some_other_feature")
	})

	t.Run("native_v2_applies_to_non_oauth_too", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		MarkOpenAINativeCompactionV2(c)
		h := http.Header{}
		applyOpenAICodexBetaFeatures(c, apiKeyAccount, h)
		require.Equal(t, "remote_compaction_v2", h.Get("x-codex-beta-features"))
	})

	t.Run("non_oauth_plain_request_untouched", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		h := http.Header{}
		applyOpenAICodexBetaFeatures(c, apiKeyAccount, h)
		require.Empty(t, h.Get("x-codex-beta-features"),
			"非 Codex 后端不做会话级注入")
	})

	t.Run("nil_account_plain_request_untouched", func(t *testing.T) {
		c, _ := newTurnStateTestContext(t, 7, "sess-beta")
		h := http.Header{}
		applyOpenAICodexBetaFeatures(c, nil, h)
		require.Empty(t, h.Get("x-codex-beta-features"))
	})
}

// WS 握手与 HTTP 出站必须给出同一份会话级 beta 头：真实 Codex 的
// build_websocket_headers 复用 build_responses_headers（client.rs），
// 两侧不一致还会让预热连接与实际请求落进不同的连接池兼容分桶。
func TestBuildOpenAIWSHeaders_CarriesSessionBetaFeatures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	decision := OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}

	build := func(t *testing.T, account *Account, clientBeta string) http.Header {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		if clientBeta != "" {
			c.Request.Header.Set("x-codex-beta-features", clientBeta)
		}
		headers, _, err := svc.buildOpenAIWSHeaders(
			context.Background(), c, account, "test-token", decision,
			true, "", "", "", "gpt-5.6-codex", "",
		)
		require.NoError(t, err)
		return headers
	}

	oauthAccount := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "test-account"},
	}

	headers := build(t, oauthAccount, "")
	require.Equal(t, "remote_compaction_v2", headers.Get("x-codex-beta-features"),
		"WS 握手也必须带会话级 beta 头")

	declared := build(t, oauthAccount, "some_other_feature")
	require.Equal(t, []string{"some_other_feature"}, declared.Values("x-codex-beta-features"),
		"客户端已声明时原样保留")

	apiKeyHeaders := build(t, &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "")
	require.Empty(t, apiKeyHeaders.Get("x-codex-beta-features"),
		"非 Codex 后端不注入")
}

// WS 三条通路的剥离与溯源：原生 WS 上客户端是从带内 `response.metadata` 事件里拿到 state 的，
// 它下次连进来会放在 upgrade 请求头或帧内 client_metadata 上回带。不记这一笔，剥离对 WS 铸造的 state
// 就是空操作；记的粒度不对（只记会话最近账号）则会同时造成误放行与误剥离。
func TestOpenAIWSCodexTurnStateEchoGuard(t *testing.T) {
	metadataFrame := func(state string) []byte {
		return []byte(`{"type":"response.metadata","response":{"headers":{"x-codex-turn-state":"` + state + `"}}}`)
	}
	upgradeWith := func(t *testing.T, session, state string) *gin.Context {
		c, _ := newTurnStateTestContext(t, 7, session)
		c.Request.Header.Set(openAIWSTurnStateHeader, state)
		return c
	}

	t.Run("带内下发的 state 记溯源，换号后回带即剥离", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-1")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, metadataFrame("blob-ws"))

		next := upgradeWith(t, "ws-sess-1", "blob-ws")
		require.Empty(t, svc.openAIWSClientTurnStateForAccount(next, &Account{ID: 43}), "别的账号铸造的 state 不许用")
		// **不得改动客户端那份请求头**：同一个请求里后面还可能重试回原账号。
		require.Equal(t, "blob-ws", next.Request.Header.Get(openAIWSTurnStateHeader))
	})

	t.Run("同账号回带保留", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-2")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, metadataFrame("blob-ws"))

		next := upgradeWith(t, "ws-sess-2", "blob-ws")
		require.Equal(t, "blob-ws", svc.openAIWSClientTurnStateForAccount(next, &Account{ID: 42}))
	})

	t.Run("换号剥离之后重试回原账号，原值仍然可用", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-retry")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, metadataFrame("blob-A"))

		next := upgradeWith(t, "ws-sess-retry", "blob-A")
		require.Empty(t, svc.openAIWSClientTurnStateForAccount(next, &Account{ID: 43}))
		require.Equal(t, "blob-A", svc.openAIWSClientTurnStateForAccount(next, &Account{ID: 42}),
			"B 那次的判定不得把客户端原值抹掉")
	})

	t.Run("同会话并存两个账号的 blob，各判各的", func(t *testing.T) {
		// 同一 session-id 下可以并存父子线程，一次 failover 也会让会话先后持有两个账号的 blob。
		// 按"会话最近账号"记会同时造成误放行（旧 blob 发给新账号）与误剥离（本账号自己的 blob）。
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-mixed")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, metadataFrame("blob-A"))
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 43}, metadataFrame("blob-B"))

		require.Empty(t, svc.openAIWSClientTurnStateForAccount(upgradeWith(t, "ws-sess-mixed", "blob-A"), &Account{ID: 43}),
			"A 的 blob 发给 B 必须剥")
		require.Equal(t, "blob-A", svc.openAIWSClientTurnStateForAccount(upgradeWith(t, "ws-sess-mixed", "blob-A"), &Account{ID: 42}),
			"A 的 blob 发给 A 不许误剥")
		require.Equal(t, "blob-B", svc.openAIWSClientTurnStateForAccount(upgradeWith(t, "ws-sess-mixed", "blob-B"), &Account{ID: 43}))
		require.Equal(t, "blob-unknown", svc.openAIWSClientTurnStateForAccount(upgradeWith(t, "ws-sess-mixed", "blob-unknown"), &Account{ID: 43}),
			"没登记过的 blob 按约定保留")
	})

	t.Run("帧内载体同样受判定", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-frame")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, metadataFrame("blob-A"))

		frame := []byte(`{"type":"response.create","model":"gpt-5.1","client_metadata":{"x-codex-turn-state":"blob-A","keep":"me"}}`)
		stripped, strippedState, err := svc.stripForeignOpenAIWSFrameTurnState(c, &Account{ID: 43}, frame)
		require.NoError(t, err)
		require.Equal(t, "blob-A", strippedState, "被剥掉的原值要交回调用方，请求特征靠它记客户端那一档")
		require.False(t, gjson.GetBytes(stripped, "client_metadata.x-codex-turn-state").Exists(), "异账号的帧内 blob 必须剥")
		require.Equal(t, "me", gjson.GetBytes(stripped, "client_metadata.keep").String(), "其它字段不得受影响")
		require.Equal(t, "blob-A", gjson.GetBytes(frame, "client_metadata.x-codex-turn-state").String(), "不得改动调用方那份")

		kept, keptState, err := svc.stripForeignOpenAIWSFrameTurnState(c, &Account{ID: 42}, frame)
		require.NoError(t, err)
		require.Empty(t, keptState, "没剥就没有被剥值")
		require.Equal(t, "blob-A", gjson.GetBytes(kept, "client_metadata.x-codex-turn-state").String(), "同账号不许剥")
	})

	t.Run("转义写法的键同样要被判定", func(t *testing.T) {
		// 廉价前置排除比的是原始字节，而解析器读的是解码后的键：`x-codex-turn-stat\u0065` 是一段合法
		// JSON，字面串比不中、解码出来却仍是那个键。只按字面串提前返回就等于给了一条绕过通道。
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-escaped")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, metadataFrame("blob-A"))

		frame := []byte(`{"type":"response.create","client_metadata":{"x-codex-turn-stat\u0065":"blob-A"}}`)
		out, stripped, err := svc.stripForeignOpenAIWSFrameTurnState(c, &Account{ID: 43}, frame)
		require.NoError(t, err)
		require.Equal(t, "blob-A", stripped)
		require.NotContains(t, string(out), "blob-A", "异账号的 blob 不得留在帧里")
	})

	t.Run("载体有歧义且含异账号 blob 时拒服", func(t *testing.T) {
		// gjson 读第一处、sjson 也只删第一处，而上游可能按末键解码：删掉第一处之后帧里反而只剩那个
		// 异账号的值，JSON 还变得毫无歧义。所以这种帧不许发，拒服。
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-dup")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, metadataFrame("blob-A"))

		for name, frame := range map[string]string{
			"内层重复键":                `{"client_metadata":{"x-codex-turn-state":"blob-A","x-codex-turn-state":"blob-A"}}`,
			"外层重复 client_metadata": `{"client_metadata":{"x-codex-turn-state":"blob-X"},"client_metadata":{"x-codex-turn-state":"blob-A"}}`,
			"未知值遮住异账号值":            `{"client_metadata":{"x-codex-turn-state":"blob-unknown","x-codex-turn-state":"blob-A"}}`,
		} {
			out, _, err := svc.stripForeignOpenAIWSFrameTurnState(c, &Account{ID: 43}, []byte(frame))
			require.Error(t, err, name)
			require.JSONEq(t, frame, string(out), name+"：拒服时原样返回，不做半截改写")
		}

		// 同样歧义但不含异账号 blob 时不拦：这条路只负责跨账号隔离，帧语义歧义另有 GuardWSFrame 管。
		clean := `{"client_metadata":{"x-codex-turn-state":"blob-unknown","x-codex-turn-state":"blob-unknown2"}}`
		out, _, err := svc.stripForeignOpenAIWSFrameTurnState(c, &Account{ID: 43}, []byte(clean))
		require.NoError(t, err)
		require.JSONEq(t, clean, string(out))
	})

	t.Run("map 载体不得就地改内层 map", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-map")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, metadataFrame("blob-A"))

		shared := map[string]any{"x-codex-turn-state": "blob-A", "keep": "me"}
		payload := map[string]any{"client_metadata": shared}
		require.Equal(t, "blob-A", svc.stripForeignOpenAIWSMapTurnState(c, &Account{ID: 43}, payload))

		next, _ := payload["client_metadata"].(map[string]any)
		require.NotNil(t, next)
		_, present := next["x-codex-turn-state"]
		require.False(t, present, "异账号的 blob 必须剥")
		require.Equal(t, "me", next["keep"])
		require.Equal(t, "blob-A", shared["x-codex-turn-state"], "客户端原始请求体共享的那份内层 map 不得被改")
	})

	t.Run("非 metadata 事件与不带 state 的事件都不记", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		c, _ := newTurnStateTestContext(t, 7, "ws-sess-3")
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, []byte(`{"type":"response.completed"}`))
		svc.noteOpenAIWSCodexTurnStateDelivered(c, &Account{ID: 42}, []byte(`{"type":"response.metadata","response":{"headers":{}}}`))

		require.Zero(t, countOpenAICodexTurnStateOrigins(svc), "这两种事件都不该产生任何登记")

		next := upgradeWith(t, "ws-sess-3", "blob-unknown")
		require.Equal(t, "blob-unknown", svc.openAIWSClientTurnStateForAccount(next, &Account{ID: 43}),
			"没登记过来源就不该剥——无从判定来源却剥，等于无谓丢掉客户端上下文")
	})
}

// countOpenAICodexTurnStateOrigins 数溯源表里的条目。
//
// 否定断言必须用它而不是 Load 某个键：溯源按 blob 记之后键里含 blob 指纹，查一个猜出来的键永远不会命中，
// 那样的断言即使真的发生了错误登记也照样通过。
func countOpenAICodexTurnStateOrigins(svc *OpenAIGatewayService) int {
	n := 0
	svc.openaiCodexTurnStateOrigins.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
