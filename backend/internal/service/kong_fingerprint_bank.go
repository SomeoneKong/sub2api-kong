package service

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// 指纹归因的校准资料（ModelTrace 的 unified bank）。
//
// 默认那份随二进制走，同时留一个外部覆盖路径：模型会演进，校准资料过期时应当能换一份而不必
// 重新构建镜像。两者都可能被换掉，所以每次归因都要把当时用的版本记进探测记录——档 "1" 不是
// 一份永不变的资料，换了资料同一序列会算出不同概率，没有版本标识就既不能复核也不能重算。

//go:embed kong_data/unified_bank.json
var kongFingerprintBankJSON []byte

// KongFingerprintBankPathEnv 指向一份外部校准资料，覆盖内置的那份。
const KongFingerprintBankPathEnv = "KONG_FINGERPRINT_BANK"

// KongFingerprintModel 是库里的一个候选模型。
type KongFingerprintModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Family      string `json:"family"`
	FamilyName  string `json:"family_name"`
}

// KongFingerprintHellinger 是 Hellinger 分支的资料。
type KongFingerprintHellinger struct {
	FeatureMean   []float64   `json:"feature_mean"`
	FeatureScale  []float64   `json:"feature_scale"`
	NuisanceBasis [][]float64 `json:"nuisance_basis"`
	Centroids     [][]float64 `json:"centroids"`
}

// KongFingerprintOrderedBlocks 是有序块分支的资料。
type KongFingerprintOrderedBlocks struct {
	Weight               float64       `json:"weight"`
	FeatureMean          []float64     `json:"feature_mean"`
	FeatureScale         []float64     `json:"feature_scale"`
	NuisanceBasis        [][]float64   `json:"nuisance_basis"`
	Centroids            [][]float64   `json:"centroids"`
	EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
}

// KongFingerprintRobust 汇总两个分支。
type KongFingerprintRobust struct {
	ModelOrder    []string                     `json:"model_order"`
	Hellinger     KongFingerprintHellinger     `json:"hellinger"`
	OrderedBlocks KongFingerprintOrderedBlocks `json:"ordered_blocks"`
}

// KongFingerprintCalibration 是某个份数档的温度。
type KongFingerprintCalibration struct {
	Beta       float64 `json:"beta"`
	CVAccuracy float64 `json:"cv_accuracy"`
	Fallback   bool    `json:"fallback"`
}

// KongFingerprintBank 是整份校准资料。
type KongFingerprintBank struct {
	Schema      string                                `json:"schema"`
	BuiltAt     string                                `json:"built_at"`
	Models      []KongFingerprintModel                `json:"models"`
	Robust      KongFingerprintRobust                 `json:"robust"`
	Calibration map[string]KongFingerprintCalibration `json:"calibration"`

	// digest 是资料内容的摘要，随归因结果一起记进探测记录。
	digest    string
	source    string
	nameByID  map[string]string
	modelSeen map[string]bool
}

// DisplayName 返回模型的展示名，未知 id 原样返回。
func (b *KongFingerprintBank) DisplayName(id string) string {
	if b == nil || b.nameByID == nil {
		return id
	}
	if name, ok := b.nameByID[id]; ok && name != "" {
		return name
	}
	return id
}

// Version 是要写进探测记录的资料标识（设计 §5.6）。少了它，历史结论既不能复核也不能重算。
func (b *KongFingerprintBank) Version() map[string]any {
	if b == nil {
		return map[string]any{}
	}
	return map[string]any{
		"schema":   b.Schema,
		"built_at": b.BuiltAt,
		"digest":   b.digest,
		"source":   b.source,
		"models":   len(b.Robust.ModelOrder),
	}
}

// HasModel 报告该模型是否在库里。闭集归因下，库外模型会被归到最相似的现有候选——所以准入
// 判定要用的那个模型 id 必须确实在库中，否则「判为 astra」根本不成立。
func (b *KongFingerprintBank) HasModel(id string) bool {
	return b != nil && b.modelSeen[id]
}

