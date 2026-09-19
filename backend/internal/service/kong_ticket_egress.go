package service

import (
	"fmt"
	"time"
)

// 票据出口的标识与可用性判定。设计见 DESIGN-codex-ticket.md §3.1 / §3.2。

// KongEgressKey 返回出口的稳定标识，用于事件表与静默统计。
// 不同代理要能分开计静默，所以带上 id；直连是全局唯一的一个出口。
func KongEgressKey(egress KongTicketEgress, proxyID *int64) string {
	switch egress {
	case KongTicketEgressDirect:
		return "direct"
	case KongTicketEgressProxy:
		if proxyID != nil {
			return fmt.Sprintf("proxy:%d", *proxyID)
		}
		return "proxy:unset"
	default:
		return "none"
	}
}

// KongTrafficEgressKey 返回流量出口的标识。账号的 proxy_id 为空即直连。
func KongTrafficEgressKey(proxyID *int64) string {
	if proxyID == nil {
		return "direct"
	}
	return fmt.Sprintf("proxy:%d", *proxyID)
}

// KongTicketProxyState 是票据代理此刻的状态。
type KongTicketProxyState struct {
	// Exists 为假表示这个 id 指向的代理已经不存在。配置存在 accounts.extra 里没有外键
	// 替我们清空失效的 id，所以这是预期会出现的状态，不是异常。
	Exists bool
	// ExpiresAt 为空表示不过期。上游的到期清扫只改写 accounts.proxy_id，**不认识**
	// 我们的键，所以票据代理到期不会触发任何自动处理——必须自己查。
	ExpiresAt *time.Time
	// Disabled 表示代理被显式停用。
	Disabled bool
}

// 出口不可用的具体原因，用于事件与界面提示。
const (
	KongEgressReasonNone         = "not_configured"  // 配置为 none
	KongEgressReasonProxyMissing = "proxy_missing"   // 代理不存在（多半已被删除）
	KongEgressReasonProxyExpired = "proxy_expired"   // 代理已到期
	KongEgressReasonProxyOff     = "proxy_disabled"  // 代理被停用
	KongEgressReasonSameAsProxy  = "same_as_traffic" // 与流量出口是同一个代理
	KongEgressReasonBothDirect   = "both_direct"     // 票据走直连，业务也走直连
)

// KongEvaluateEgress 判断此刻能否用这个票据出口取票，不可用时给出原因。
//
// 这个判定必须在**每次取票前**做，保存时的校验只是提前发现错误——业务出口可以在无人保存票据
// 配置的情况下改变（管理员改账号的 proxy_id、删代理，或上游的到期清扫按 fallback 策略把账号
// 自动改投为直连），那几条路径都不经过票据配置的保存。
//
// 出口一旦与流量出口合并，业务流量就会持续清零票据出口的静默，主动取票必然拿回坏票，而表象
// 只是「一直拿不到合格票」。
func KongEvaluateEgress(cfg KongTicketConfig, trafficProxyID *int64, state KongTicketProxyState, now time.Time) (bool, string) {
	switch cfg.Egress {
	case KongTicketEgressNone:
		return false, KongEgressReasonNone

	case KongTicketEgressDirect:
		// 业务也走直连时两者是同一个出口：服务器本机 IP。
		if trafficProxyID == nil {
			return false, KongEgressReasonBothDirect
		}
		return true, ""

	case KongTicketEgressProxy:
		if cfg.ProxyID == nil || !state.Exists {
			return false, KongEgressReasonProxyMissing
		}
		if state.Disabled {
			return false, KongEgressReasonProxyOff
		}
		if state.ExpiresAt != nil && !state.ExpiresAt.After(now) {
			return false, KongEgressReasonProxyExpired
		}
		if trafficProxyID != nil && *trafficProxyID == *cfg.ProxyID {
			return false, KongEgressReasonSameAsProxy
		}
		return true, ""

	default:
		return false, KongEgressReasonNone
	}
}

