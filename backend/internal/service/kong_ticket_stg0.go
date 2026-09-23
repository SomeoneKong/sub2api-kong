package service

import (
	"fmt"
	"sort"
	"strings"
)

// stg0 是降智识别的第一道门：看**上游响应里回报的 model** 是不是我们请求的那个。
//
// 它与 stg1（指纹归因）是串联的两层，各自看得见对方看不见的东西——stg0 抓上游明说"我给了别的
// 模型"，stg1 抓上游什么都不说但输出分布变了。设计见 DESIGN-codex-ticket.md §4.0。
//
// 成本为零：字段本来就在解析中（业务路径由上游的 observer 采集并落 usage_logs，验票路径在既有的
// SSE 解析里多取一个字段）。所以它排在 stg1 之前——判 fail 就不必再烧三份长答案的额度。

// kongStg0AcceptKey 是白名单的配置键路径，只用于报错时指明该改哪里。
const kongStg0AcceptKey = "gateway.kong_codex_ticket.stg0_accept"

// KongStg0Verdict 是 stg0 的判定结果。
type KongStg0Verdict int

const (
	// KongStg0Unknown 表示没观测到响应 model（字段缺失、非 SSE、流读失败）。
	//
	// 它**不阻止放行**：没测出来对票什么都没说，据它作废票是无谓拒服（与事件的 inconclusive
	// 同一口径，见 §5.4）。
	KongStg0Unknown KongStg0Verdict = iota
	// KongStg0Pass 表示回报的就是请求的模型（或其快照版本，或白名单接受的替代）。
	KongStg0Pass
	// KongStg0Fail 表示上游回报了**别的**模型——由上游自己声明的降智证据。
	KongStg0Fail
)

// KongStg0Accept 记录每个门控模型额外接受哪些**上游回报值**。
//
// **与 KongTicketAccept 是两张表，不可合并、也不该互抄值**：那一张补偿的是指纹归因的区分度不足
// （`gpt-5.6-sol` 接受 `gpt-5.5` 的归因，因为指纹分不开两者），而 stg0 读的是上游自己的声明、
// 没有测量噪声。把那条补偿抄过来，会让一次上游明说的降智被放过。
type KongStg0Accept map[string][]string

// KongParseStg0Accept 按门控集合与白名单配置构造 stg0 的接受关系。
//
// 格式与 accept_extra 一致（`model:accepted[,accepted…]`，条目以 `;` 分隔），运维读同一套语法。
//
// 与 KongParseTicketAccept 的**唯一实质差别**：不校验取值是否在指纹校准资料里。stg0 的值是上游
// 可能回报的任何 model 名——`gpt-6-sol` 这种升级投放的目标压根不在校准资料中，照那边校验会把一条
// 正确配置当笔误忽略掉。
//
// 键不是门控模型时忽略并告警（那条配置永不生效，多半是拼错模型名）；格式错仍然报错，否则运维会
// 以为配上了。
func KongParseStg0Accept(gated []string, raw string) (KongStg0Accept, []string, error) {
	var warnings []string
	accept := make(KongStg0Accept, len(gated))
	gatedSet := make(map[string]bool, len(gated))
	for _, m := range gated {
		gatedSet[m] = true
		accept[m] = nil
	}

	for _, entry := range strings.Split(raw, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		model, list, ok := strings.Cut(entry, ":")
		model = strings.TrimSpace(model)
		if !ok || model == "" {
			return nil, nil, fmt.Errorf("%s 的条目 %q 不是 `model:accepted[,accepted…]` 形式",
				kongStg0AcceptKey, entry)
		}
		if !gatedSet[model] {
			warnings = append(warnings, fmt.Sprintf(
				"%s 给了 %q 的接受关系，但它不在 %s 里，该条已忽略",
				kongStg0AcceptKey, model, KongTicketGatedModelsEnv))
			continue
		}
		for _, accepted := range kongSplitModels(list) {
			accept[model] = kongAppendUnique(accept[model], accepted)
		}
	}
	return accept, warnings, nil
}

