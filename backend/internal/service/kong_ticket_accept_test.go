//go:build unit

package service

import (
	"math"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 采纳判据的两条要点：自接受不靠配置写对；概率按白名单求和而不是只看 argmax。
func TestKongTicketAcceptMassAndSelf(t *testing.T) {
	gated := []string{"gpt-6-astra", "gpt-5.6-sol"}
	accept, _, err := KongParseTicketAccept(gated, "gpt-5.6-sol:gpt-6-astra", nil)
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

	// 死配置：忽略 + 告警，**不让启动失败**。白名单只会比预期更小，方向是拒服而非放行降智；
	// 而拒绝启动会把配置笔误升级成整个网关不可用。
	accept, warns, err := KongParseTicketAccept(gated, "gpt-5.6-sol:gpt-6-astra", nil)
	if err != nil {
		t.Fatalf("键不在门控集合里不该报错：%v", err)
	}
	if len(warns) != 1 {
		t.Errorf("应当给出一条告警，得到 %v", warns)
	}
	if _, ok := accept["gpt-5.6-sol"]; ok {
		t.Error("被忽略的条目不得进入接受关系")
	}

	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	accept, warns, err = KongParseTicketAccept(gated, "gpt-6-astra:no-such-model", bank)
	if err != nil {
		t.Fatalf("值不在校准资料里不该报错：%v", err)
	}
	if len(warns) != 1 {
		t.Errorf("应当给出一条告警，得到 %v", warns)
	}
	// 忽略掉那一项后 astra 仍然接受自己——自接受由代码补齐，不受配置错误影响。
	if got := accept.Of("gpt-6-astra"); len(got) != 1 || got[0] != "gpt-6-astra" {
		t.Errorf("astra 应仍只接受自己，得到 %v", got)
	}

	// 格式错误仍然报错：整条配置没被解析成任何东西，静默忽略会让人以为配上了。
	if _, _, err := KongParseTicketAccept(gated, "gpt-6-astra", nil); err == nil {
		t.Error("缺冒号的条目应当报错")
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

// 内置默认必须自成一致：门控集合里的每个模型都在校准资料里，且默认白名单能被这个集合接受。
//
// 两者分处 service 与 config 两个包，改一处忘另一处不会编译失败，只会表现成启动报错（键不在
// 门控集合里）或 sol 侧间歇拒服（归因给 astra 却不被接受）。
func TestKongDefaultsAreConsistent(t *testing.T) {
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	for _, m := range KongDefaultGatedModels {
		if !bank.HasModel(m) {
			t.Errorf("默认门控模型 %q 不在校准资料里，准入判定不成立", m)
		}
	}
	accept, warns, err := KongParseTicketAccept(KongDefaultGatedModels, config.DefaultKongTicketAcceptExtra, bank)
	if err != nil {
		t.Fatalf("默认白名单与默认门控集合不自洽: %v", err)
	}
	// **必须断言 warnings 为空**：死配置现在只告警不报错，丢掉这个返回值的话，往默认串里加一个
	// 拼错的模型名测试照样通过，这条"默认全自洽"的检查就形同虚设。
	if len(warns) != 0 {
		t.Errorf("默认配置不该产生任何告警，得到 %v", warns)
	}
	// 5.6-sol 接受自己、astra、gpt-5.5（指纹法分不开 5.6-sol 与 5.5）与 gpt-6-sol（升级投放）；
	// gpt-6-sol 接受自己与 astra；astra 只接受自己。
	if got := accept.Of("gpt-5.6-sol"); strings.Join(got, ",") != "gpt-5.5,gpt-5.6-sol,gpt-6-astra,gpt-6-sol" {
		t.Errorf("5.6-sol 应接受自己、astra、gpt-5.5 与 gpt-6-sol，得到 %v", got)
	}
	if got := accept.Of("gpt-6-sol"); strings.Join(got, ",") != "gpt-6-astra,gpt-6-sol" {
		t.Errorf("gpt-6-sol 应只接受自己与 astra，得到 %v", got)
	}
	if got := accept.Of("gpt-6-astra"); len(got) != 1 || got[0] != "gpt-6-astra" {
		t.Errorf("astra 应只接受自己，得到 %v", got)
	}
	// 归因为 astra 的票：三个门控模型都放行；归因为 luna 的票都拒。
	astraProbs := map[string]float64{"gpt-6-astra": 0.97, "gpt-5.6-sol": 0.03}
	for _, m := range KongDefaultGatedModels {
		if !accept.Accepts(m, astraProbs, 0.9) {
			t.Errorf("归因为 astra 的票必须能支持 %q 请求——拿到更高档位不是降智", m)
		}
	}
	// 5.6-sol 的真实回答在含 gpt-6-sol 候选的资料下会分一部分给它：5.6-sol 要放行，
	// gpt-6-sol 不能放行——那张票更像 5.6-sol，对 gpt-6-sol 请求就是跨代下降。
	solSplit := map[string]float64{"gpt-5.6-sol": 0.6, "gpt-6-sol": 0.35, "gpt-6-astra": 0.05}
	if !accept.Accepts("gpt-5.6-sol", solSplit, 0.9) {
		t.Errorf("5.6-sol 应接受分给 gpt-6-sol 的那部分，得到质量 %v", accept.Mass("gpt-5.6-sol", solSplit))
	}
	if accept.Accepts("gpt-6-sol", solSplit, 0.9) {
		t.Error("gpt-6-sol 不得接受以 5.6-sol 为主的归因")
	}
	// 实测分布（2026-09-20，为 sol 取的 292 票）：只看 argmax 是 0.8214 达不到 0.9，
	// 计入 5.5 之后 0.9993 通过。这一条锁住"加 5.5"的实际效果。
	measured := map[string]float64{
		"gpt-5.6-sol": 0.8213532036796947,
		"gpt-5.5":     0.17788932213595104,
		"gpt-6-astra": 0.000016642761560881856,
	}
	if !accept.Accepts("gpt-5.6-sol", measured, 0.9) {
		t.Errorf("实测分布应当通过，得到质量 %v", accept.Mass("gpt-5.6-sol", measured))
	}
	// 同一份分布对 astra 与 gpt-6-sol 仍须拒绝：它们都不接受 5.6-sol / 5.5。
	for _, m := range []string{"gpt-6-astra", "gpt-6-sol"} {
		if accept.Accepts(m, measured, 0.9) {
			t.Errorf("%q 不得接受 5.6-sol / 5.5 的归因", m)
		}
	}

	for _, lunaProbs := range []map[string]float64{
		{"gpt-5.6-luna": 0.99, "gpt-6-astra": 0.01},
		{"gpt-6-luna": 0.95, "gpt-6-sol": 0.05},
	} {
		for _, m := range KongDefaultGatedModels {
			if accept.Accepts(m, lunaProbs, 0.9) {
				t.Errorf("归因为 luna 的票不得支持 %q：%v", m, lunaProbs)
			}
		}
	}
}

// top_models 是事后复核归因判断的唯一依据（票会随过期清理，事件长期保留），所以它必须按概率
// 降序、并列时定序、不足 3 个也不报错。
func TestKongTopModels(t *testing.T) {
	got := kongTopModels(map[string]float64{
		"gpt-5.6-sol":  0.5,
		"gpt-5.5":      0.3,
		"gpt-6-astra":  0.15,
		"gpt-5.6-luna": 0.05,
	}, 3)
	want := []KongTopModel{
		{Model: "gpt-5.6-sol", P: 0.5},
		{Model: "gpt-5.5", P: 0.3},
		{Model: "gpt-6-astra", P: 0.15},
	}
	if len(got) != len(want) {
		t.Fatalf("取前三应得 3 项，实得 %d：%v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项应为 %v，实为 %v", i, want[i], got[i])
		}
	}

	// 并列按模型名定序：同一份分布每次都要产出同样的顺序，否则两条相同的记录看起来不同。
	tie := kongTopModels(map[string]float64{"b": 0.5, "a": 0.5}, 2)
	if len(tie) != 2 || tie[0].Model != "a" || tie[1].Model != "b" {
		t.Fatalf("并列应按模型名升序，实得 %v", tie)
	}

	if got := kongTopModels(map[string]float64{"a": 1}, 3); len(got) != 1 {
		t.Fatalf("少于 n 项时应原样返回，实得 %v", got)
	}
	if got := kongTopModels(nil, 3); got != nil {
		t.Fatalf("空分布应返回 nil，实得 %v", got)
	}
}