// kongParseFingerprintBank 解析并校验一份校准资料。
//
// 校验不是洁癖：维度对不上时算法会算出一个「看起来正常」的分数，而不是报错——那正是移植偏差
// 最危险的形态。
// kongCheckScale 校验一组 scale 全部有限且严格为正。
//
// 只查长度是不够的：负 scale 会把标准分整体翻向，算出一个高置信的**错**模型；极小正值
// （如 1e-320）让标准分溢出成 Inf，一路传到 softmax 变 NaN，而 NaN 参与比较恒为假——
// 归因「成功」返回，概率却是 NaN，表现成永远不下结论且无一处报错。
func kongCheckScale(scale []float64, source, branch string) error {
	for i, v := range scale {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return fmt.Errorf("校准资料(%s)的 %s scale 第 %d 维必须是有限正数，实际 %v", source, branch, i, v)
		}
	}
	return nil
}

// kongCheckFinite 校验一组数值全部有限。
func kongCheckFinite(values []float64, source, what string) error {
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("校准资料(%s)的 %s 第 %d 维不是有限值: %v", source, what, i, v)
		}
	}
	return nil
}

// kongStrictFloat 是「拒绝 null 的浮点数」。
//
// `json.Unmarshal` 把数值位置上的 `null` 解成 0，而 0 是一个**合法**的中心分量——之后的维度校验
// 与有限性校验都看不出区别。一个缺失值被当成 0 参与打分，足以把归因从一个模型翻到另一个（实测把
// 非目标模型的中心元素换成 null 后，原本判 sol/0.995 的样本变成 astra/0.9999999999）。
//
// 为什么用自定义标量类型而不是按路径扫原文：**校验必须与实际解码用同一套字段名规则**。
// `encoding/json` 匹配字段名是**大小写不敏感**的，`"CENTROIDS"` 照样会被解进 `Centroids`；而按
// gjson 路径扫只认精确小写，那个键就整段漏检。重复键也一样——`encoding/json` 会把每一次出现都
// 解一遍，null 那次自然会在这里报错。把规则交给同一个解码器，就不存在两套规则的缝隙。
type kongStrictFloat float64

func (f *kongStrictFloat) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		return errors.New("参与打分的数值不能是 null（缺失值会被解成 0，与合法的 0 无法区分）")
	}
	var v float64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*f = kongStrictFloat(v)
	return nil
}

// kongStrictBranch 是一个分支里参与打分的数值字段。字段名与 tag 必须与
// KongFingerprintHellinger / KongFingerprintOrderedBlocks 完全一致。
type kongStrictBranch struct {
	FeatureMean          []kongStrictFloat     `json:"feature_mean"`
	FeatureScale         []kongStrictFloat     `json:"feature_scale"`
	NuisanceBasis        [][]kongStrictFloat   `json:"nuisance_basis"`
	Centroids            [][]kongStrictFloat   `json:"centroids"`
	EnvironmentCentroids [][][]kongStrictFloat `json:"environment_centroids"`
	Weight               *kongStrictFloat      `json:"weight"`
}

// kongStrictBank 只覆盖参与打分的字段。
//
// 不参与算法的字段（`cv_accuracy`、`built_at`、展示名等）刻意不在这里：官方实现对它们容忍 null，
// 同一份资料在那边照常加载、归因结果一字不差。比对端更严就是一条兼容性回归，而这条校验在启动
// 路径上——代价是整个服务起不来。
type kongStrictBank struct {
	Robust struct {
		Hellinger     kongStrictBranch `json:"hellinger"`
		OrderedBlocks kongStrictBranch `json:"ordered_blocks"`
	} `json:"robust"`
	Calibration map[string]struct {
		Beta *kongStrictFloat `json:"beta"`
	} `json:"calibration"`
}

// kongRejectScoringNull 用与实际装配**同一个解码器**再解一遍参与打分的字段，借此挡住 null。
func kongRejectScoringNull(data []byte, source string) error {
	var strict kongStrictBank
	if err := json.Unmarshal(data, &strict); err != nil {
		return fmt.Errorf("校准资料(%s)的打分字段校验失败: %w", source, err)
	}
	return nil
}

