//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKongCodexCatalogPlanTier(t *testing.T) {
	cases := map[string]int{
		"prolite": 1, "pro": 1, "promax": 1, "self_serve_business_prolite": 1, " PRO ": 1,
		"plus": 2, "team": 2, "business": 2, "self_serve_business_usage_based": 2, "enterprise": 2,
		"enterprise_cbp_automation": 2, "ent26": 2, "edu": 2, "edu_pro": 2, "education": 2, "brand_new_plan": 2,
		"go": 3, "free": 3, "free_workspace": 3, "unknown": 3, "abnormal": 3, "": 3, "  ": 3,
	}
	for plan, want := range cases {
		require.Equal(t, want, kongCodexCatalogPlanTier(plan), "plan %q", plan)
	}
}

func kongCatalogTestAccount(id int64, plan string) Account {
	return Account{
		ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"plan_type": plan},
	}
}

func TestKongCodexCatalogEligible(t *testing.T) {
	oauth := kongCatalogTestAccount(1, "pro")
	require.True(t, kongCodexCatalogEligible(&oauth))

	mapped := kongCatalogTestAccount(2, "pro")
	mapped.Credentials["model_mapping"] = map[string]any{"alias": "gpt-6.1-sol"}
	require.False(t, kongCodexCatalogEligible(&mapped), "有映射且没开透传的要按账号投影")
	mapped.Extra = map[string]any{"openai_passthrough": true}
	require.True(t, kongCodexCatalogEligible(&mapped), "开了透传就不投影")

	parent := int64(1)
	shadow := kongCatalogTestAccount(3, "pro")
	shadow.ParentAccountID = &parent
	require.False(t, kongCodexCatalogEligible(&shadow))

	apiKey := kongCatalogTestAccount(4, "")
	apiKey.Type = AccountTypeAPIKey
	require.False(t, kongCodexCatalogEligible(&apiKey))
	require.False(t, kongCodexCatalogEligible(nil))
}

func TestKongCodexCatalogVotersUseTopTier(t *testing.T) {
	accounts := []Account{
		kongCatalogTestAccount(1, "free"), kongCatalogTestAccount(2, "plus"), kongCatalogTestAccount(3, "go"),
		kongCatalogTestAccount(4, "plus"), kongCatalogTestAccount(5, "free"),
	}
	voters, tier := kongCodexCatalogVoters(accounts)
	require.Equal(t, 2, tier)
	require.Equal(t, []int64{2, 4}, kongCatalogTestIDs(voters))

	accounts = append(accounts, kongCatalogTestAccount(6, "prolite"))
	voters, tier = kongCodexCatalogVoters(accounts)
	require.Equal(t, 1, tier)
	require.Equal(t, []int64{6}, kongCatalogTestIDs(voters))

	voters, tier = kongCodexCatalogVoters(nil)
	require.Zero(t, tier)
	require.Empty(t, voters)
}

func kongCatalogTestIDs(accounts []Account) []int64 {
	ids := make([]int64, 0, len(accounts))
	for i := range accounts {
		ids = append(ids, accounts[i].ID)
	}
	return ids
}

// 测试用的版本：名字即 sha，模型名集合固定。
var kongCatalogTestModels = map[string]map[string]struct{}{
	"C":  kongCatalogSet("sol", "astra", "luna", "reserve", "terra", "luna56", "sol56", "gpt55", "sol6"), // 9 个
	"C2": kongCatalogSet("sol", "astra", "luna", "reserve", "terra", "luna56", "sol56", "gpt55", "sol6"), // 同名不同内容
	"V":  kongCatalogSet("sol", "astra", "luna", "reserve", "terra", "luna56", "sol56", "gpt55"),         // 少一个
	"D":  kongCatalogSet("luna", "reserve", "terra", "luna56", "gpt55"),                                  // free 的缩水版
	"G":  kongCatalogSet("sol", "astra", "luna", "reserve", "terra", "luna56", "sol56", "gpt55", "sol6", "new"),
}

func kongCatalogSet(slugs ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(slugs))
	for _, slug := range slugs {
		set[slug] = struct{}{}
	}
	return set
}

func kongCatalogTestModelsOf(sha string) map[string]struct{} { return kongCatalogTestModels[sha] }

func kongCatalogVote(id int64, sha string, ago time.Duration, now time.Time) kongCodexCatalogVote {
	return kongCodexCatalogVote{accountID: id, sha: sha, lastSeen: now.Add(-ago)}
}

func TestKongCodexCatalogDecideFirstElection(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	decide := func(votes ...kongCodexCatalogVote) string {
		chosen, basis := kongCodexCatalogDecide("", votes, 4, now, kongCatalogTestModelsOf)
		require.Equal(t, kongCodexCatalogFirst, basis)
		return chosen
	}
	require.Equal(t, "", decide())
	// 票数优先，不限新鲜程度
	require.Equal(t, "V", decide(kongCatalogVote(1, "V", 3*time.Hour, now), kongCatalogVote(2, "V", 5*time.Hour, now), kongCatalogVote(3, "C", time.Minute, now)))
	// 平票取模型多的
	require.Equal(t, "C", decide(kongCatalogVote(1, "V", time.Minute, now), kongCatalogVote(2, "C", 2*time.Hour, now)))
	// 再平取最近看到的
	require.Equal(t, "C2", decide(kongCatalogVote(1, "C", 2*time.Hour, now), kongCatalogVote(2, "C2", time.Minute, now)))
}

