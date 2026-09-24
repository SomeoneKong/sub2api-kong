package service

import (
	"encoding/json"
	"os"
	"testing"
)

// 挑战 prompt 必须与建库时用的逐字一致：校准库的 environment_centroids 是按 prompt 条件训练
// 的，换了措辞等于换了环境，特征整体偏移而归因不会报错——只会给出一个看起来正常的错结论。
// testdata 里那份是从建库脚本直接导出的。
func TestKongFingerprintChallengesMatchGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/kong_challenges_golden.json")
	if err != nil {
		t.Fatalf("读取挑战基准: %v", err)
	}
	var want []struct {
		ID            string `json:"id"`
		ExpectedCount int    `json:"expected_count"`
		Prompt        string `json:"prompt"`
		System        string `json:"system"`
		UserPrefix    string `json:"user_prefix"`
		Transport     string `json:"transport"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("解析挑战基准: %v", err)
	}

	got := KongFingerprintChallenges()
	if len(got) != len(want) {
		t.Fatalf("挑战条数 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Errorf("第 %d 条 id = %q, want %q", i+1, got[i].ID, want[i].ID)
		}
		if got[i].ExpectedCount != want[i].ExpectedCount {
			t.Errorf("%s 期望个数 = %d, want %d", want[i].ID, got[i].ExpectedCount, want[i].ExpectedCount)
		}
		if got[i].Prompt != want[i].Prompt {
			t.Errorf("%s 的 prompt 与建库时不一致\n得到: %q\n期望: %q", want[i].ID, got[i].Prompt, want[i].Prompt)
		}
		// environment-01 是 clean transport：没有 system 前缀也没有 user 前缀。
		// 若基准里出现前缀，说明取错了环境组。
		if want[i].Transport != "clean" || want[i].System != "" || want[i].UserPrefix != "" {
			t.Errorf("%s 的基准不是 clean transport（transport=%q），说明挑战集取错了环境组",
				want[i].ID, want[i].Transport)
		}
	}

	// 三条必须互不相同——它们是三个不同的 prompt，不是同一 prompt 重复采样。
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c.Prompt] {
			t.Error("出现重复的 prompt：多份回答会失去独立性")
		}
		seen[c.Prompt] = true
	}

	if c, ok := KongFingerprintChallengeByID("query-02"); !ok || c.ExpectedCount != 233 {
		t.Error("按 id 取回挑战失败——离线重算历史记录时需要还原当时的输入条件")
	}
	if _, ok := KongFingerprintChallengeByID("query-99"); ok {
		t.Error("不存在的 id 不该返回挑战")
	}
}
