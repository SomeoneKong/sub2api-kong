package service

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// 账号指纹测试的文本指纹库：每个模型在合并挑战每道题上的用词与词对计数，加上两层判定的参数。
// 由工作区的 fp-lab/library.py build 生成，设计见 DESIGN-openai-fingerprint-test.md。
//
// 默认那份随二进制走，同时留一个外部覆盖路径：上游静默更新模型后要能换一份库而不必重新构建镜像。
// 换了库，同一份回答会算出不同结论，所以每一份探测记录都带着当时用的库版本。

//go:embed kong_data/gpt_text_bank.json
var kongFingerprintBankJSON []byte

// KongFingerprintBankPathEnv 指向一份外部指纹库，覆盖内置的那份。
const KongFingerprintBankPathEnv = "KONG_FINGERPRINT_BANK"

const (
	kongTextBankSchema = "fp-lab-text-library"
	kongTextBankFormat = "packed"
	// kongTextMaxRoundsLimit 是库里最多份数的上限：每份都是这个账号上的一次真实请求。
	kongTextMaxRoundsLimit = 10
	// kongTextPairLogitLimit 是第二层各份截断后相加的上限，保证相加不溢出、sigmoid 不饱和成 0 或 1。
	kongTextPairLogitLimit = 700
)

// kongTextFamilies 是评分用的特征族，顺序与库文件的 families 一致。
var kongTextFamilies = []string{"word", "bigram"}

// KongFingerprintChallenge 是一份挑战：题目全文与请求条件都来自指纹库，换题就是换库。
type KongFingerprintChallenge struct {
	ID     string
	Prompt string
	// Effort 是请求的推理强度，与建库时一致。
	Effort string
}

// kongTextBankFile 是库文件里参与评分与判定的字段。数值字段用指针：缺字段与 null 都解成 nil，
// 与合法的 0 区分开，加载时一律拒绝。
type kongTextBankFile struct {
	Schema             string   `json:"schema"`
	BuiltAt            string   `json:"built_at"`
	Effort             string   `json:"effort"`
	Models             []string `json:"models"`
	Tasks              []string `json:"tasks"`
	Families           []string `json:"families"`
	Alpha              *float64 `json:"alpha"`
	Format             string   `json:"format"`
	InstructionsSHA256 string   `json:"instructions_sha256"`
	Challenge          *struct {
		ID       string   `json:"id"`
		Prompt   string   `json:"prompt"`
		Sections []string `json:"sections"`
	} `json:"challenge"`
	TwoLevel *struct {
		Group        []string `json:"group"`
		Tau          *float64 `json:"tau"`
		MaxRounds    *int     `json:"max_rounds"`
		PairMinParts *int     `json:"pair_min_parts"`
		Beta1        *float64 `json:"beta1"`
		Pair         *struct {
			W   *float64 `json:"w"`
			M   *float64 `json:"m"`
			Cap *float64 `json:"cap"`
		} `json:"pair"`
	} `json:"two_level"`
	// Counts 是 题 → 特征族 → 模型 → 特征 → 计数。
	Counts map[string]map[string]map[string]map[string]*float64 `json:"counts"`
}

// kongTextFamilyTable 是一道题、一个特征族预先算好的对数概率：已见特征为
// log((c + α) / (N + α·V))，未见特征为 log(α / (N + α·V))，按模型顺序排列。
type kongTextFamilyTable struct {
	logProb map[string][]float64
	unseen  []float64
}

// KongFingerprintBank 是加载并校验过的文本指纹库。
type KongFingerprintBank struct {
	// Models 是库里的候选模型，也就是可选的目标。
	Models    []string
	Challenge KongFingerprintChallenge
	// Group 是难分的一对：第一层把它们合并成一类，第二层再分。
	Group [2]string
	// sections 是合并挑战各题的名字，按题号排列；tables 与它对齐，每题按 kongTextFamilies 的顺序。
	sections []string
	tables   [][]kongTextFamilyTable

	tau, beta1            float64
	pairW, pairM, pairCap float64
	maxParts, pairMinPts  int

	builtAt string
	digest  string
	source  string
}

