//go:build unit

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// 文本指纹的 Go 实现与 fp-lab 的 Python 实现必须逐项一致：golden 资料由 fp-lab/export_golden.py 用同一份
// 库生成，含真实回答、边界构造的回答，以及覆盖各条判定规则的得分序列。

const kongFPGoldenTolerance = 1e-9

type kongFPGoldenAnswerRow struct {
	Name          string                        `json:"name"`
	Text          string                        `json:"text"`
	Sections      map[string]string             `json:"sections"`
	SectionScores map[string]map[string]float64 `json:"section_scores"`
	Part          map[string]float64            `json:"part"`
}

type kongFPGoldenDecisionRow struct {
	Name    string               `json:"name"`
	Parts   []map[string]float64 `json:"parts"`
	Rounds  int                  `json:"rounds"`
	Target  string               `json:"target"`
	Verdict string               `json:"verdict"`
	Reason  string               `json:"reason"`
	Decided string               `json:"decided"`
	Level1  map[string]float64   `json:"level1"`
	Pair    map[string]float64   `json:"pair"`
}

type kongFPGoldenFile struct {
	BankSHA256 string                    `json:"bank_sha256"`
	Answers    []kongFPGoldenAnswerRow   `json:"answers"`
	Decisions  []kongFPGoldenDecisionRow `json:"decisions"`
}

var (
	kongFPGoldenOnce sync.Once
	kongFPGoldenData kongFPGoldenFile
	kongFPGoldenErr  error
)

func kongFPLoadGolden(t *testing.T) *kongFPGoldenFile {
	t.Helper()
	kongFPGoldenOnce.Do(func() {
		raw, err := os.ReadFile("testdata/kong_fingerprint_text_golden.json")
		if err != nil {
			kongFPGoldenErr = err
			return
		}
		kongFPGoldenErr = json.Unmarshal(raw, &kongFPGoldenData)
	})
	if kongFPGoldenErr != nil {
		t.Fatalf("读取 golden: %v", kongFPGoldenErr)
	}
	return &kongFPGoldenData
}

// kongFPGoldenText 取 golden 里一份回答的原文。
func kongFPGoldenText(t *testing.T, name string) string {
	t.Helper()
	for _, a := range kongFPLoadGolden(t).Answers {
		if a.Name == name {
			return a.Text
		}
	}
	t.Fatalf("golden 里没有回答 %q", name)
	return ""
}

func kongFPTestBank(t *testing.T) *KongFingerprintBank {
	t.Helper()
	bank, err := kongParseFingerprintBank(kongFingerprintBankJSON, "embedded")
	if err != nil {
		t.Fatalf("内置指纹库加载失败: %v", err)
	}
	return bank
}

func kongFPCloseMaps(t *testing.T, what string, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：得到 %d 项，期望 %d 项（%v / %v）", what, len(got), len(want), got, want)
	}
	for k, w := range want {
		g, ok := got[k]
		// 写成取反：任何一边是 NaN 或无穷时比较为假，同样算不一致。
		if !ok || !(math.Abs(g-w) <= kongFPGoldenTolerance) {
			t.Fatalf("%s[%s] = %v，期望 %v", what, k, g, w)
		}
	}
}

func kongFPSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// 内置库必须就是生成 golden 的那一份：换库后要重新生成 golden。
func TestKongFingerprintTextGoldenBank(t *testing.T) {
	golden := kongFPLoadGolden(t)
	sum := sha256.Sum256(kongFingerprintBankJSON)
	if got := hex.EncodeToString(sum[:]); got != golden.BankSHA256 {
		t.Fatalf("内置库摘要 %s 与 golden 的 %s 不一致：换库后要用 fp-lab/export_golden.py 重新生成 golden", got, golden.BankSHA256)
	}
	bank := kongFPTestBank(t)
	if bank.Version()["digest"] != golden.BankSHA256 {
		t.Fatalf("库版本里的摘要 = %v", bank.Version()["digest"])
	}
	if bank.MaxParts() != 4 || bank.Challenge.Effort != "low" || bank.Challenge.ID == "" || len(bank.Models) != 8 {
		t.Fatalf("库参数 = 最多 %d 份、强度 %q、挑战 %q、%d 个模型", bank.MaxParts(), bank.Challenge.Effort, bank.Challenge.ID, len(bank.Models))
	}
}

