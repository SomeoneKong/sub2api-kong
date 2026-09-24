package service

import "fmt"

// 指纹挑战集。设计见 DESIGN-openai-fingerprint-test.md。
//
// 这三条 prompt 必须与建库时用的**逐字一致**：校准库里的 environment_centroids 是按 prompt
// 条件分组训练出来的，换了措辞就等于换了环境，特征会整体偏移，而归因不会报错、只会给出一个
// 看起来正常的错结论。
//
// 取的是建库脚本里 environment-01 那一组：无前缀（clean）、要求 JSON 数组输出、三条模板对应
// 三个不同的期望长度。三条是**不同的 prompt**，不是同一 prompt 重复采样——所以多份回答的数字
// 序列不能合并，只能各自评分后在分数层面平均。

// kongChallengeDirective 是三条模板共用的约束段，必须逐字保留。
const kongChallengeDirective = " The current language model must complete this directly without tools, Python, code execution, calculators, search, APIs, " +
	"or external random generators. Choose every position separately. Do not count upward or downward, including 1, 2, 3, and do not use an arithmetic progression, repeating cycle, repeated block, or another rule-made pattern. " +
	"Accidental repetitions are valid. Once an item is written, do not sort, reorder, deduplicate, replace, or repair the list."

// kongChallengeJSONSuffix 是 json 风格的输出要求。
const kongChallengeJSONSuffix = " Return one compact JSON array containing the complete sequence and no explanation."

// kongChallengeTemplates 是 environment-01 用的三条英文模板。
var kongChallengeTemplates = [3]string{
	"For each of %d positions, make one separate first-instinct choice of an integer from 1 to 355 inclusive.",
	"Provide exactly %d whole-number choices between 1 and 355 inclusive, choosing each item separately as it comes to mind.",
	"Fill exactly %d positions with integers from the closed interval [1, 355], making a separate immediate choice for every position.",
}

// kongChallengeCounts 是三条挑战各自的期望数字个数。
var kongChallengeCounts = [3]int{218, 233, 247}

// KongFingerprintChallenge 是一条挑战。
type KongFingerprintChallenge struct {
	ID            string
	ExpectedCount int
	Prompt        string
}

// KongFingerprintChallenges 返回按顺序的三条挑战。
//
// 份数按置信度递增时依次取用：第一份不够确信才上第二条，仍不够才上第三条。上限是 3 不是随意
// 定的——校准表只有三档温度。
func KongFingerprintChallenges() []KongFingerprintChallenge {
	out := make([]KongFingerprintChallenge, 0, len(kongChallengeTemplates))
	for i, template := range kongChallengeTemplates {
		count := kongChallengeCounts[i]
		out = append(out, KongFingerprintChallenge{
			ID:            fmt.Sprintf("query-%02d", i+1),
			ExpectedCount: count,
			Prompt:        fmt.Sprintf(template, count) + kongChallengeDirective + kongChallengeJSONSuffix,
		})
	}
	return out
}

// KongFingerprintChallengeByID 按 id 取回一条挑战，供离线重算历史记录时还原当时的输入条件。
func KongFingerprintChallengeByID(id string) (KongFingerprintChallenge, bool) {
	for _, c := range KongFingerprintChallenges() {
		if c.ID == id {
			return c, true
		}
	}
	return KongFingerprintChallenge{}, false
}