func kongTextPositive(v *float64) bool {
	return v != nil && *v > 0 && !math.IsInf(*v, 0)
}

func kongTextFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// kongParseFingerprintBank 解析并校验一份文本指纹库。
//
// 校验不是洁癖：计数缺了一个模型、题目对不上号时，评分照样会算出一个看起来正常的分数，而不是报错。
func kongParseFingerprintBank(data []byte, source string) (*KongFingerprintBank, error) {
	var file kongTextBankFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("解析指纹库(%s): %w", source, err)
	}
	fail := func(format string, args ...any) (*KongFingerprintBank, error) {
		return nil, fmt.Errorf("指纹库(%s)%s", source, fmt.Sprintf(format, args...))
	}
	if file.Schema != kongTextBankSchema {
		return fail("的 schema 是 %q，应为 %q", file.Schema, kongTextBankSchema)
	}
	if file.Format != kongTextBankFormat {
		return fail("的 format 是 %q，应为 %q", file.Format, kongTextBankFormat)
	}
	if strings.TrimSpace(file.Effort) == "" {
		return fail("缺少推理强度 effort")
	}
	// 建库时的 instructions 必须就是测试请求要带的这一份：instructions 不同，模型的写法就不同。
	sum := sha256.Sum256([]byte(kongFingerprintInstructions()))
	if file.InstructionsSHA256 != hex.EncodeToString(sum[:]) {
		return fail("的 instructions 摘要与网关的默认 instructions 不一致")
	}
	if strings.Join(file.Families, ",") != strings.Join(kongTextFamilies, ",") {
		return fail("的特征族是 %v，应为 %v", file.Families, kongTextFamilies)
	}
	if !kongTextPositive(file.Alpha) {
		return fail("的平滑系数 alpha 必须是有限正数")
	}
	alpha := *file.Alpha

	if len(file.Models) < 2 {
		return fail("至少要有两个模型")
	}
	modelSeen := make(map[string]bool, len(file.Models))
	for i, m := range file.Models {
		if strings.TrimSpace(m) == "" || m != strings.TrimSpace(m) {
			return fail("的第 %d 个模型名为空或带空白", i+1)
		}
		if modelSeen[m] {
			return fail("的模型 %q 重复", m)
		}
		modelSeen[m] = true
	}

	ch := file.Challenge
	if ch == nil || strings.TrimSpace(ch.ID) == "" || strings.TrimSpace(ch.Prompt) == "" {
		return fail("缺少挑战的 id 或题目全文")
	}
	if len(ch.Sections) == 0 || len(ch.Sections) > 99 || len(ch.Sections) != len(file.Tasks) {
		return fail("的挑战有 %d 道题、tasks 有 %d 项，必须一致且不超过 99", len(ch.Sections), len(file.Tasks))
	}
	sectionSeen := make(map[string]bool, len(ch.Sections))
	for i, name := range ch.Sections {
		if name == "" || file.Tasks[i] != "pk-"+name {
			return fail("的第 %d 道题 %q 与 tasks 的 %q 对不上", i+1, name, file.Tasks[i])
		}
		// 题名重复时拆题只认第一次出现，评分却按题号逐个计入，同一道题会算两次、另一道题丢掉。
		if sectionSeen[name] {
			return fail("的题名 %q 重复", name)
		}
		sectionSeen[name] = true
	}

	tl := file.TwoLevel
	if tl == nil || tl.Pair == nil {
		return fail("缺少两层判定参数 two_level")
	}
	if len(tl.Group) != 2 || tl.Group[0] == tl.Group[1] || !modelSeen[tl.Group[0]] || !modelSeen[tl.Group[1]] {
		return fail("的 two_level.group 必须是库里两个不同的模型，实际 %v", tl.Group)
	}
	if tl.Tau == nil || !(*tl.Tau > 0.5 && *tl.Tau < 1) {
		return fail("的阈值 tau 必须在 (0.5, 1) 内")
	}
	if tl.MaxRounds == nil || tl.PairMinParts == nil || *tl.PairMinParts < 1 || *tl.MaxRounds < *tl.PairMinParts ||
		*tl.MaxRounds > kongTextMaxRoundsLimit {
		return fail("的份数参数必须满足 %d ≥ max_rounds ≥ pair_min_parts ≥ 1", kongTextMaxRoundsLimit)
	}
	if !kongTextPositive(tl.Beta1) || !kongTextPositive(tl.Pair.W) || !kongTextPositive(tl.Pair.Cap) {
		return fail("的 beta1、pair.w、pair.cap 必须是有限正数")
	}
	if *tl.Pair.Cap*float64(*tl.MaxRounds) > kongTextPairLogitLimit {
		return fail("的 pair.cap × max_rounds 不能超过 %d", kongTextPairLogitLimit)
	}
	if tl.Pair.M == nil || math.IsNaN(*tl.Pair.M) || math.IsInf(*tl.Pair.M, 0) {
		return fail("的 pair.m 必须是有限值")
	}

	bank := &KongFingerprintBank{
		Models:    file.Models,
		Challenge: KongFingerprintChallenge{ID: ch.ID, Prompt: ch.Prompt, Effort: file.Effort},
		Group:     [2]string{tl.Group[0], tl.Group[1]},
		sections:  ch.Sections,
		tables:    make([][]kongTextFamilyTable, len(file.Tasks)),
		tau:       *tl.Tau, beta1: *tl.Beta1,
		pairW: *tl.Pair.W, pairM: *tl.Pair.M, pairCap: *tl.Pair.Cap,
		maxParts: *tl.MaxRounds, pairMinPts: *tl.PairMinParts,
		builtAt: file.BuiltAt, source: source,
	}
	for ti, task := range file.Tasks {
		for _, family := range kongTextFamilies {
			byModel := file.Counts[task][family]
			if len(byModel) != len(file.Models) {
				return fail("的 %s/%s 计数有 %d 个模型，应为 %d", task, family, len(byModel), len(file.Models))
			}
			vocab := map[string]bool{}
			totals := make([]float64, len(file.Models))
			for mi, m := range file.Models {
				counts, ok := byModel[m]
				// 计数对象为 null 时键在、值是 nil map，遍历不报错，会被当成这个模型什么都没见过。
				if !ok || counts == nil {
					return fail("的 %s/%s 缺少模型 %s 的计数", task, family, m)
				}
				for feature, c := range counts {
					if c == nil || *c < 0 || *c != math.Trunc(*c) || math.IsInf(*c, 0) {
						return fail("的 %s/%s/%s 特征 %q 的计数必须是非负整数", task, family, m, feature)
					}
					vocab[feature] = true
					totals[mi] += *c
				}
			}
			// 词表大小是全部模型的并集再加 1，留给没见过的特征。
			size := float64(len(vocab) + 1)
			table := kongTextFamilyTable{logProb: make(map[string][]float64, len(vocab)), unseen: make([]float64, len(file.Models))}
			// 每个参数单独有限，算出来的对数概率仍可能溢出或下溢成无穷，之后的得分与概率会变成 NaN：
			// NaN 参与比较恒为假，判定会照常走下去给出错误结论，落库时编码也会失败。
			for mi, m := range file.Models {
				denom := totals[mi] + alpha*size
				table.unseen[mi] = math.Log(alpha / denom)
				if !kongTextFinite(table.unseen[mi]) {
					return fail("的 %s/%s/%s 计数或 alpha 取值使对数概率不是有限值", task, family, m)
				}
				for feature := range vocab {
					row := table.logProb[feature]
					if row == nil {
						row = make([]float64, len(file.Models))
						table.logProb[feature] = row
					}
					row[mi] = math.Log((kongTextCount(byModel[m], feature) + alpha) / denom)
					if !kongTextFinite(row[mi]) {
						return fail("的 %s/%s/%s 计数或 alpha 取值使对数概率不是有限值", task, family, m)
					}
				}
			}
			bank.tables[ti] = append(bank.tables[ti], table)
		}
	}
	if len(file.Counts) != len(file.Tasks) {
		return fail("的计数有 %d 道题，应为 %d", len(file.Counts), len(file.Tasks))
	}

	digest := sha256.Sum256(data)
	bank.digest = hex.EncodeToString(digest[:])
	return bank, nil
}