func TestKongCodexCatalogDecideReplacement(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		voters int
		votes  []kongCodexCatalogVote
		want   string
	}{
		{
			name:   "一个账号掉档，其他账号闲着",
			voters: 4,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "D", time.Minute, now),
				kongCatalogVote(2, "C", 3*time.Hour, now), kongCatalogVote(3, "C", 5*time.Hour, now),
			},
			want: "C",
		},
		{
			name:   "C 两票、少一个模型的旧版 V 两票都新鲜，一个看 C 的账号掉档",
			voters: 4,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "D", time.Minute, now), kongCatalogVote(2, "C", 10*time.Minute, now),
				kongCatalogVote(3, "V", 30*time.Minute, now), kongCatalogVote(4, "V", 30*time.Minute, now),
			},
			want: "C",
		},
		{
			name:   "模型名相同而内容不同的旧版两票新鲜，一个看 C 的账号掉档",
			voters: 4,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "D", time.Minute, now), kongCatalogVote(2, "C", 10*time.Minute, now),
				kongCatalogVote(3, "C2", 30*time.Minute, now), kongCatalogVote(4, "C2", 30*time.Minute, now),
			},
			want: "C",
		},
		{
			name:   "冷启动只有一份 C，之后这个账号掉档，其他账号没有观测",
			voters: 3,
			votes:  []kongCodexCatalogVote{kongCatalogVote(1, "D", time.Minute, now)},
			want:   "C",
		},
		{
			name:   "两个账号都看到少一个模型的 X，C 已没有新鲜票",
			voters: 2,
			votes:  []kongCodexCatalogVote{kongCatalogVote(1, "V", time.Minute, now), kongCatalogVote(2, "V", 2*time.Minute, now)},
			want:   "V",
		},
		{
			name:   "少模型的版本两票新鲜，但 C 还有一张新鲜票",
			voters: 3,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "V", time.Minute, now), kongCatalogVote(2, "V", 2*time.Minute, now),
				kongCatalogVote(3, "C", 50*time.Minute, now),
			},
			want: "C",
		},
		{
			name:   "C 的最后一张新鲜票过期后切到少模型的版本",
			voters: 3,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "V", time.Minute, now), kongCatalogVote(2, "V", 2*time.Minute, now),
				kongCatalogVote(3, "C", 61*time.Minute, now),
			},
			want: "V",
		},
		{
			name:   "上游新增模型，新鲜票过 2 且多于 C",
			voters: 5,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "G", time.Minute, now), kongCatalogVote(2, "G", 2*time.Minute, now),
				kongCatalogVote(3, "G", 3*time.Minute, now), kongCatalogVote(4, "C", 8*time.Minute, now),
				kongCatalogVote(5, "C", 9*time.Minute, now),
			},
			want: "G",
		},
		{
			name:   "新增模型的版本新鲜票与 C 持平时不切",
			voters: 4,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "G", time.Minute, now), kongCatalogVote(2, "G", 2*time.Minute, now),
				kongCatalogVote(3, "C", 8*time.Minute, now), kongCatalogVote(4, "C", 9*time.Minute, now),
			},
			want: "C",
		},
		{
			name:   "新增模型的版本只有一票",
			voters: 3,
			votes:  []kongCodexCatalogVote{kongCatalogVote(1, "G", time.Minute, now)},
			want:   "C",
		},
		{
			name:   "灰度：一部分账号长期看到另一份内容",
			voters: 5,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "C2", time.Minute, now), kongCatalogVote(2, "C2", 2*time.Minute, now),
				kongCatalogVote(3, "C2", 3*time.Minute, now), kongCatalogVote(4, "C", 5*time.Minute, now),
			},
			want: "C",
		},
		{
			name:   "只有一个合格账号时跟着它走",
			voters: 1,
			votes:  []kongCodexCatalogVote{kongCatalogVote(1, "D", time.Minute, now)},
			want:   "D",
		},
		{
			name:   "两个账号同时掉档、其他账号都闲着（超出单账号隔离的范围）",
			voters: 4,
			votes: []kongCodexCatalogVote{
				kongCatalogVote(1, "D", time.Minute, now), kongCatalogVote(2, "D", 2*time.Minute, now),
				kongCatalogVote(3, "C", 3*time.Hour, now),
			},
			want: "D",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chosen, basis := kongCodexCatalogDecide("C", tc.votes, tc.voters, now, kongCatalogTestModelsOf)
			require.Equal(t, tc.want, chosen)
			if tc.want == "C" {
				require.Equal(t, kongCodexCatalogKeep, basis)
			} else {
				require.Equal(t, kongCodexCatalogReplace, basis)
			}
		})
	}
}

func TestKongCodexCatalogDecidePrefersGrowthCandidate(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	votes := []kongCodexCatalogVote{
		kongCatalogVote(1, "G", time.Minute, now), kongCatalogVote(2, "G", time.Minute, now),
		kongCatalogVote(3, "V", time.Minute, now), kongCatalogVote(4, "V", time.Minute, now), kongCatalogVote(5, "V", time.Minute, now),
	}
	chosen, _ := kongCodexCatalogDecide("C", votes, 5, now, kongCatalogTestModelsOf)
	require.Equal(t, "G", chosen, "C 没有新鲜票时两者都满足，先取只多不少的")
}

func TestKongCodexCatalogModelDiffAndSuperset(t *testing.T) {
	missing, added := kongCodexCatalogModelDiff(kongCatalogTestModels["C"], kongCatalogTestModels["D"])
	require.Equal(t, []string{"astra", "sol", "sol56", "sol6"}, missing)
	require.Empty(t, added)
	require.True(t, kongCodexCatalogStrictSuperset(kongCatalogTestModels["G"], kongCatalogTestModels["C"]))
	require.False(t, kongCodexCatalogStrictSuperset(kongCatalogTestModels["C2"], kongCatalogTestModels["C"]))
	require.False(t, kongCodexCatalogStrictSuperset(kongCatalogTestModels["C"], kongCatalogTestModels["G"]))
}
