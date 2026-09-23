package service

import (
	"testing"
	"time"
)

// 出口合并是一类只表现为「一直拿不到合格票」的故障：配置看起来完全合理，业务流量却在持续
// 清零票据出口的静默。而且它的几条成因都**不经过票据配置的保存**，所以判定必须在取票前做。
func TestKongEvaluateEgress(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	id := func(v int64) *int64 { return &v }

	cases := []struct {
		name         string
		cfg          KongTicketConfig
		trafficProxy *int64
		state        KongTicketProxyState
		wantUsable   bool
		wantReason   string
	}{
		{
			name:       "none：没有票据出口可用",
			cfg:        KongTicketConfig{Egress: KongTicketEgressNone},
			wantUsable: false,
			wantReason: KongEgressReasonNone,
		},
		{
			name:         "direct + 业务走代理 → 可用",
			cfg:          KongTicketConfig{Egress: KongTicketEgressDirect},
			trafficProxy: id(5),
			wantUsable:   true,
		},
		{
			name:         "direct + 业务也直连 → 同一个本机 IP",
			cfg:          KongTicketConfig{Egress: KongTicketEgressDirect},
			trafficProxy: nil,
			wantUsable:   false,
			wantReason:   KongEgressReasonBothDirect,
		},
		{
			name:         "proxy + 不同代理 → 可用",
			cfg:          KongTicketConfig{Egress: KongTicketEgressProxy, ProxyID: id(9)},
			trafficProxy: id(5),
			state:        KongTicketProxyState{Exists: true},
			wantUsable:   true,
		},
		{
			name:         "proxy 与流量出口是同一个代理",
			cfg:          KongTicketConfig{Egress: KongTicketEgressProxy, ProxyID: id(5)},
			trafficProxy: id(5),
			state:        KongTicketProxyState{Exists: true},
			wantUsable:   false,
			wantReason:   KongEgressReasonSameAsProxy,
		},
		{
			name:         "代理已被删除：配置里的 id 还在，但指向不存在的代理",
			cfg:          KongTicketConfig{Egress: KongTicketEgressProxy, ProxyID: id(9)},
			trafficProxy: id(5),
			state:        KongTicketProxyState{Exists: false},
			wantUsable:   false,
			wantReason:   KongEgressReasonProxyMissing,
		},
		{
			name:         "代理已到期——上游的到期清扫不认识票据出口这个键",
			cfg:          KongTicketConfig{Egress: KongTicketEgressProxy, ProxyID: id(9)},
			trafficProxy: id(5),
			state:        KongTicketProxyState{Exists: true, ExpiresAt: &past},
			wantUsable:   false,
			wantReason:   KongEgressReasonProxyExpired,
		},
		{
			name:         "代理未到期",
			cfg:          KongTicketConfig{Egress: KongTicketEgressProxy, ProxyID: id(9)},
			trafficProxy: id(5),
			state:        KongTicketProxyState{Exists: true, ExpiresAt: &future},
			wantUsable:   true,
		},
		{
			name:         "代理被停用",
			cfg:          KongTicketConfig{Egress: KongTicketEgressProxy, ProxyID: id(9)},
			trafficProxy: id(5),
			state:        KongTicketProxyState{Exists: true, Disabled: true},
			wantUsable:   false,
			wantReason:   KongEgressReasonProxyOff,
		},
		{
			name:         "业务代理到期被自动改投直连后，票据仍是 proxy → 仍可用（两者不同）",
			cfg:          KongTicketConfig{Egress: KongTicketEgressProxy, ProxyID: id(9)},
			trafficProxy: nil,
			state:        KongTicketProxyState{Exists: true},
			wantUsable:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usable, reason := KongEvaluateEgress(c.cfg, c.trafficProxy, c.state, now)
			if usable != c.wantUsable {
				t.Fatalf("usable = %v（原因 %q），want %v", usable, reason, c.wantUsable)
			}
			if !usable && reason != c.wantReason {
				t.Errorf("reason = %q, want %q", reason, c.wantReason)
			}
			// 不可用时绝不能被误解成「可以退回直连」——直连取票用的是服务器真实 IP。
			if !usable && reason == "" {
				t.Error("不可用时必须给出原因")
			}
		})
	}
}

func TestKongEgressKey(t *testing.T) {
	id := func(v int64) *int64 { return &v }
	cases := []struct {
		egress  KongTicketEgress
		proxyID *int64
		want    string
	}{
		{KongTicketEgressDirect, nil, "direct"},
		{KongTicketEgressProxy, id(12), "proxy:12"},
		{KongTicketEgressProxy, nil, "proxy:unset"},
		{KongTicketEgressNone, nil, "none"},
	}
	for _, c := range cases {
		if got := KongEgressKey(c.egress, c.proxyID); got != c.want {
			t.Errorf("KongEgressKey(%q, %v) = %q, want %q", c.egress, c.proxyID, got, c.want)
		}
	}
	// 不同代理必须是不同的键：它们各自独立积累静默。
	if KongEgressKey(KongTicketEgressProxy, id(1)) == KongEgressKey(KongTicketEgressProxy, id(2)) {
		t.Error("不同代理不能共用一个出口标识")
	}
	if KongTrafficEgressKey(nil) != "direct" {
		t.Error("流量出口为空应当是 direct")
	}
}