// kongFingerprintInstructions 是挑战请求带的 instructions：网关的默认 instructions，换行统一为 LF。
// 嵌入的 instructions.txt 在 Windows 工作区会被检出成 CRLF；统一之后各平台构建发出的内容相同，也与建库时发送的相同。
func kongFingerprintInstructions() string {
	return strings.ReplaceAll(openai.DefaultInstructions, "\r\n", "\n")
}

func kongTextCount(counts map[string]*float64, feature string) float64 {
	if c := counts[feature]; c != nil {
		return *c
	}
	return 0
}

// MaxParts 是一次测试最多发几份挑战。
func (b *KongFingerprintBank) MaxParts() int {
	return b.maxParts
}

// GroupClass 是第一层里难分的一对合并成的那一类的标识。
func (b *KongFingerprintBank) GroupClass() string {
	return b.Group[0] + "|" + b.Group[1]
}

// DisplayName 返回候选的展示名：模型就是模型 id，合并的那一类列出两个模型。
func (b *KongFingerprintBank) DisplayName(id string) string {
	if id == b.GroupClass() {
		return b.Group[0] + " / " + b.Group[1]
	}
	return id
}

// HasModel 报告该模型是否在库里。闭集归因下库外模型会被归到最像的候选，所以目标必须在库里。
func (b *KongFingerprintBank) HasModel(id string) bool {
	for _, m := range b.Models {
		if m == id {
			return true
		}
	}
	return false
}

