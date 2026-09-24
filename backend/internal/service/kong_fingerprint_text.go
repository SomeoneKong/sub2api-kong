package service

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// 文本指纹的评分与两层判定。与 fp-lab/library.py 的 split_packed / score / decide 逐条对应，
// testdata/kong_fingerprint_text_golden.json 锁住两边算出的结果一致。

// kongTextHeaderRe 是合并回答里每道题开头的 "### N" 行。题号只认 ASCII 数字。
var kongTextHeaderRe = regexp.MustCompile(`(?m)^[ \t]*#{2,4}[ \t]*([0-9]{1,2})[ \t]*\.?[ \t]*$`)

var kongTextWordRe = regexp.MustCompile(`[a-z']+`)

// kongTextSplit 按 "### N" 行把合并回答拆成 {题名: 该题回答}。题号超出范围的行只当分隔，
// 同一题号重复出现时只取第一次；拆不出的题不出现在结果里。
func kongTextSplit(text string, sections []string) map[string]string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	marks := kongTextHeaderRe.FindAllStringSubmatchIndex(text, -1)
	out := make(map[string]string)
	for i, mark := range marks {
		number, err := strconv.Atoi(text[mark[2]:mark[3]])
		if err != nil || number < 1 || number > len(sections) {
			continue
		}
		name := sections[number-1]
		if _, seen := out[name]; seen {
			continue
		}
		end := len(text)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		out[name] = strings.TrimSpace(text[mark[1]:end])
	}
	return out
}

// kongTextCounter 是按首次出现顺序记录的特征计数。
type kongTextCounter struct {
	keys   []string
	counts map[string]int
	total  int
}

