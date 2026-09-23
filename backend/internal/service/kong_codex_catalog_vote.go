package service

import (
	"sort"
	"strings"
	"time"
)

// Codex 目录存档的计票规则（DESIGN-codex-catalog-consensus.md 2.2、2.4）。这里只放不碰状态的判定，
// 方便逐条测试；状态与任务在 kong_codex_catalog.go。

// kongCodexCatalogPlanTier 把 plan_type 分成三档，数字越小越高。不认识的套餐名放第 2 档：上游新增的
// 多是付费或工作区套餐；空值、unknown、abnormal 是套餐不明，与 go、free 同在最低档。
func kongCodexCatalogPlanTier(plan string) int {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "prolite", "pro", "promax", "self_serve_business_prolite":
		return 1
	case "go", "free", "free_workspace", "unknown", "abnormal", "":
		return 3
	default:
		return 2
	}
}

// kongCodexCatalogEligible 判断账号的目录能否与别的账号共用一个版本：OpenAI OAuth、不是影子账号
// （影子用母账号的凭据），并且给下游前不需要按账号投影（没有模型映射或开了透传）。
func kongCodexCatalogEligible(account *Account) bool {
	if account == nil || !account.IsOpenAIOAuth() || account.IsShadow() {
		return false
	}
	return account.IsOpenAIPassthroughEnabled() || len(account.GetModelMapping()) == 0
}

// kongCodexCatalogVoters 从合格账号里取最高一档，返回这一档的账号与档位；没有合格账号时档位为 0。
func kongCodexCatalogVoters(accounts []Account) ([]Account, int) {
	tier := 0
	for i := range accounts {
		if t := kongCodexCatalogPlanTier(accounts[i].GetCredential("plan_type")); tier == 0 || t < tier {
			tier = t
		}
	}
	voters := make([]Account, 0, len(accounts))
	for i := range accounts {
		if kongCodexCatalogPlanTier(accounts[i].GetCredential("plan_type")) == tier {
			voters = append(voters, accounts[i])
		}
	}
	return voters, tier
}

// kongCodexCatalogVote 是一个投票人在该客户端版本下最新一段观测投的版本。
type kongCodexCatalogVote struct {
	accountID int64
	sha       string
	lastSeen  time.Time
}

type kongCodexCatalogBasis string

const (
	kongCodexCatalogKeep    kongCodexCatalogBasis = "保持"
	kongCodexCatalogFirst   kongCodexCatalogBasis = "首次选出"
	kongCodexCatalogReplace kongCodexCatalogBasis = "替代"
)

// kongCodexCatalogTally 是一个版本得到的票。
type kongCodexCatalogTally struct {
	sha      string
	votes    int
	fresh    int
	models   int
	lastSeen time.Time
}

// kongCodexCatalogDecide 按 2.4 选对外版本。current 为空时按全部选票首次选出；否则只有满足替代条件
// 的版本才能取代 current，current 失去支持本身不触发替代。models 返回某版本的模型名集合。
func kongCodexCatalogDecide(
	current string,
	votes []kongCodexCatalogVote,
	voterCount int,
	now time.Time,
	models func(sha string) map[string]struct{},
) (string, kongCodexCatalogBasis) {
	tallies := kongCodexCatalogTallies(votes, now, models)
	if current == "" {
		if len(tallies) == 0 {
			return "", kongCodexCatalogFirst
		}
		sort.SliceStable(tallies, func(i, j int) bool {
			a, b := tallies[i], tallies[j]
			if a.votes != b.votes {
				return a.votes > b.votes
			}
			if a.models != b.models {
				return a.models > b.models
			}
			return a.lastSeen.After(b.lastSeen)
		})
		return tallies[0].sha, kongCodexCatalogFirst
	}

	threshold := min(2, voterCount)
	currentFresh := 0
	for _, t := range tallies {
		if t.sha == current {
			currentFresh = t.fresh
		}
	}
	currentModels := models(current)
	type candidate struct {
		kongCodexCatalogTally
		growth bool
	}
	var candidates []candidate
	for _, t := range tallies {
		if t.sha == current || threshold == 0 || t.fresh < threshold {
			continue
		}
		growth := kongCodexCatalogStrictSuperset(models(t.sha), currentModels)
		if growth && t.fresh > currentFresh || !growth && currentFresh == 0 {
			candidates = append(candidates, candidate{t, growth})
		}
	}
	if len(candidates) == 0 {
		return current, kongCodexCatalogKeep
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.growth != b.growth {
			return a.growth
		}
		if a.fresh != b.fresh {
			return a.fresh > b.fresh
		}
		if a.models != b.models {
			return a.models > b.models
		}
		return a.lastSeen.After(b.lastSeen)
	})
	return candidates[0].sha, kongCodexCatalogReplace
}

func kongCodexCatalogTallies(votes []kongCodexCatalogVote, now time.Time, models func(string) map[string]struct{}) []kongCodexCatalogTally {
	index := make(map[string]int, len(votes))
	var tallies []kongCodexCatalogTally
	for _, v := range votes {
		i, ok := index[v.sha]
		if !ok {
			i = len(tallies)
			index[v.sha] = i
			tallies = append(tallies, kongCodexCatalogTally{sha: v.sha, models: len(models(v.sha))})
		}
		t := &tallies[i]
		t.votes++
		if now.Sub(v.lastSeen) <= kongCodexCatalogFreshWindow {
			t.fresh++
		}
		if v.lastSeen.After(t.lastSeen) {
			t.lastSeen = v.lastSeen
		}
	}
	return tallies
}

// kongCodexCatalogStrictSuperset 判断 a 是否包含 b 的全部模型名并且多出至少一个。
func kongCodexCatalogStrictSuperset(a, b map[string]struct{}) bool {
	if len(a) <= len(b) {
		return false
	}
	for slug := range b {
		if _, ok := a[slug]; !ok {
			return false
		}
	}
	return true
}

// kongCodexCatalogModelDiff 返回 to 相对 from 少了与多了的模型名，按名字排序，供日志使用。
func kongCodexCatalogModelDiff(from, to map[string]struct{}) (missing, added []string) {
	for slug := range from {
		if _, ok := to[slug]; !ok {
			missing = append(missing, slug)
		}
	}
	for slug := range to {
		if _, ok := from[slug]; !ok {
			added = append(added, slug)
		}
	}
	sort.Strings(missing)
	sort.Strings(added)
	return missing, added
}