// Of 返回某个模型额外接受的回报值，顺序稳定（便于写进事件后逐次对比）。
//
// 请求的模型自身不在这个列表里——它由 Verdict 直接比较，不依赖配置写对：漏写自己的后果是该模型
// 每张票都被 stg0 判死。
func (a KongStg0Accept) Of(model string) []string {
	list, ok := a[model]
	if !ok || len(list) == 0 {
		return nil
	}
	out := append([]string(nil), list...)
	sort.Strings(out)
	return out
}

// Verdict 判定一次上游回报。requested 是发给上游的模型名，reported 是响应里回报的。
func (a KongStg0Accept) Verdict(requested, reported string) KongStg0Verdict {
	reported = strings.TrimSpace(reported)
	if reported == "" {
		return KongStg0Unknown
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		// 不知道请求的是什么就判不了一致性。这是"没测出来"，不是"测出不合格"。
		return KongStg0Unknown
	}
	if kongModelSnapshotMatch(requested, reported) {
		return KongStg0Pass
	}
	for _, accepted := range a.Of(requested) {
		if kongModelSnapshotMatch(accepted, reported) {
			return KongStg0Pass
		}
	}
	return KongStg0Fail
}

// kongModelSnapshotMatch 判定 reported 是不是 base 这个模型（含它的快照版本）。
//
// 规则：大小写无关地相等，或者 reported 去掉 `base-` 前缀后**以数字开头**。
//
// 后缀容错是必需的：实测存在 `gpt-5.4-mini` → `gpt-5.4-mini-2026-03-17` 这种回报（上游的审计判据
// 是字面相等，把它记成了 mismatch）。按字面相等判死票，上游每换一次快照就会把该模型的票全部作废。
//
// 而"后缀必须以数字开头"把容错限制在快照与日期上。少了这一条，纯前缀匹配会让 `gpt-6` 吞掉
// `gpt-6-astra`、让 `gpt-5.6-sol` 吞掉 `gpt-5.6-sol-mini`——那都是**另一个模型**，放过去就等于
// 把这道门在最该拦住的地方打开。
func kongModelSnapshotMatch(base, reported string) bool {
	if strings.EqualFold(base, reported) {
		return true
	}
	prefix := base + "-"
	if len(reported) <= len(prefix) || !strings.EqualFold(reported[:len(prefix)], prefix) {
		return false
	}
	return reported[len(prefix)] >= '0' && reported[len(prefix)] <= '9'
}

// KongStg0Stats 是某个 (账号, 模型) 近若干天的 stg0 观测汇总，数据来自业务请求。
//
// 三个计数互斥、相加等于窗口内该模型的受控请求数。Unknown 必须单独显示：它与 Match 是两件事，
// 全是 Unknown 时"零次不一致"是假的安全感——那说明观测没工作，不是没被降智。
type KongStg0Stats struct {
	AccountID int64  `json:"account_id"`
	Model     string `json:"model"`
	// Total 是窗口内该 (账号, 模型) 的请求数，也是比例的分母。**与分子同一总体**。
	Total int64 `json:"total"`
	// Mismatch 是上游回报了别的模型的次数。
	Mismatch int64 `json:"mismatch"`
	// Unknown 是没观测到回报值的次数。
	Unknown int64 `json:"unknown"`
	// MismatchAccepted / MismatchUnaccepted 把 Mismatch 按 stg0 白名单拆成两份，相加恒等于它。
	//
	// 拆开的理由是这两份的**处置完全不同**：已接受的那份是上游在投放我们认可的替代模型（升级投放），
	// 验票不会判死票，运维无须动作；未接受的那份才是"上游自己声明给了别的模型且我们没放行"。混成一个
	// 数时，一次真正的降智会被一大批已接受的投放稀释到看不见，而配好白名单之后页面仍然长期标红——
	// 那等于让这块读数永久失去可操作性。
	MismatchAccepted   int64 `json:"mismatch_accepted"`
	MismatchUnaccepted int64 `json:"mismatch_unaccepted"`
	// TopReported 列出 Mismatch 里回报值出现最多的几项。**永远是非 nil 切片**——nil 会序列化成
	// JSON `null`，而前端按数组读它；零 mismatch 是最常见的情况，留 nil 等于让正常账号打崩页面。
	//
	// 没有它，运维看到"mismatch 87%"也不知道该往 stg0_accept 加什么——而上游投放新模型时，那恰好
	// 是唯一要做的动作。
	TopReported []KongStg0Reported `json:"top_reported"`
}