func (c *kongTextCounter) add(key string) {
	if c.counts == nil {
		c.counts = make(map[string]int)
	}
	if _, ok := c.counts[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.counts[key]++
	c.total++
}

// kongTextFeatures 取一段回答的用词与相邻词对，顺序与 kongTextFamilies 一致。
//
// 小写化与 Python 的 str.lower 对齐：U+0130（带点大写 I）在 Python 里变成 "i" 加组合点，
// Go 的 ToLower 只给 "i"，两边切出的词会不同。
func kongTextFeatures(text string) [2]kongTextCounter {
	lower := strings.ToLower(strings.ReplaceAll(text, "İ", "i̇"))
	words := kongTextWordRe.FindAllString(lower, -1)
	var out [2]kongTextCounter
	for i, w := range words {
		out[0].add(w)
		if i > 0 {
			out[1].add(words[i-1] + " " + w)
		}
	}
	return out
}

// scoreSection 给一道题的回答对每个模型打分：每个特征族的对数似然按特征总数平均后相加。
func (b *KongFingerprintBank) scoreSection(task int, body string) []float64 {
	out := make([]float64, len(b.Models))
	for f, counter := range kongTextFeatures(body) {
		if counter.total == 0 {
			continue
		}
		table := b.tables[task][f]
		for mi := range out {
			var sum float64
			for _, key := range counter.keys {
				lp := table.unseen[mi]
				if row, ok := table.logProb[key]; ok {
					lp = row[mi]
				}
				sum += float64(counter.counts[key]) * lp
			}
			out[mi] += sum / float64(counter.total)
		}
	}
	return out
}

// kongTextPartScore 是一份合并回答的评分。
type kongTextPartScore struct {
	// SectionCount 是拆出的题数。
	SectionCount int
	// Sections 是 题名 → 模型 → 得分，只含拆出的题。
	Sections map[string]map[string]float64
	// Scores 是各题之和，模型 → 得分。
	Scores map[string]float64
}

// ScoreAnswer 拆分一份合并回答并打分。
func (b *KongFingerprintBank) ScoreAnswer(text string) kongTextPartScore {
	split := kongTextSplit(text, b.sections)
	out := kongTextPartScore{
		SectionCount: len(split),
		Sections:     make(map[string]map[string]float64, len(split)),
		Scores:       make(map[string]float64, len(b.Models)),
	}
	for _, m := range b.Models {
		out.Scores[m] = 0
	}
	for ti, name := range b.sections {
		body, ok := split[name]
		if !ok {
			continue
		}
		scores := b.scoreSection(ti, body)
		section := make(map[string]float64, len(b.Models))
		for mi, m := range b.Models {
			section[m] = scores[mi]
			out.Scores[m] += scores[mi]
		}
		out.Sections[name] = section
	}
	return out
}

// kongTextClassProb 是第一层的一类与它的概率。
type kongTextClassProb struct {
	Class       string
	Probability float64
}

// level1 是第一层：softmax(β1 × 得分)，难分的一对合并成一类、概率相加。
// 顺序固定为组外模型按库里的顺序在前、合并的那一类在最后，并列时取靠前的。
func (b *KongFingerprintBank) level1(scores map[string]float64) []kongTextClassProb {
	top := math.Inf(-1)
	for _, m := range b.Models {
		top = math.Max(top, scores[m])
	}
	weights := make([]float64, len(b.Models))
	var total float64
	for i, m := range b.Models {
		weights[i] = math.Exp(b.beta1 * (scores[m] - top))
		total += weights[i]
	}
	out := make([]kongTextClassProb, 0, len(b.Models))
	var group float64
	for i, m := range b.Models {
		p := weights[i] / total
		if m == b.Group[0] || m == b.Group[1] {
			group += p
			continue
		}
		out = append(out, kongTextClassProb{Class: m, Probability: p})
	}
	return append(out, kongTextClassProb{Class: b.GroupClass(), Probability: group})
}

func kongTextTop(classes []kongTextClassProb) kongTextClassProb {
	best := classes[0]
	for _, c := range classes[1:] {
		if c.Probability > best.Probability {
			best = c
		}
	}
	return best
}

// PartAttribution 是一份回答自己在第一层最像的那一类。
func (b *KongFingerprintBank) PartAttribution(scores map[string]float64) string {
	return kongTextTop(b.level1(scores)).Class
}

// pairProb 是第二层：每份的得分差按等方差正态换算成对数几率并截到 ±cap，各份相加后取 sigmoid，
// 给出这一对里第一个模型的概率。
func (b *KongFingerprintBank) pairProb(parts []map[string]float64) float64 {
	var z float64
	for _, part := range parts {
		logit := b.pairW * (part[b.Group[0]] - part[b.Group[1]] - b.pairM)
		z += math.Max(math.Min(logit, b.pairCap), -b.pairCap)
	}
	z = math.Max(math.Min(z, 700), -700)
	return 1 / (1 + math.Exp(-z))
}

// kongTextDecision 是一份挑战之后的判定状态。
type kongTextDecision struct {
	// Verdict 为空表示还不能下结论，继续发下一份。
	Verdict string
	Reason  string
	// Decided 是结论指向的模型；第二层分不清时是合并的那一类。
	Decided string
	// Level1 是累计得分的第一层分布；没有有效份时为空。
	Level1 []kongTextClassProb
	// Pair 是第二层这一对各自的概率，按 Group 的顺序；没进入第二层时为空。
	Pair []kongTextClassProb
}

// Decide 按两层判定规则给出第 rounds 份之后的状态。parts 是各份有效回答的得分，rounds 是已用掉的份数
// （含无效份）。规则见 DESIGN-openai-fingerprint-test.md。
func (b *KongFingerprintBank) Decide(parts []map[string]float64, rounds int, target string) kongTextDecision {
	last := rounds >= b.maxParts
	var d kongTextDecision
	exhausted := func() kongTextDecision {
		if last {
			d.Verdict, d.Reason = KongFingerprintVerdictInconclusive, kongFingerprintEndPartsExhausted
		}
		return d
	}
	if len(parts) == 0 {
		return exhausted()
	}
	cumulative := make(map[string]float64, len(b.Models))
	for _, m := range b.Models {
		for _, part := range parts {
			cumulative[m] += part[m]
		}
	}
	d.Level1 = b.level1(cumulative)
	top := kongTextTop(d.Level1)
	// 写成取反：概率为 NaN 时也按不够把握处理。
	if !(top.Probability >= b.tau) {
		return exhausted()
	}
	// 不带票的各份请求不保证路由到同一个模型：各份第一层指向不同时只报告观察到了差异。
	for _, part := range parts {
		if b.PartAttribution(part) != top.Class {
			d.Verdict, d.Reason = KongFingerprintVerdictInconclusive, kongFingerprintEndPartsDisagree
			return d
		}
	}
	verdictFor := func(decided string) string {
		if decided == target {
			return KongFingerprintVerdictMatch
		}
		return KongFingerprintVerdictMismatch
	}
	if top.Class != b.GroupClass() {
		d.Verdict, d.Reason, d.Decided = verdictFor(top.Class), kongFingerprintEndConfident, top.Class
		return d
	}
	p := b.pairProb(parts)
	d.Pair = []kongTextClassProb{{Class: b.Group[0], Probability: p}, {Class: b.Group[1], Probability: 1 - p}}
	best := b.Group[1]
	if p >= 0.5 {
		best = b.Group[0]
	}
	switch {
	case target != b.Group[0] && target != b.Group[1]:
		d.Verdict, d.Reason, d.Decided = KongFingerprintVerdictMismatch, kongFingerprintEndConfident, best
	case len(parts) >= b.pairMinPts && math.Max(p, 1-p) >= b.tau:
		d.Verdict, d.Reason, d.Decided = verdictFor(best), kongFingerprintEndConfident, best
	case last:
		d.Verdict, d.Reason, d.Decided = KongFingerprintVerdictInconclusive, kongFingerprintEndPairUnresolved, b.GroupClass()
	}
	return d
}
