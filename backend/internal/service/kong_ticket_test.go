package service

import (
	"encoding/json"
	"testing"
)

// 配置解析的保守回退失效时是静默的：非法取值若落到 direct，系统会用服务器真实 IP 去打票，
// 而「把账号与服务器真实出口关联起来」是不可逆的。没有报错、没有崩溃，所以必须有测试兜着。
func TestParseKongTicketConfigFallsBackConservatively(t *testing.T) {
	cases := []struct {
		name         string
		extra        map[string]any
		wantMode     KongTicketMode
		wantEgress   KongTicketEgress
		wantProxyID  *int64
		wantRejected []string
	}{
		{
			name:       "空配置即完全关闭",
			extra:      map[string]any{},
			wantMode:   KongTicketModeOff,
			wantEgress: KongTicketEgressNone,
		},
		{
			name:       "nil map 不 panic",
			extra:      nil,
			wantMode:   KongTicketModeOff,
			wantEgress: KongTicketEgressNone,
		},
		{
			name: "合法的 full + proxy",
			extra: map[string]any{
				KongTicketModeKey:    "full",
				KongTicketEgressKey:  "proxy",
				KongTicketProxyIDKey: float64(12), // jsonb 解出来的数字是 float64
			},
			wantMode:    KongTicketModeFull,
			wantEgress:  KongTicketEgressProxy,
			wantProxyID: kongInt64Ptr(12),
		},
		{
			name: "合法的 observe + direct",
			extra: map[string]any{
				KongTicketModeKey:   "observe",
				KongTicketEgressKey: "direct",
			},
			wantMode:   KongTicketModeObserve,
			wantEgress: KongTicketEgressDirect,
		},
		{
			name:         "mode 非法 → off，并报告该键",
			extra:        map[string]any{KongTicketModeKey: "FULL"}, // 大小写敏感，不做宽容解析
			wantMode:     KongTicketModeOff,
			wantEgress:   KongTicketEgressNone,
			wantRejected: []string{KongTicketModeKey},
		},
		{
			name: "egress 非法 → none，绝不能是 direct",
			extra: map[string]any{
				KongTicketModeKey:   "full",
				KongTicketEgressKey: "socks5",
			},
			wantMode:     KongTicketModeFull,
			wantEgress:   KongTicketEgressNone,
			wantRejected: []string{KongTicketEgressKey},
		},
		{
			name: "egress=proxy 但 id 缺失 → none，绝不能退化成 direct",
			extra: map[string]any{
				KongTicketModeKey:   "full",
				KongTicketEgressKey: "proxy",
			},
			wantMode:     KongTicketModeFull,
			wantEgress:   KongTicketEgressNone,
			wantRejected: []string{KongTicketProxyIDKey},
		},
		{
			name: "代理被删除后留下的 id=0 → none",
			extra: map[string]any{
				KongTicketModeKey:    "full",
				KongTicketEgressKey:  "proxy",
				KongTicketProxyIDKey: float64(0),
			},
			wantMode:     KongTicketModeFull,
			wantEgress:   KongTicketEgressNone,
			wantRejected: []string{KongTicketProxyIDKey},
		},
		{
			name: "id 是字符串时也接受（手工写入 extra 的情形）",
			extra: map[string]any{
				KongTicketModeKey:    "full",
				KongTicketEgressKey:  "proxy",
				KongTicketProxyIDKey: " 7 ",
			},
			wantMode:    KongTicketModeFull,
			wantEgress:  KongTicketEgressProxy,
			wantProxyID: kongInt64Ptr(7),
		},
		{
			name: "id 是不可解析的字符串 → none",
			extra: map[string]any{
				KongTicketModeKey:    "full",
				KongTicketEgressKey:  "proxy",
				KongTicketProxyIDKey: "twelve",
			},
			wantMode:     KongTicketModeFull,
			wantEgress:   KongTicketEgressNone,
			wantRejected: []string{KongTicketProxyIDKey},
		},
		{
			name: "前后空白不影响解析",
			extra: map[string]any{
				KongTicketModeKey:   "  full  ",
				KongTicketEgressKey: " direct ",
			},
			wantMode:   KongTicketModeFull,
			wantEgress: KongTicketEgressDirect,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, rejected := ParseKongTicketConfig(c.extra)
			if cfg.Mode != c.wantMode {
				t.Errorf("Mode = %q, want %q", cfg.Mode, c.wantMode)
			}
			if cfg.Egress != c.wantEgress {
				t.Errorf("Egress = %q, want %q", cfg.Egress, c.wantEgress)
			}
			// 这一条单独强调：任何解析失败都不得产生 direct。
			if c.wantEgress != KongTicketEgressDirect && cfg.Egress == KongTicketEgressDirect {
				t.Fatalf("解析失败时落到了 direct——会用服务器真实 IP 打票")
			}
			switch {
			case c.wantProxyID == nil && cfg.ProxyID != nil:
				t.Errorf("ProxyID = %d, want nil", *cfg.ProxyID)
			case c.wantProxyID != nil && cfg.ProxyID == nil:
				t.Errorf("ProxyID = nil, want %d", *c.wantProxyID)
			case c.wantProxyID != nil && *cfg.ProxyID != *c.wantProxyID:
				t.Errorf("ProxyID = %d, want %d", *cfg.ProxyID, *c.wantProxyID)
			}
			if len(rejected) != len(c.wantRejected) {
				t.Fatalf("rejected = %v, want %v", rejected, c.wantRejected)
			}
			for i := range rejected {
				if rejected[i] != c.wantRejected[i] {
					t.Errorf("rejected[%d] = %q, want %q", i, rejected[i], c.wantRejected[i])
				}
			}
		})
	}
}