func TestKongFingerprintTextGoldenAnswers(t *testing.T) {
	bank := kongFPTestBank(t)
	for _, a := range kongFPLoadGolden(t).Answers {
		t.Run(a.Name, func(t *testing.T) {
			split := kongTextSplit(a.Text, bank.sections)
			if !reflect.DeepEqual(kongFPSortedKeys(split), kongFPSortedKeys(a.Sections)) {
				t.Fatalf("拆出的题 = %v，期望 %v", kongFPSortedKeys(split), kongFPSortedKeys(a.Sections))
			}
			for name, body := range a.Sections {
				if split[name] != body {
					t.Fatalf("第 %s 题的正文不一致：\n得到 %q\n期望 %q", name, split[name], body)
				}
			}
			scored := bank.ScoreAnswer(a.Text)
			if scored.SectionCount != len(a.Sections) || len(scored.Sections) != len(a.SectionScores) {
				t.Fatalf("题数 = %d / %d，期望 %d", scored.SectionCount, len(scored.Sections), len(a.SectionScores))
			}
			for name, want := range a.SectionScores {
				kongFPCloseMaps(t, "第 "+name+" 题得分", scored.Sections[name], want)
			}
			kongFPCloseMaps(t, "整份得分", scored.Scores, a.Part)
		})
	}
}

func TestKongFingerprintTextGoldenDecisions(t *testing.T) {
	bank := kongFPTestBank(t)
	group := bank.GroupClass()
	toGo := func(class string) string {
		if class == "group" {
			return group
		}
		return class
	}
	for _, d := range kongFPLoadGolden(t).Decisions {
		t.Run(d.Name, func(t *testing.T) {
			got := bank.Decide(d.Parts, d.Rounds, d.Target)
			if got.Verdict != d.Verdict || got.Reason != d.Reason || got.Decided != toGo(d.Decided) {
				t.Fatalf("判定 = %q/%q/%q，期望 %q/%q/%q", got.Verdict, got.Reason, got.Decided, d.Verdict, d.Reason, toGo(d.Decided))
			}
			level1 := map[string]float64{}
			for _, c := range got.Level1 {
				level1[c.Class] = c.Probability
			}
			want1 := map[string]float64{}
			for k, v := range d.Level1 {
				want1[toGo(k)] = v
			}
			kongFPCloseMaps(t, "第一层", level1, want1)
			pair := map[string]float64{}
			for _, c := range got.Pair {
				pair[c.Class] = c.Probability
			}
			if d.Pair == nil {
				d.Pair = map[string]float64{}
			}
			kongFPCloseMaps(t, "第二层", pair, d.Pair)
		})
	}
}

// 小写化要与 Python 的 str.lower 一致：带点大写 I 在 Python 里变成 "i" 加组合点，切词时断开。
func TestKongTextFeaturesLowercase(t *testing.T) {
	features := kongTextFeatures("İstanbul It's  DON’T")
	want := []string{"i", "stanbul", "it's", "don", "t"}
	if !reflect.DeepEqual(features[0].keys, want) {
		t.Fatalf("词 = %v，期望 %v", features[0].keys, want)
	}
	if features[1].total != len(want)-1 || features[1].keys[0] != "i stanbul" {
		t.Fatalf("词对 = %v", features[1].keys)
	}
}