func kongParseFingerprintBank(data []byte, source string) (*KongFingerprintBank, error) {
	if !gjson.ValidBytes(data) {
		return nil, fmt.Errorf("校准资料(%s)不是合法 JSON", source)
	}
	if err := kongRejectScoringNull(data, source); err != nil {
		return nil, err
	}
	var bank KongFingerprintBank
	if err := json.Unmarshal(data, &bank); err != nil {
		return nil, fmt.Errorf("解析校准资料(%s): %w", source, err)
	}

	models := len(bank.Robust.ModelOrder)
	if models == 0 {
		return nil, fmt.Errorf("校准资料(%s)缺少 robust.model_order", source)
	}
	// `models` 与 `robust.model_order` 必须逐项一致。
	//
	// 归因结果按 `model_order` 标注（中心矩阵的行序就是它），而官方 fingerprint.py 按 `models`
	// 标注。两份序列不一致时双方对同一个分数向量给出不同的模型名——只把 model_order 里的两个
	// 标签对调，同一份资料、同一个样本，这里就会把 sol 的分数报成 astra，而 HasModel 照样通过。
	// 不能自动重排：无法据此确定中心矩阵采用的是哪一份顺序。
	if len(bank.Models) != models {
		return nil, fmt.Errorf("校准资料(%s)的 models 有 %d 项、model_order 有 %d 项，必须一致",
			source, len(bank.Models), models)
	}
	seenModel := make(map[string]bool, models)
	for i, id := range bank.Robust.ModelOrder {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("校准资料(%s)的 model_order 第 %d 项为空", source, i)
		}
		if seenModel[id] {
			return nil, fmt.Errorf("校准资料(%s)的 model_order 第 %d 项 %q 重复", source, i, id)
		}
		seenModel[id] = true
		if got := strings.TrimSpace(bank.Models[i].ID); got != id {
			return nil, fmt.Errorf("校准资料(%s)第 %d 项模型不一致：models=%q、model_order=%q", source, i, got, id)
		}
	}
	h := bank.Robust.Hellinger
	if len(h.FeatureMean) != kongFPDim || len(h.FeatureScale) != kongFPDim {
		return nil, fmt.Errorf("校准资料(%s)的 hellinger 特征维度应为 %d，实际 %d/%d",
			source, kongFPDim, len(h.FeatureMean), len(h.FeatureScale))
	}
	if err := kongCheckScale(h.FeatureScale, source, "hellinger"); err != nil {
		return nil, err
	}
	if err := kongCheckFinite(h.FeatureMean, source, "hellinger 均值"); err != nil {
		return nil, err
	}
	if len(h.Centroids) != models {
		return nil, fmt.Errorf("校准资料(%s)的 hellinger 中心数 %d 与模型数 %d 不符", source, len(h.Centroids), models)
	}
	for i, row := range h.Centroids {
		if len(row) != kongFPDim {
			return nil, fmt.Errorf("校准资料(%s)的 hellinger 中心 %d 维度为 %d，应为 %d", source, i, len(row), kongFPDim)
		}
	}
	for i, row := range h.NuisanceBasis {
		if len(row) != kongFPDim {
			return nil, fmt.Errorf("校准资料(%s)的 hellinger 干扰基 %d 维度为 %d，应为 %d", source, i, len(row), kongFPDim)
		}
	}

	ob := bank.Robust.OrderedBlocks
	// 权重必须落在 [0,1]：它是两个分支的凸组合系数，超出这个区间算出来的不是相似度。
	if ob.Weight < 0 || ob.Weight > 1 {
		return nil, fmt.Errorf("校准资料(%s)的有序块权重 %v 不在 [0,1] 内", source, ob.Weight)
	}
	// 官方口径是两条分支的融合（0.75 Hellinger + 0.25 有序块），所以**有序块分支必须存在**。
	//
	// 不能用「权重为 0 就跳过校验」来放过它：整段 `ordered_blocks` 缺失时所有字段都是 JSON 零值，
	// 与「有意把这一支关掉」长得一模一样，于是融合算法被静默改成纯 Hellinger——归因照样给出高
	// 置信结论，只是少了 0.25 的判据。缺字段与显式数值必须分开判。
	if ob.Weight == 0 || len(ob.Centroids) == 0 || len(ob.FeatureMean) == 0 || len(ob.FeatureScale) == 0 {
		return nil, fmt.Errorf("校准资料(%s)缺少有序块分支（weight=%v、中心 %d 个、均值 %d 维、scale %d 维）；"+
			"官方口径要求两条分支都在，缺一支会把融合算法静默改成纯 Hellinger",
			source, ob.Weight, len(ob.Centroids), len(ob.FeatureMean), len(ob.FeatureScale))
	}
	{
		if len(ob.FeatureMean) != kongFPBlockDim || len(ob.FeatureScale) != kongFPBlockDim {
			return nil, fmt.Errorf("校准资料(%s)的有序块特征维度应为 %d，实际 %d/%d",
				source, kongFPBlockDim, len(ob.FeatureMean), len(ob.FeatureScale))
		}
		if err := kongCheckScale(ob.FeatureScale, source, "有序块"); err != nil {
			return nil, err
		}
		if err := kongCheckFinite(ob.FeatureMean, source, "有序块均值"); err != nil {
			return nil, err
		}
		if len(ob.Centroids) != models {
			return nil, fmt.Errorf("校准资料(%s)的有序块中心数 %d 与模型数 %d 不符", source, len(ob.Centroids), models)
		}
		if len(ob.EnvironmentCentroids) == 0 {
			return nil, fmt.Errorf("校准资料(%s)的有序块权重非零但缺少 environment_centroids", source)
		}
		for i, row := range ob.Centroids {
			if len(row) != kongFPBlockDim {
				return nil, fmt.Errorf("校准资料(%s)的有序块中心 %d 维度为 %d，应为 %d", source, i, len(row), kongFPBlockDim)
			}
		}
		for i, row := range ob.NuisanceBasis {
			if len(row) != kongFPBlockDim {
				return nil, fmt.Errorf("校准资料(%s)的有序块干扰基 %d 维度为 %d，应为 %d", source, i, len(row), kongFPBlockDim)
			}
		}
		for i, env := range ob.EnvironmentCentroids {
			if len(env) != models {
				return nil, fmt.Errorf("校准资料(%s)的环境中心组 %d 含 %d 个模型，应为 %d", source, i, len(env), models)
			}
			for j, row := range env {
				if len(row) != kongFPBlockDim {
					return nil, fmt.Errorf("校准资料(%s)的环境中心 %d/%d 维度为 %d，应为 %d", source, i, j, len(row), kongFPBlockDim)
				}
			}
		}
	}
	for _, tier := range []string{"1", "2", "3"} {
		c, ok := bank.Calibration[tier]
		if !ok {
			return nil, fmt.Errorf("校准资料(%s)缺少档 %q", source, tier)
		}
		if c.Beta <= 0 || math.IsNaN(c.Beta) || math.IsInf(c.Beta, 0) {
			return nil, fmt.Errorf("校准资料(%s)档 %q 的 beta 必须是有限正数: %v", source, tier, c.Beta)
		}
	}

	sum := sha256.Sum256(data)
	bank.digest = hex.EncodeToString(sum[:])[:16]
	bank.source = source
	bank.nameByID = make(map[string]string, len(bank.Models))
	bank.modelSeen = make(map[string]bool, len(bank.Robust.ModelOrder))
	for _, m := range bank.Models {
		bank.nameByID[m.ID] = m.DisplayName
	}
	for _, id := range bank.Robust.ModelOrder {
		bank.modelSeen[id] = true
	}
	return &bank, nil
}

