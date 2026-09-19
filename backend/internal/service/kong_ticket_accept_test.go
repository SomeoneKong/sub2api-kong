//go:build unit

package service

import (
	"math"
	"testing"
)

// 采纳判据的两条要点：自接受不靠配置写对；概率按白名单求和而不是只看 argmax。
func TestKongTicketAcceptMassAndSelf(t *testing.T) {
	gated := []string{"gpt-6-astra", "gpt-5.6-sol"}
	accept, err := KongParseTicketAccept(gated, "gpt-5.6-sol:gpt-6-astra", nil)
	if err != nil {
		t.Fatalf("解析白名单: %v", err)
	}

	// 自接受由构造函数补齐——配置里没写 astra:astra，它仍然接受自己。
	if got := accept.Of("gpt-6-astra"); len(got) != 1 || got[0] != "gpt-6-astra" {
		t.Errorf("astra 应当只接受自己，得到 %v", got)
	}
	if got := accept.Of("gpt-5.6-sol"); len(got) != 2 {
		t.Errorf("sol 应当接受自己与 astra，得到 %v", got)
	}

	// 证据分散在两个都接受的归因之间：按 argmax 会被拒，按求和应当通过。
	split := map[string]float64{"gpt-6-astra": 0.6, "gpt-5.6-sol": 0.35, "gpt-5.6-luna": 0.05}
	if mass := accept.Mass("gpt-5.6-sol", split); math.Abs(mass-0.95) > 1e-9 {
		t.Errorf("sol 的接受质量应为 0.95，得到 %v", mass)
	}
	if !accept.Accepts("gpt-5.6-sol", split, 0.9) {
		t.Error("0.95 的接受质量应当通过 0.9 的阈值——按 argmax(0.6) 判会造成无谓拒服")
	}
	// 同一份分布对 astra 只有 0.6，必须拒：升级方向单向，不能反着用。
	if accept.Accepts("gpt-6-astra", split, 0.9) {
		t.Error("astra 只接受自己，0.6 不足以放行")
	}

	// 没有分布的票（历史行或落库时没写）一律拒绝。
	if accept.Accepts("gpt-5.6-sol", nil, 0.9) {
		t.Error("没有分布时不得放行")
	}
}

// 两类配置错误都必须让启动失败：它们不会报错，只会表现成「配了没生效」。
func TestKongParseTicketAcceptRejectsDeadConfig(t *testing.T) {
	gated := []string{"gpt-6-astra"}
	if _, err := KongParseTicketAccept(gated, "gpt-5.6-sol:gpt-6-astra", nil); err == nil {
		t.Error("键不在门控集合里时应当报错——那条配置永远不会被用到")
	}
	if _, err := KongParseTicketAccept(gated, "gpt-6-astra", nil); err == nil {
		t.Error("缺冒号的条目应当报错")
	}
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	if _, err := KongParseTicketAccept(gated, "gpt-6-astra:no-such-model", bank); err == nil {
		t.Error("值不在校准资料里时应当报错——闭集归因永远不会判为它")
	}
}

// 阈值非正时必须拒绝。未启用那条装配路径用的是 accept=nil / confidence=0，若在那里放行，
// 任何一处新增调用都会变成静默放行降智票。
func TestKongTicketAcceptRejectsNonPositiveConfidence(t *testing.T) {
	accept := KongTicketAccept{"gpt-6-astra": []string{"gpt-6-astra"}}
	probs := map[string]float64{"gpt-6-astra": 1.0}
	if accept.Accepts("gpt-6-astra", probs, 0) {
		t.Error("confidence=0 时不得放行")
	}
	var nilAccept KongTicketAccept
	if nilAccept.Accepts("gpt-6-astra", probs, 0) {
		t.Error("未启用装配（accept=nil, confidence=0）不得放行")
	}
}