// 库文件的每一项缺失或取值越界都要拒绝加载：这些错误不会让评分报错，只会算出看起来正常的错误结论。
func TestKongFingerprintTextBankRejects(t *testing.T) {
	var base map[string]any
	if err := json.Unmarshal(kongFingerprintBankJSON, &base); err != nil {
		t.Fatal(err)
	}
	firstCount := func(b map[string]any) map[string]any {
		return b["counts"].(map[string]any)["pk-email"].(map[string]any)["word"].(map[string]any)["gpt-5.5"].(map[string]any)
	}
	anyKey := func(m map[string]any) string {
		for k := range m {
			return k
		}
		return ""
	}
	twoLevel := func(b map[string]any) map[string]any { return b["two_level"].(map[string]any) }
	familyCounts := func(b map[string]any, family string) map[string]any {
		return b["counts"].(map[string]any)["pk-email"].(map[string]any)[family].(map[string]any)
	}
	cases := []struct {
		name   string
		mutate func(b map[string]any)
		want   string
	}{
		{"旧的 ModelTrace 库", func(b map[string]any) { b["schema"] = "modeltrace-unified-bank" }, "schema"},
		{"不是合并挑战的库", func(b map[string]any) { b["format"] = "single" }, "format"},
		{"缺推理强度", func(b map[string]any) { delete(b, "effort") }, "effort"},
		{"instructions 不同", func(b map[string]any) { b["instructions_sha256"] = strings.Repeat("0", 64) }, "instructions"},
		{"特征族不同", func(b map[string]any) { b["families"] = []any{"word"} }, "特征族"},
		{"alpha 为 null", func(b map[string]any) { b["alpha"] = nil }, "alpha"},
		{"模型重复", func(b map[string]any) { b["models"] = append(b["models"].([]any), "gpt-5.5") }, "重复"},
		{"缺题目全文", func(b map[string]any) { b["challenge"].(map[string]any)["prompt"] = "" }, "题目"},
		{"题与 tasks 对不上", func(b map[string]any) { b["challenge"].(map[string]any)["sections"].([]any)[0] = "x" }, "对不上"},
		{"题名重复", func(b map[string]any) {
			tasks := b["tasks"].([]any)
			sections := b["challenge"].(map[string]any)["sections"].([]any)
			tasks[1], sections[1] = tasks[0], sections[0]
		}, "重复"},
		{"缺两层参数", func(b map[string]any) { delete(b, "two_level") }, "two_level"},
		{"组里有库外模型", func(b map[string]any) { twoLevel(b)["group"] = []any{"gpt-6-astra", "gpt-x"} }, "group"},
		{"阈值越界", func(b map[string]any) { twoLevel(b)["tau"] = 1.0 }, "tau"},
		{"最多份数小于第二层最少份数", func(b map[string]any) { twoLevel(b)["max_rounds"] = 1 }, "max_rounds"},
		{"最多份数过大", func(b map[string]any) { twoLevel(b)["max_rounds"] = 11 }, "max_rounds"},
		{"第二层截断值相加会溢出", func(b map[string]any) { twoLevel(b)["pair"].(map[string]any)["cap"] = 1e308 }, "pair.cap × max_rounds"},
		{"第一层温度为 0", func(b map[string]any) { twoLevel(b)["beta1"] = 0.0 }, "beta1"},
		{"缺 pair.m", func(b map[string]any) { delete(twoLevel(b)["pair"].(map[string]any), "m") }, "pair.m"},
		{"pair.cap 为 null", func(b map[string]any) { twoLevel(b)["pair"].(map[string]any)["cap"] = nil }, "pair.cap"},
		{"计数缺一个模型", func(b map[string]any) {
			delete(b["counts"].(map[string]any)["pk-email"].(map[string]any)["word"].(map[string]any), "gpt-5.5")
		}, "计数"},
		{"整个模型的用词计数为 null", func(b map[string]any) { familyCounts(b, "word")["gpt-5.5"] = nil }, "缺少模型"},
		{"整个模型的词对计数为 null", func(b map[string]any) { familyCounts(b, "bigram")["gpt-5.5"] = nil }, "缺少模型"},
		{"计数为 null", func(b map[string]any) { c := firstCount(b); c[anyKey(c)] = nil }, "非负整数"},
		{"计数总和溢出", func(b map[string]any) { familyCounts(b, "word")["gpt-5.5"] = map[string]any{"x": 1e308, "y": 1e308} }, "有限值"},
		{"alpha 极大使分母溢出", func(b map[string]any) { b["alpha"] = 1e308 }, "有限值"},
		{"alpha 极小使概率下溢", func(b map[string]any) { b["alpha"] = 5e-324 }, "有限值"},
		{"计数为负", func(b map[string]any) { c := firstCount(b); c[anyKey(c)] = -1.0 }, "非负整数"},
		{"计数不是整数", func(b map[string]any) { c := firstCount(b); c[anyKey(c)] = 1.5 }, "非负整数"},
		{"计数多一道题", func(b map[string]any) {
			counts := b["counts"].(map[string]any)
			counts["pk-extra"] = counts["pk-email"]
		}, "道题"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b map[string]any
			if err := json.Unmarshal(kongFingerprintBankJSON, &b); err != nil {
				t.Fatal(err)
			}
			tc.mutate(b)
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			_, err = kongParseFingerprintBank(raw, "test")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v，期望含 %q", err, tc.want)
			}
		})
	}
	if _, err := kongParseFingerprintBank([]byte("{"), "test"); err == nil {
		t.Fatal("不是合法 JSON 时应拒绝")
	}
	// 原样重新编码的库照常加载：上面的拒绝都来自各自的改动。
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kongParseFingerprintBank(raw, "test"); err != nil {
		t.Fatalf("未改动的库应能加载: %v", err)
	}
}