var (
	kongFingerprintBankOnce sync.Once
	kongFingerprintBank     *KongFingerprintBank
	kongFingerprintBankErr  error
)

// KongFingerprintBankLoad 返回校准资料，优先使用环境变量指定的外部文件。
// 结果缓存：资料在进程生命周期内不变，换资料要重启（这样同一进程内的归因口径才是一致的）。
func KongFingerprintBankLoad() (*KongFingerprintBank, error) {
	kongFingerprintBankOnce.Do(func() {
		if path := strings.TrimSpace(os.Getenv(KongFingerprintBankPathEnv)); path != "" {
			// 路径来自**服务端环境变量**，由运维自己设置，不是请求可控的输入；能设这个变量的人本来
			// 就能读服务进程可读的任何文件。Clean 只为规范化，真正的边界是「谁能设环境变量」。
			data, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // G703：路径源自部署环境的配置，不含用户输入
			if err != nil {
				kongFingerprintBankErr = fmt.Errorf("读取外部校准资料 %s: %w", path, err)
				return
			}
			kongFingerprintBank, kongFingerprintBankErr = kongParseFingerprintBank(data, "file:"+path)
			return
		}
		kongFingerprintBank, kongFingerprintBankErr = kongParseFingerprintBank(kongFingerprintBankJSON, "embedded")
	})
	return kongFingerprintBank, kongFingerprintBankErr
}