// KongStg0Reported 是一个被回报过的替代模型及其次数。
type KongStg0Reported struct {
	Model string `json:"model"`
	Count int64  `json:"count"`
	// Accepted 表示这个回报值在 stg0 白名单内（或只是同一模型的快照后缀），验票不会据它判死票。
	Accepted bool `json:"accepted"`
}

// kongStg0TopReportedMax 是页面上每个 (账号, 模型) 列出的回报值种数上限。
//
// 它只为挡住"上游大面积回报各种模型时把这一列刷满"。取 5：运维需要的是"该往 stg0_accept 加哪一条"，
// 前几名足以回答，而列全部只会把那个答案埋掉。截断发生在分类与排序**之后**——未接受的回报值正是
// 要人动手的那些，先截断再排序会把它们挤掉。
const kongStg0TopReportedMax = 5

// ClassifyStats 按白名单把一条统计的 Mismatch 拆成已接受 / 未接受，并给明细排序截断。
//
// **判据必须是 Verdict，不能是"在不在白名单的列表里"**：仓储那侧的 mismatch 用的是上游审计口径
// （字面相等），它把 `gpt-5.4-mini-2026-03-17` 这种快照后缀也算成不一致，而验票是否判死票由 Verdict
// 说了算（它连快照匹配一起判）。两处口径不同源时，页面标红的与实际判死的就不是同一批请求。
//
// 明细里没列到的那部分（回报值种类超过仓储上限时的剩余）一律计入**未接受**：那是未知，而把未知
// 当已接受会让一次真正的降智显示成中性色——放过降智比无谓地标一次红严重得多。
func (a KongStg0Accept) ClassifyStats(s *KongStg0Stats) {
	if s == nil {
		return
	}
	var accepted int64
	for i := range s.TopReported {
		r := &s.TopReported[i]
		r.Accepted = a.Verdict(s.Model, r.Model) == KongStg0Pass
		if r.Accepted {
			accepted += r.Count
		}
	}
	// 汇总与明细是两次查询，之间可能又落进新行，于是明细的和可以超过 Mismatch。夹一下，
	// 否则未接受数会变成负值、比例算出负百分比。
	if accepted > s.Mismatch {
		accepted = s.Mismatch
	}
	s.MismatchAccepted = accepted
	s.MismatchUnaccepted = s.Mismatch - accepted

	sort.SliceStable(s.TopReported, func(i, j int) bool {
		a, b := s.TopReported[i], s.TopReported[j]
		if a.Accepted != b.Accepted {
			// 未接受的排前面：它是唯一需要人动手的那类。
			return !a.Accepted
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Model < b.Model
	})
	if len(s.TopReported) > kongStg0TopReportedMax {
		s.TopReported = s.TopReported[:kongStg0TopReportedMax]
	}
}

// MismatchRate 返回不一致占比。Total 为 0 时返回 0，调用方需自行区分"无样本"与"零不一致"
// ——把无样本显示成 0% 会被读成"查过了、没问题"。
func (s *KongStg0Stats) MismatchRate() float64 {
	if s == nil || s.Total <= 0 {
		return 0
	}
	return float64(s.Mismatch) / float64(s.Total)
}
