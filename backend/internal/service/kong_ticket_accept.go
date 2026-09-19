package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// kongTicketAcceptKey 是白名单的配置键路径，只用于报错时指明该改哪里。
//
// 配置本身在 config.Gateway.KongCodexTicket.AcceptExtra（定义与语义见
// internal/config/kong_ticket_config.go），viper 的 env 绑定让它同时可以用
// GATEWAY_KONG_CODEX_TICKET_ACCEPT_EXTRA 设定。
const kongTicketAcceptKey = "gateway.kong_codex_ticket.accept_extra"

// KongTicketAccept 记录每个门控模型接受哪些归因结果。
//
// **每个模型永远接受自己的归因**，这一条由构造函数补齐，不依赖配置写对：漏写自己的后果是该
// 模型永久拒服，而拒服不报错、只表现成「一直拿不到合格票」。
type KongTicketAccept map[string][]string

// KongParseTicketAccept 按门控集合与白名单配置构造接受关系。
//
// 两处校验都让启动失败，而不是忽略：
//   - 键不是门控模型——那条配置永远不会被用到，多半是拼错了模型名；
//   - 值不在校准资料里——闭集归因永远不会产出该结果，那条接受关系恒为空。
//
// 两种错误都不会报错，只会表现成「配了却没生效」，所以必须在启动时挡掉。
func KongParseTicketAccept(gated []string, raw string, bank *KongFingerprintBank) (KongTicketAccept, error) {
	accept := make(KongTicketAccept, len(gated))
	gatedSet := make(map[string]bool, len(gated))
	for _, m := range gated {
		gatedSet[m] = true
		accept[m] = []string{m}
	}

	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		model, list, ok := strings.Cut(entry, ":")
		model = strings.TrimSpace(model)
		if !ok || model == "" {
			return nil, fmt.Errorf("%s 的条目 %q 不是 `model:accepted[,accepted…]` 形式",
				kongTicketAcceptKey, entry)
		}
		if !gatedSet[model] {
			return nil, fmt.Errorf("%s 给了 %q 的接受关系，但它不在 %s 里，这条配置不会生效",
				kongTicketAcceptKey, model, KongTicketGatedModelsEnv)
		}
		for _, accepted := range kongSplitModels(list) {
			if bank != nil && !bank.HasModel(accepted) {
				return nil, fmt.Errorf("%s 让 %q 接受 %q，但后者不在校准资料里，归因永远不会判为它",
					kongTicketAcceptKey, model, accepted)
			}
			accept[model] = kongAppendUnique(accept[model], accepted)
		}
	}
	return accept, nil
}

// Of 返回某个模型接受的归因结果，顺序稳定（便于写进事件与日志后逐次对比）。
//
// 模型不在表里时退回「只接受自己」：那是最保守的一档，不会因为表缺了一项而放行不该放的票。
func (a KongTicketAccept) Of(model string) []string {
	if list, ok := a[model]; ok && len(list) > 0 {
		out := append([]string(nil), list...)
		sort.Strings(out)
		return out
	}
	return []string{model}
}

// Mass 求白名单内各归因结果的概率之和。
//
// 用和而不是只取最像的那一个：门要判的是「有没有掉到白名单之外」，那是一个并事件。sol 接受
// astra 时，两者各占 0.6 / 0.35 的分布已有 0.95 的证据支持「sol 或更好」，而 argmax 只有 0.6
// ——按 argmax 判会把一张合格的票拒掉，那是无谓拒服。
func (a KongTicketAccept) Mass(model string, probs map[string]float64) float64 {
	var mass float64
	for _, accepted := range a.Of(model) {
		mass += probs[accepted]
	}
	return mass
}

// Accepts 判定一张已归因的票能否支持该模型的请求。
//
// 这是**唯一**一处采纳判据：验证时落结论、读当前票时筛选、管理面显示都走它。分成两处写过的
// 代价已经见过——判据放在 SQL 里而采纳判定在 Go 里时，两边对同一张票会给出不同结论，而不一致
// 的那一侧不会报错。
func (a KongTicketAccept) Accepts(model string, probs map[string]float64, confidence float64) bool {
	if len(probs) == 0 {
		return false
	}
	// 阈值非正时一律拒绝，而不是"任何分布都够"。启用时 confidence 被校验在 (0,1) 内，取到 0 只
	// 可能是未启用那条装配路径的零值（Admin 拿 accept=nil / confidence=0）。目前没有调用点会走
	// 到这里，但缺了这一条，将来任何一处新调用都会变成静默放行。
	if confidence <= 0 {
		return false
	}
	return a.Mass(model, probs) >= confidence
}

func kongAppendUnique(list []string, item string) []string {
	for _, existing := range list {
		if existing == item {
			return list
		}
	}
	return append(list, item)
}

// kongProbsOf 把归因结果摊成 模型 → 概率。
//
// 闭集 softmax 的产物，各项之和为 1。采纳判据要的是白名单那几项之和，所以必须拿到整张分布，
// 而不是只拿 Prediction / Probability。
func kongProbsOf(result *KongFingerprintResult) map[string]float64 {
	if result == nil {
		return nil
	}
	probs := make(map[string]float64, len(result.Candidates))
	for _, c := range result.Candidates {
		probs[c.Model] = c.Probability
	}
	return probs
}

// kongPickCurrent 挑出一张能支持该模型请求的票：verified、未过期，且归因按**当前**白名单与
// 置信度仍然合格。
//
// 编排服务与管理面共用这一份：页面显示的「当前票」必须和业务实际会注入的那张是同一张，各写
// 一遍就会出现「页面说有票、请求却被拒」这种没法排查的分歧。
//
// 仓储按 expires_at 降序返回，所以拿到的是最晚过期的那张合格票——一张较新但按当前白名单不合格
// 的票不会挡住它。
func kongPickCurrent(ctx context.Context, repo KongTicketRepository, accept KongTicketAccept,
	confidence float64, accountID int64, model string) (*KongTicket, error) {
	tickets, err := repo.VerifiedTickets(ctx, accountID, model)
	if err != nil {
		return nil, err
	}
	for _, t := range tickets {
		if accept.Accepts(model, t.FingerprintProbs, confidence) {
			return t, nil
		}
	}
	return nil, nil
}