func kongInt64Ptr(v int64) *int64 { return &v }

// 配置项类型写错时，光有保守回退不够——还必须能被看见。第二个返回值漏报的话，写错的配置会
// 静默按缺省值生效，运维查「为什么这个账号不取票」时没有任何线索。
func TestParseKongTicketConfigReportsWrongTypes(t *testing.T) {
	cases := []struct {
		name         string
		extra        map[string]any
		wantMode     KongTicketMode
		wantEgress   KongTicketEgress
		wantRejected []string
	}{
		{
			name:         "mode 是布尔",
			extra:        map[string]any{KongTicketModeKey: true},
			wantMode:     KongTicketModeOff,
			wantEgress:   KongTicketEgressNone,
			wantRejected: []string{KongTicketModeKey},
		},
		{
			name:         "egress 是数字",
			extra:        map[string]any{KongTicketModeKey: "full", KongTicketEgressKey: float64(1)},
			wantMode:     KongTicketModeFull,
			wantEgress:   KongTicketEgressNone,
			wantRejected: []string{KongTicketEgressKey},
		},
		{
			name:         "两个键同时类型错误",
			extra:        map[string]any{KongTicketModeKey: []any{"full"}, KongTicketEgressKey: map[string]any{}},
			wantMode:     KongTicketModeOff,
			wantEgress:   KongTicketEgressNone,
			wantRejected: []string{KongTicketModeKey, KongTicketEgressKey},
		},
		{
			name:       "显式 null 等同于未配置",
			extra:      map[string]any{KongTicketModeKey: nil, KongTicketEgressKey: nil},
			wantMode:   KongTicketModeOff,
			wantEgress: KongTicketEgressNone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, rejected := ParseKongTicketConfig(c.extra)
			if cfg.Mode != c.wantMode || cfg.Egress != c.wantEgress {
				t.Errorf("得到 mode=%q egress=%q，想要 mode=%q egress=%q", cfg.Mode, cfg.Egress, c.wantMode, c.wantEgress)
			}
			if len(rejected) != len(c.wantRejected) {
				t.Fatalf("rejected = %v, want %v", rejected, c.wantRejected)
			}
			for i := range rejected {
				if rejected[i] != c.wantRejected[i] {
					t.Errorf("rejected[%d] = %q, want %q", i, rejected[i], c.wantRejected[i])
				}
			}
		})
	}
}

// 代理 id 经 json.Unmarshal 之后一律是 float64。带小数或超出安全整数范围的值若被静默取整，
// 会指向另一个真实存在的代理——取票就走到了错误出口，而出口关联无法事后撤销。
// 这里走真实的 JSON 解码路径，而不是手写 map，因为舍入发生在解码阶段。
func TestParseKongTicketConfigRejectsUnsafeProxyID(t *testing.T) {
	cases := []struct {
		name        string
		json        string
		wantEgress  KongTicketEgress
		wantProxyID *int64
	}{
		{
			name:        "正常整数",
			json:        `{"kong_ticket_mode":"full","kong_ticket_egress":"proxy","kong_ticket_proxy_id":12}`,
			wantEgress:  KongTicketEgressProxy,
			wantProxyID: kongInt64Ptr(12),
		},
		{
			name:       "带小数：12.5 不能变成 12",
			json:       `{"kong_ticket_mode":"full","kong_ticket_egress":"proxy","kong_ticket_proxy_id":12.5}`,
			wantEgress: KongTicketEgressNone,
		},
		{
			name:       "超出安全整数范围：解码阶段已被舍入",
			json:       `{"kong_ticket_mode":"full","kong_ticket_egress":"proxy","kong_ticket_proxy_id":9007199254740993}`,
			wantEgress: KongTicketEgressNone,
		},
		{
			name:        "安全整数上界仍接受",
			json:        `{"kong_ticket_mode":"full","kong_ticket_egress":"proxy","kong_ticket_proxy_id":9007199254740991}`,
			wantEgress:  KongTicketEgressProxy,
			wantProxyID: kongInt64Ptr(9007199254740991),
		},
		{
			name:       "负数",
			json:       `{"kong_ticket_mode":"full","kong_ticket_egress":"proxy","kong_ticket_proxy_id":-3}`,
			wantEgress: KongTicketEgressNone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var extra map[string]any
			if err := json.Unmarshal([]byte(c.json), &extra); err != nil {
				t.Fatalf("测试数据本身不是合法 JSON: %v", err)
			}
			cfg, rejected := ParseKongTicketConfig(extra)
			if cfg.Egress != c.wantEgress {
				t.Errorf("Egress = %q, want %q", cfg.Egress, c.wantEgress)
			}
			switch {
			case c.wantProxyID == nil && cfg.ProxyID != nil:
				t.Errorf("ProxyID = %d, want nil", *cfg.ProxyID)
			case c.wantProxyID != nil && cfg.ProxyID == nil:
				t.Errorf("ProxyID = nil, want %d", *c.wantProxyID)
			case c.wantProxyID != nil && *cfg.ProxyID != *c.wantProxyID:
				t.Errorf("ProxyID = %d, want %d", *cfg.ProxyID, *c.wantProxyID)
			}
			if c.wantEgress == KongTicketEgressNone && len(rejected) == 0 {
				t.Error("非法的 proxy id 被接受了，rejected 为空")
			}
		})
	}
}
