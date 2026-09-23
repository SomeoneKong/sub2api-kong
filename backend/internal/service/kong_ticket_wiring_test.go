//go:build unit

package service

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 事件保留天数不合法时拒绝启动，合法时装配到服务上。取值边界见 TestKongEventRetentionBounds。
func TestKongTicketComponentsRejectNonPositiveEventRetention(t *testing.T) {
	t.Setenv(KongTicketGatedModelsEnv, "gpt-6-astra")
	cfg := config.KongCodexTicketConfig{AcceptExtra: config.DefaultKongTicketAcceptExtra, EventRetentionDays: 0}
	if _, err := NewKongTicketComponents(nil, nil, nil, nil, cfg); err == nil ||
		!strings.Contains(err.Error(), config.KongTicketEventRetentionDaysEnv) {
		t.Fatalf("保留天数为 0 应当拒绝启动并指明配置项，得到 %v", err)
	}

	cfg.EventRetentionDays = 90
	comp, err := NewKongTicketComponents(nil, nil, nil, nil, cfg)
	if err != nil {
		t.Fatalf("合法配置不该报错: %v", err)
	}
	if comp.Service.eventRetention != 90*24*time.Hour {
		t.Errorf("保留期 = %v，应为 90 天", comp.Service.eventRetention)
	}
}

// 保留期必须能无回绕地换算成时长，且盖得住调度回看事件表的每个窗口——短于任何一个，清理都会删掉
// 仍在窗口里的事件，调度读不到便提前取票、重试或付费探测。
func TestKongEventRetentionBounds(t *testing.T) {
	day := 24 * time.Hour
	with := func(f func(*KongTicketParams)) KongTicketParams {
		p := KongDefaultTicketParams()
		f(&p)
		return p
	}
	defaults := KongDefaultTicketParams()
	cases := []struct {
		name   string
		days   int
		params KongTicketParams
		ok     bool
	}{
		{"零", 0, defaults, false},
		{"负数", -1, defaults, false},
		{"默认参数下一天即可", 1, defaults, true},
		{"上限本身", kongMaxEventRetentionDays, defaults, true},
		{"上限加一会回绕成负数", kongMaxEventRetentionDays + 1, defaults, false},
		{"两倍上限会回绕成几十分钟", 2 * (kongMaxEventRetentionDays + 1), defaults, false},
		// 回绕后约 30 天，窗口校验挡不住，只能靠上限。
		{"回绕成看似正常的时长", 213534, defaults, false},
		{"短于探测间隔", 1, with(func(p *KongTicketParams) { p.ObserveProbeInterval = 2 * day }), false},
		{"等于探测间隔", 2, with(func(p *KongTicketParams) { p.ObserveProbeInterval = 2 * day }), true},
		{"短于最小静默", 2, with(func(p *KongTicketParams) { p.TicketFetchMinIdle = 3 * day }), false},
		{"等于最小静默", 3, with(func(p *KongTicketParams) { p.TicketFetchMinIdle = 3 * day }), true},
		{"短于失败冷却", 1, with(func(p *KongTicketParams) { p.VerifyFailCooldown = day + time.Second }), false},
		{"盖住失败冷却", 2, with(func(p *KongTicketParams) { p.VerifyFailCooldown = day + time.Second }), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kongEventRetention(tc.days, tc.params)
			if !tc.ok {
				if err == nil {
					t.Fatalf("应当拒绝，却得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该拒绝: %v", err)
			}
			if got != time.Duration(tc.days)*day {
				t.Errorf("保留期 = %v，应为 %d 天", got, tc.days)
			}
		})
	}
}