// KongApplyTicketConfig 产出要合并进 accounts.extra 的键值对。
//
// 上游的 UpdateExtra 走 JSONB 合并（原子，避免读-改-写丢掉并发写入的其它键），所以这里只回
// 本功能的那几个键、不回整份 extra。
//
// 合并语义下删不掉键，所以 egress 不是 proxy 时把 proxy id 显式设为 JSON null——解析侧把 null
// 与缺失同等对待，界面也就不会显示成「还配着代理」。
func KongApplyTicketConfig(extra map[string]any, cfg KongTicketConfig) map[string]any {
	if extra == nil {
		extra = map[string]any{}
	}
	for k, v := range KongTicketConfigPatch(cfg) {
		extra[k] = v
	}
	return extra
}

// KongTicketConfigPatch 返回**只含本功能三个键**的补丁，供写库时使用。
//
// 写库必须用它，不能把整份 extra 快照传下去：仓储那一步是 JSONB 合并，右侧带上全部旧键时，
// 读取之后别处并发写入的键（上游配置、额度快照……）会被我们手里的旧值覆盖掉。这种覆盖不会
// 报错，只会让另一个功能的设置莫名其妙地回退。
//
// proxy id 在非 proxy 出口下写的是显式 null 而不是省略：JSONB 合并删不掉键，省略等于留着
// 上一次的残值，下次解析会读出一个早已不该生效的代理。
func KongTicketConfigPatch(cfg KongTicketConfig) map[string]any {
	patch := map[string]any{
		KongTicketModeKey:   string(cfg.Mode),
		KongTicketEgressKey: string(cfg.Egress),
	}
	if cfg.Egress == KongTicketEgressProxy && cfg.ProxyID != nil {
		patch[KongTicketProxyIDKey] = *cfg.ProxyID
	} else {
		patch[KongTicketProxyIDKey] = nil
	}
	return patch
}

// KongValidateTicketConfig 是保存时的校验。它挡不住后续的出口漂移（那要靠
// KongEvaluateEgress 在取票前判定），只负责把当场就能看出的错误拦下来。
func KongValidateTicketConfig(cfg KongTicketConfig, trafficProxyID *int64) error {
	switch cfg.Mode {
	case KongTicketModeOff, KongTicketModeObserve, KongTicketModeFull:
	default:
		return fmt.Errorf("未知的模式 %q", cfg.Mode)
	}
	switch cfg.Egress {
	case KongTicketEgressNone, KongTicketEgressDirect, KongTicketEgressProxy:
	default:
		return fmt.Errorf("未知的票据出口 %q", cfg.Egress)
	}
	if cfg.Egress == KongTicketEgressProxy && (cfg.ProxyID == nil || *cfg.ProxyID <= 0) {
		return fmt.Errorf("票据出口设为 proxy 时必须指定代理")
	}
	if ok, reason := KongEvaluateEgress(cfg, trafficProxyID, KongTicketProxyState{Exists: true}, time.Now()); !ok {
		switch reason {
		case KongEgressReasonSameAsProxy:
			return fmt.Errorf("票据出口与流量出口是同一个代理：业务流量会持续清零它的静默，主动取票必然拿回坏票")
		case KongEgressReasonBothDirect:
			return fmt.Errorf("票据出口设为直连时，账号的流量出口不能也是直连——两者会是同一个本机 IP")
		}
	}
	return nil
}

// KongProxyStateOf 把一个代理记录折成票据功能关心的三件事。
//
// 管理面与取票前检查必须共用它：两边各写一份的结果是「界面显示不可用、取票照样发」，而那种
// 不一致不会报错。到期与停用分开表达而不是合成一个布尔——原因要能显示给运维看。
func KongProxyStateOf(proxy *Proxy, now time.Time) KongTicketProxyState {
	if proxy == nil {
		return KongTicketProxyState{Exists: false}
	}
	state := KongTicketProxyState{Exists: true, ExpiresAt: proxy.ExpiresAt}
	if proxy.Status == StatusExpired || proxy.IsExpired(now) {
		// 归一成「已过期」：ExpiresAt 可能为空（只有状态位标了过期），而调用方按时间判断。
		expired := now.Add(-time.Second)
		state.ExpiresAt = &expired
		return state
	}
	// 非 active 且不是过期，就是被显式停用。
	state.Disabled = !proxy.IsActive()
	return state
}