// Version 是写进每一份探测记录的库标识。换了库或判定规则，同一份回答会得出不同结论，
// 没有版本标识就既不能复核也不能重算。
func (b *KongFingerprintBank) Version() map[string]any {
	return map[string]any{
		"schema":       kongTextBankSchema,
		"built_at":     b.builtAt,
		"digest":       b.digest,
		"source":       b.source,
		"challenge":    b.Challenge.ID,
		"rule_version": kongFingerprintRuleVersion,
	}
}

var (
	kongFingerprintBankOnce sync.Once
	kongFingerprintBank     *KongFingerprintBank
	kongFingerprintBankErr  error
)

// KongFingerprintBankLoad 返回指纹库，优先使用环境变量指定的外部文件。
// 结果缓存：库在进程生命周期内不变，换库要重启，这样同一进程内的判定口径是一致的。
func KongFingerprintBankLoad() (*KongFingerprintBank, error) {
	kongFingerprintBankOnce.Do(func() {
		if path := strings.TrimSpace(os.Getenv(KongFingerprintBankPathEnv)); path != "" {
			// 路径来自服务端环境变量，由运维自己设置，不是请求可控的输入。Clean 只为规范化。
			data, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // G703：路径源自部署环境的配置，不含用户输入
			if err != nil {
				kongFingerprintBankErr = fmt.Errorf("读取外部指纹库 %s: %w", path, err)
				return
			}
			kongFingerprintBank, kongFingerprintBankErr = kongParseFingerprintBank(data, "file:"+path)
			return
		}
		kongFingerprintBank, kongFingerprintBankErr = kongParseFingerprintBank(kongFingerprintBankJSON, "embedded")
	})
	return kongFingerprintBank, kongFingerprintBankErr
}