// 配置写回 extra 之后必须能被原样读回来，否则保存与生效会对不上。
func TestKongApplyTicketConfigRoundTrip(t *testing.T) {
	id := func(v int64) *int64 { return &v }
	cases := []KongTicketConfig{
		{Mode: KongTicketModeOff, Egress: KongTicketEgressNone},
		{Mode: KongTicketModeObserve, Egress: KongTicketEgressDirect},
		{Mode: KongTicketModeFull, Egress: KongTicketEgressProxy, ProxyID: id(42)},
	}
	for _, cfg := range cases {
		extra := KongApplyTicketConfig(map[string]any{"unrelated": "keep me"}, cfg)
		if extra["unrelated"] != "keep me" {
			t.Error("写配置时把 extra 里的其它键弄丢了")
		}
		got, rejected := ParseKongTicketConfig(extra)
		if len(rejected) != 0 {
			t.Errorf("%v: 自己写出的配置被自己判为非法: %v", cfg, rejected)
		}
		if got.Mode != cfg.Mode || got.Egress != cfg.Egress {
			t.Errorf("回读得到 mode=%q egress=%q，写入的是 %q / %q", got.Mode, got.Egress, cfg.Mode, cfg.Egress)
		}
		switch {
		case cfg.ProxyID == nil && got.ProxyID != nil:
			t.Errorf("回读多出了 proxy id %d", *got.ProxyID)
		case cfg.ProxyID != nil && (got.ProxyID == nil || *got.ProxyID != *cfg.ProxyID):
			t.Errorf("proxy id 回读不一致")
		}
	}

	// 从 proxy 切回 none 时，残留的 proxy id 必须被清掉。
	extra := KongApplyTicketConfig(map[string]any{}, KongTicketConfig{
		Mode: KongTicketModeFull, Egress: KongTicketEgressProxy, ProxyID: id(7),
	})
	extra = KongApplyTicketConfig(extra, KongTicketConfig{Mode: KongTicketModeFull, Egress: KongTicketEgressNone})
	// JSONB 合并删不掉键，所以这里是显式的 null；解析侧必须把它当作「未配置」。
	if v, ok := extra[KongTicketProxyIDKey]; !ok || v != nil {
		t.Errorf("切回 none 后 proxy id 应为显式 null，实际 %#v（存在=%v）", v, ok)
	}
	if cfg, rejected := ParseKongTicketConfig(extra); cfg.ProxyID != nil || len(rejected) != 0 {
		t.Errorf("显式 null 应被当作未配置，得到 ProxyID=%v rejected=%v", cfg.ProxyID, rejected)
	}
}

func TestKongValidateTicketConfig(t *testing.T) {
	id := func(v int64) *int64 { return &v }
	if err := KongValidateTicketConfig(KongTicketConfig{
		Mode: KongTicketModeFull, Egress: KongTicketEgressProxy, ProxyID: id(9),
	}, id(5)); err != nil {
		t.Errorf("合法配置被拒: %v", err)
	}
	bad := []struct {
		name         string
		cfg          KongTicketConfig
		trafficProxy *int64
	}{
		{"proxy 但没给 id", KongTicketConfig{Mode: KongTicketModeFull, Egress: KongTicketEgressProxy}, id(5)},
		{"与流量出口同代理", KongTicketConfig{Mode: KongTicketModeFull, Egress: KongTicketEgressProxy, ProxyID: id(5)}, id(5)},
		{"都走直连", KongTicketConfig{Mode: KongTicketModeFull, Egress: KongTicketEgressDirect}, nil},
		{"未知模式", KongTicketConfig{Mode: "turbo", Egress: KongTicketEgressNone}, nil},
		{"未知出口", KongTicketConfig{Mode: KongTicketModeOff, Egress: "socks5"}, nil},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := KongValidateTicketConfig(c.cfg, c.trafficProxy); err == nil {
				t.Error("应当被拒绝")
			}
		})
	}
}

// 出口冲突只拦 full：流量代理被改成与票据代理相同之后，账号仍要能退回 off / observe。
func TestKongValidateTicketConfigAllowsLeavingFullWithConflictingEgress(t *testing.T) {
	id := func(v int64) *int64 { return &v }
	conflicts := []struct {
		name         string
		egress       KongTicketEgress
		proxyID      *int64
		trafficProxy *int64
	}{
		{"与流量出口同代理", KongTicketEgressProxy, id(5), id(5)},
		{"都走直连", KongTicketEgressDirect, nil, nil},
	}
	for _, c := range conflicts {
		for _, mode := range []KongTicketMode{KongTicketModeOff, KongTicketModeObserve} {
			cfg := KongTicketConfig{Mode: mode, Egress: c.egress, ProxyID: c.proxyID}
			if err := KongValidateTicketConfig(cfg, c.trafficProxy); err != nil {
				t.Errorf("%s / %s 应当可以保存: %v", c.name, mode, err)
			}
		}
		full := KongTicketConfig{Mode: KongTicketModeFull, Egress: c.egress, ProxyID: c.proxyID}
		if err := KongValidateTicketConfig(full, c.trafficProxy); err == nil {
			t.Errorf("%s / full 仍应被拒绝", c.name)
		}
	}
}
