package service

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"unicode"
)

// Codex 票据的指纹归因：ModelTrace 官方口径的 Go 移植。设计见 DESIGN-codex-ticket.md §4.1。
//
// 判据只需要 astra / 非 astra 的二分，但算法必须照官方来、不自创变体——偏差不会报错，只会
// 表现成「一直拿不到合格票」。移植正确性由 golden file 回归测试锁住（kong_fingerprint_test.go）。
//
// 两处最容易做错、也是复刻时最需要对齐 numpy 语义的地方：
//   - 多份回答不能合并数字序列再算：三条挑战是三个不同的 prompt，分布基准不同，池化会污染
//     特征。只能各自评分、在**分数层面**平均。
//   - 校准温度按**计入平均的有效份数**选（档 1/2/3）。平均会压低分数方差，温度必须跟着变。

const (
	kongFPValueMin = 1
	kongFPValueMax = 355
	kongFPDim      = kongFPValueMax - kongFPValueMin + 1
	kongFPAlpha    = 0.5
	// 有序块特征：4 段 × 16 个直方图桶 + 10 个末位数字桶
	kongFPBlockSegments = 4
	kongFPBlockBins     = 16
	kongFPLastDigitBins = 10
	kongFPBlockDim      = kongFPBlockSegments*kongFPBlockBins + kongFPLastDigitBins
)

// kongFPDigitRe 按 **Unicode 十进制数字**（Nd）取游程。
//
// Go 的 `\d` 只认 ASCII，而官方 Python 实现里 `\d` 认整个 Nd。差别不是「多认几个字符」：
// 全角 `１` 在 ASCII 口径下会被当成普通分隔符，于是 `1１3` 被拆成 `1` 和 `3` 两个整数，而 Python
// 得到一个 `113`。序列长度和取值全变，归因照样给出高置信结论、不报任何错。
var kongFPDigitRe = regexp.MustCompile(`[\p{Nd}]+`)

// kongFPHasNonASCIIDigit 报告文本里是否出现了非 ASCII 的十进制数字。
//
// 出现即把该份回答判为无效，而不是尝试解析：本项目的取舍是**宁可不用这一份，也不产出一个与
// 官方口径不同的序列**。少一份回答只会让置信度不够、再加一份挑战；序列算错却会安静地给出错档位。
func kongFPHasNonASCIIDigit(text string) bool {
	for _, r := range text {
		if r > unicode.MaxASCII && unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// kongFPParseNumbers 从回答里提取数字序列。
//
// 规则来自官方实现：取**最长的一段连续数字游程**，字母类分隔符会把游程切断（避免把散落在
// 说明文字里的数字也算进来），只保留 1..355 范围内的值。
func kongFPParseNumbers(text string) []int {
	var runs [][]int
	var current []int
	previousEnd := 0

	for _, loc := range kongFPDigitRe.FindAllStringIndex(text, -1) {
		separator := text[previousEnd:loc[0]]
		hasAlpha := false
		for _, r := range separator {
			if unicode.IsLetter(r) {
				hasAlpha = true
				break
			}
		}
		if len(current) > 0 && hasAlpha {
			runs = append(runs, current)
			current = nil
		}
		// 超长数字串直接跳过：它不可能落在值域内，按官方实现也只是不收集。
		if v, ok := kongFPAtoi(text[loc[0]:loc[1]]); ok && v >= kongFPValueMin && v <= kongFPValueMax {
			current = append(current, v)
		}
		previousEnd = loc[1]
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	if len(runs) == 0 {
		return nil
	}
	longest := runs[0]
	for _, r := range runs[1:] {
		if len(r) > len(longest) {
			longest = r
		}
	}
	return longest
}

// kongFPAtoi 解析十进制整数，溢出即视为不在值域内。
func kongFPAtoi(s string) (int, bool) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
		if v > kongFPValueMax {
			return 0, false
		}
	}
	return v, true
}

// kongFPMinimumNumbers 是一份回答被采纳所需的最少数字个数。
// expected 为 0（未声明期望长度）时退化为固定下限 80。
func kongFPMinimumNumbers(expected int) int {
	if expected <= 0 {
		return 80
	}
	byRatio := int(math.Ceil(float64(expected) * 0.55))
	if byRatio > 80 {
		return byRatio
	}
	return 80
}

func kongFPCountNumbers(numbers []int) []float64 {
	counts := make([]float64, kongFPDim)
	for _, n := range numbers {
		if n >= kongFPValueMin && n <= kongFPValueMax {
			counts[n-kongFPValueMin]++
		}
	}
	return counts
}

// kongFPStandardize 按总体标准差（ddof=0）做 z-score，与 numpy 侧一致。
func kongFPStandardize(values []float64, what string) ([]float64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if err := kongFPRequireFinite(values, what); err != nil {
		// 上一步（矩阵乘法等）已经溢出了。不先挡住的话，下面的标准化会把 Inf 除成 0 或 NaN，
		// 把一个已经坏掉的向量变成看起来正常的数字。
		return nil, err
	}
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= float64(len(values))
	if math.IsNaN(mean) || math.IsInf(mean, 0) {
		return nil, fmt.Errorf("%s 的均值不是有限值: %v", what, mean)
	}
	// 先除最大绝对偏差再平方求和。直接 `variance += d*d` 在 d 约 1e200 时溢出成 Inf，
	// scale 随之成 Inf，整个向量被除成零——融合结果仍然有限，后置的有限性检查捕获不到，
	// 于是这一支的判据静默消失、归因照样给出高置信结论。口径与 kongFPNormalize 一致。
	maxAbs := 0.0
	for _, v := range values {
		if d := math.Abs(v - mean); d > maxAbs {
			maxAbs = d
		}
	}
	scale := 0.0
	if maxAbs > 0 {
		sum := 0.0
		for _, v := range values {
			d := (v - mean) / maxAbs
			sum += d * d
		}
		scale = maxAbs * math.Sqrt(sum/float64(len(values)))
		if math.IsNaN(scale) || math.IsInf(scale, 0) {
			return nil, fmt.Errorf("%s 的标准差不是有限值: %v", what, scale)
		}
	}
	// 近零方差的下限保留官方口径：常量向量在那边同样是除以 1e-12。
	if scale < 1e-12 {
		scale = 1e-12
	}
	out := make([]float64, len(values))
	for i, v := range values {
		out[i] = (v - mean) / scale
	}
	if err := kongFPRequireFinite(out, what+"（标准化后）"); err != nil {
		return nil, err
	}
	return out, nil
}

// kongFPHellingerFeature = sqrt((counts + alpha) / sum(counts + alpha))
func kongFPHellingerFeature(counts []float64) []float64 {
	out := make([]float64, len(counts))
	total := 0.0
	for i, c := range counts {
		out[i] = c + kongFPAlpha
		total += out[i]
	}
	for i := range out {
		out[i] = math.Sqrt(out[i] / total)
	}
	return out
}

// kongFPSplitIndices 复刻 numpy.array_split(arr, sections) 的切分点：
// 前 n%sections 段长度为 ceil(n/sections)，其余为 floor(n/sections)。
func kongFPSplitIndices(n, sections int) [][2]int {
	out := make([][2]int, 0, sections)
	base := n / sections
	remainder := n % sections
	start := 0
	for i := 0; i < sections; i++ {
		size := base
		if i < remainder {
			size++
		}
		out = append(out, [2]int{start, start + size})
		start += size
	}
	return out
}

// kongFPHistogram 复刻 numpy.histogram(chunk, bins, range=(lo, hi))：等宽分桶，
// 最后一个桶右闭（值恰好等于 hi 时计入最后一个桶）。
func kongFPHistogram(values []int, bins int, lo, hi float64) []float64 {
	counts := make([]float64, bins)
	if hi <= lo {
		return counts
	}
	width := (hi - lo) / float64(bins)
	for _, raw := range values {
		v := float64(raw)
		if v < lo || v > hi {
			continue
		}
		idx := int((v - lo) / width)
		if idx >= bins { // v == hi
			idx = bins - 1
		}
		counts[idx]++
	}
	return counts
}

// kongFPSqrtNormalize 对平滑后的计数做 sqrt(p) 归一，写回原切片。
func kongFPSqrtNormalize(counts []float64) {
	total := 0.0
	for _, c := range counts {
		total += c
	}
	if total <= 0 {
		return
	}
	for i := range counts {
		counts[i] = math.Sqrt(counts[i] / total)
	}
}

// kongFPOrderedBlockFeature 是 74 维的有序块特征：把序列等分 4 段各取 16 桶直方图，
// 再接上末位数字的 10 桶分布，各自平滑 0.5 后做 sqrt 归一。
//
// 它与 Hellinger 特征的区别在于**保留了顺序信息**——Hellinger 只看整体分布。
func kongFPOrderedBlockFeature(numbers []int) []float64 {
	feature := make([]float64, 0, kongFPBlockDim)
	for _, span := range kongFPSplitIndices(len(numbers), kongFPBlockSegments) {
		hist := kongFPHistogram(numbers[span[0]:span[1]], kongFPBlockBins, 1.0, 356.0)
		for i := range hist {
			hist[i] += 0.5
		}
		kongFPSqrtNormalize(hist)
		feature = append(feature, hist...)
	}
	lastDigits := make([]float64, kongFPLastDigitBins)
	for _, n := range numbers {
		d := n % kongFPLastDigitBins
		if d < 0 {
			d += kongFPLastDigitBins
		}
		lastDigits[d]++
	}
	for i := range lastDigits {
		lastDigits[i] += 0.5
	}
	kongFPSqrtNormalize(lastDigits)
	return append(feature, lastDigits...)
}

// ---- 线性代数辅助（维度都在几百以内，手写即可，不引入新依赖） ----

func kongFPSubScaled(feature, mean, scale []float64) ([]float64, error) {
	if len(feature) != len(mean) || len(feature) != len(scale) {
		return nil, fmt.Errorf("特征维度 %d 与校准资料 (%d, %d) 不匹配", len(feature), len(mean), len(scale))
	}
	out := make([]float64, len(feature))
	for i := range feature {
		// scale 非法时报错，不替换成一个兜底值：任何替换值都没有校准依据，却会让一份损坏的
		// 资料算出「看起来正常」的巨大标准分。有限性与正性在加载时已经校验（见 bank 加载器），
		// 这里只兜住漏网的情况。
		if scale[i] <= 0 || math.IsInf(scale[i], 0) || math.IsNaN(scale[i]) {
			return nil, fmt.Errorf("校准资料第 %d 维的 scale 非法: %v", i, scale[i])
		}
		out[i] = (feature[i] - mean[i]) / scale[i]
		if math.IsNaN(out[i]) || math.IsInf(out[i], 0) {
			return nil, fmt.Errorf("第 %d 维标准化结果非有限值", i)
		}
	}
	return out, nil
}

// kongFPProjectOutBasis 从向量里投影掉 basis 张成的子空间（basis 的行须已正交归一）。
// 这一步去掉的是建库时估计出的共享环境干扰方向。
//
// 维度不符时返回错误而不是跳过那一行：跳过等于悄悄少去掉一个干扰方向，算出来的分数看着完全
// 正常，但已经不是这套算法定义的分数了。维度在加载时就该被挡住，这里是第二道。
func kongFPProjectOutBasis(vec []float64, basis [][]float64) error {
	for i, row := range basis {
		if len(row) != len(vec) {
			return fmt.Errorf("干扰基第 %d 行维度为 %d，应为 %d", i, len(row), len(vec))
		}
		dot := 0.0
		for i := range vec {
			dot += vec[i] * row[i]
		}
		for i := range vec {
			vec[i] -= dot * row[i]
		}
	}
	return nil
}

// kongFPNormalize 把向量归一化到单位长度。
//
// 范数按「先除以最大绝对值再平方求和」算，不是直接累加平方：后者在向量元素很大时（合法但极小的
// scale 会把标准分放大到 1e200 量级）会溢出成 Inf，接着每个元素被除成 0——整条分支的分数静默变成
// 全零，归因照样给出高置信结论，只是少了 0.75 或 0.25 的判据，一处都不报错。
//
// `norm < 1e-12` 的下限保留官方口径（近零向量不做完全归一化），只是改了求法。
func kongFPNormalize(vec []float64) error {
	maxAbs := 0.0
	for _, v := range vec {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("归一化输入含非有限值: %v", v)
		}
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 {
		// 全零向量：除以下限后仍是全零，直接返回，结果与官方口径一致。
		return nil
	}
	sum := 0.0
	for _, v := range vec {
		scaled := v / maxAbs
		sum += scaled * scaled
	}
	norm := math.Sqrt(sum) * maxAbs
	if math.IsNaN(norm) || math.IsInf(norm, 0) {
		return fmt.Errorf("范数不是有限值: %v", norm)
	}
	if norm < 1e-12 {
		norm = 1e-12
	}
	for i := range vec {
		vec[i] /= norm
		if math.IsNaN(vec[i]) || math.IsInf(vec[i], 0) {
			return fmt.Errorf("归一化结果第 %d 维不是有限值", i)
		}
	}
	return nil
}

// kongFPMatVec 计算 centroids · vec，返回每个模型一个分数。
func kongFPMatVec(centroids [][]float64, vec []float64) ([]float64, error) {
	out := make([]float64, len(centroids))
	for i, row := range centroids {
		if len(row) != len(vec) {
			return nil, fmt.Errorf("中心向量维度 %d 与特征维度 %d 不匹配", len(row), len(vec))
		}
		dot := 0.0
		for j := range vec {
			dot += row[j] * vec[j]
		}
		out[i] = dot
	}
	return out, nil
}

func kongFPSoftmax(values []float64) []float64 {
	if len(values) == 0 {
		return nil
	}
	maximum := values[0]
	for _, v := range values[1:] {
		if v > maximum {
			maximum = v
		}
	}
	weights := make([]float64, len(values))
	total := 0.0
	for i, v := range values {
		weights[i] = math.Exp(v - maximum)
		total += weights[i]
	}
	for i := range weights {
		weights[i] /= total
	}
	return weights
}

// ---- 打分 ----

// kongFPHellingerScores 走「去环境方向后的 Hellinger 中心相似度」分支。
func kongFPHellingerScores(counts []float64, bank *KongFingerprintBank) ([]float64, error) {
	h := bank.Robust.Hellinger
	feature := kongFPHellingerFeature(counts)
	projected, err := kongFPSubScaled(feature, h.FeatureMean, h.FeatureScale)
	if err != nil {
		return nil, fmt.Errorf("hellinger: %w", err)
	}
	if err := kongFPProjectOutBasis(projected, h.NuisanceBasis); err != nil {
		return nil, fmt.Errorf("hellinger 去噪: %w", err)
	}
	if err := kongFPNormalize(projected); err != nil {
		return nil, fmt.Errorf("hellinger 归一化: %w", err)
	}
	scores, err := kongFPMatVec(h.Centroids, projected)
	if err != nil {
		return nil, fmt.Errorf("hellinger: %w", err)
	}
	// 官方口径在这一支上**连续标准化两次**（fingerprint.py 的 nuisance → fused 两步）。
	//
	// 看着像恒等操作——第一次之后标准差就是 1 了——但 kongFPStandardize 有 `scale < 1e-12` 的下限：
	// 近零方差时第一次除的是 1e-12，得到一组极大值，第二次才把它们归回正常量级。省掉第二次会在
	// 那种资料下给出完全不同的冠军（实测把 Hellinger 中心乘 1e-15 后，官方判 astra/0.9998，
	// 少一次标准化则判 gpt-5.5/0.2916，且不报错）。
	nuisance, err := kongFPStandardize(scores, "hellinger 分数")
	if err != nil {
		return nil, err
	}
	return kongFPStandardize(nuisance, "hellinger 融合分数")
}

// kongFPOrderedBlockScores 走有序块分支：环境模板相似度与去环境方向后的中心相似度各占一半。
func kongFPOrderedBlockScores(numbers []int, bank *KongFingerprintBank) ([]float64, error) {
	ob := bank.Robust.OrderedBlocks
	feature := kongFPOrderedBlockFeature(numbers)
	standardized, err := kongFPSubScaled(feature, ob.FeatureMean, ob.FeatureScale)
	if err != nil {
		return nil, fmt.Errorf("ordered blocks: %w", err)
	}

	// 模板分支：对每组环境中心各算一次相似度，逐模型取最大值。
	normalized := make([]float64, len(standardized))
	copy(normalized, standardized)
	if err := kongFPNormalize(normalized); err != nil {
		return nil, fmt.Errorf("有序块特征归一化: %w", err)
	}
	var template []float64
	for _, envCentroids := range ob.EnvironmentCentroids {
		scores, err := kongFPMatVec(envCentroids, normalized)
		if err != nil {
			return nil, fmt.Errorf("ordered blocks environment: %w", err)
		}
		if template == nil {
			template = scores
			continue
		}
		for i := range scores {
			if scores[i] > template[i] {
				template[i] = scores[i]
			}
		}
	}
	if template == nil {
		return nil, fmt.Errorf("ordered blocks: 校准资料缺少 environment_centroids")
	}
	if template, err = kongFPStandardize(template, "有序块模板分数"); err != nil {
		return nil, err
	}

	// 干扰分支：投影掉环境方向后再比中心。
	projected := make([]float64, len(standardized))
	copy(projected, standardized)
	if err := kongFPProjectOutBasis(projected, ob.NuisanceBasis); err != nil {
		return nil, fmt.Errorf("有序块去噪: %w", err)
	}
	if err := kongFPNormalize(projected); err != nil {
		return nil, fmt.Errorf("有序块归一化: %w", err)
	}
	nuisance, err := kongFPMatVec(ob.Centroids, projected)
	if err != nil {
		return nil, fmt.Errorf("ordered blocks: %w", err)
	}
	if nuisance, err = kongFPStandardize(nuisance, "有序块干扰分数"); err != nil {
		return nil, err
	}

	fused := make([]float64, len(template))
	for i := range fused {
		fused[i] = 0.5*template[i] + 0.5*nuisance[i]
	}
	return kongFPStandardize(fused, "有序块融合分数")
}

// kongFPScoreOne 给一份回答打分，返回每个模型一个融合后的 z-score。
// kongFPRequireFinite 检查一组数值全部有限。
//
// NaN 会让后面每个比较都为假——「概率 >= 置信度」恒不成立，表现成永远拿不到合格票，而没有任何
// 一步报错。所以要在产出结论之前把它挡住。
func kongFPRequireFinite(values []float64, what string) error {
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s 第 %d 项不是有限值: %v", what, i, v)
		}
	}
	return nil
}

func kongFPScoreOne(numbers []int, bank *KongFingerprintBank) ([]float64, error) {
	marginal, err := kongFPHellingerScores(kongFPCountNumbers(numbers), bank)
	if err != nil {
		return nil, err
	}
	weight := bank.Robust.OrderedBlocks.Weight
	if weight == 0 {
		return marginal, nil
	}
	ordered, err := kongFPOrderedBlockScores(numbers, bank)
	if err != nil {
		return nil, err
	}
	fused := make([]float64, len(marginal))
	for i := range fused {
		fused[i] = (1.0-weight)*marginal[i] + weight*ordered[i]
	}
	if err := kongFPRequireFinite(fused, "融合分数"); err != nil {
		return nil, err
	}
	return fused, nil
}

// KongFingerprintAnswer 是送去归因的一份回答。
type KongFingerprintAnswer struct {
	Text string
	// ExpectedCount 是该条挑战要求的数字个数，用于判定回答是否被截断。0 表示未声明。
	ExpectedCount int
}

// KongFingerprintCandidate 是某个模型的归因结果。
type KongFingerprintCandidate struct {
	Model       string
	DisplayName string
	Probability float64
	Score       float64
}

// KongFingerprintPart 记录一份回答的处置，作废的也要留下——作废率本身是信号。
type KongFingerprintPart struct {
	Index          int
	ParsedNumbers  int
	MinimumNumbers int
	Accepted       bool
	InvalidReason  string
	Numbers        []int
	Scores         []float64
}

// KongFingerprintResult 是一次归因的完整结果。
type KongFingerprintResult struct {
	Prediction      string
	Probability     float64
	UsedAnswers     int
	CalibrationTier string
	Beta            float64
	Candidates      []KongFingerprintCandidate
	Parts           []KongFingerprintPart
}

// KongFingerprintAttribute 对一组回答做归因。
//
// 每份回答分别评分后在分数层面平均，再用与**有效份数**对应的校准温度做一次全局 softmax。
// 份数按置信度递增是调用方的事（达阈值即停），这里只处理传进来的这一批。
// KongFingerprintPartPrediction 返回单份回答**自己**最像哪个模型。
//
// 它与 KongFingerprintResult.Prediction 不是一回事：后者是到该份为止的累计结论。探测记录要存
// 的是前者——用累计值冒充单份归因，事后就再也分不出是哪一份把结论带偏的。
func KongFingerprintPartPrediction(part KongFingerprintPart, bank *KongFingerprintBank) (string, bool) {
	if bank == nil || !part.Accepted || len(part.Scores) == 0 {
		return "", false
	}
	order := bank.Robust.ModelOrder
	if len(part.Scores) != len(order) {
		return "", false
	}
	best := 0
	for i, v := range part.Scores {
		if v > part.Scores[best] {
			best = i
		}
	}
	return order[best], true
}

func KongFingerprintAttribute(answers []KongFingerprintAnswer, bank *KongFingerprintBank) (*KongFingerprintResult, error) {
	if bank == nil {
		return nil, fmt.Errorf("校准资料未加载")
	}
	modelCount := len(bank.Robust.ModelOrder)
	if modelCount == 0 {
		return nil, fmt.Errorf("校准资料缺少 model_order")
	}

	result := &KongFingerprintResult{}
	var valid [][]float64

	for i, answer := range answers {
		numbers := kongFPParseNumbers(answer.Text)
		minimum := kongFPMinimumNumbers(answer.ExpectedCount)
		part := KongFingerprintPart{
			Index:          i,
			ParsedNumbers:  len(numbers),
			MinimumNumbers: minimum,
			Numbers:        numbers,
		}
		// 非 ASCII 十进制数字整份作废：它的序列与官方口径必然不同（见 kongFPDigitRe 的注释），
		// 用它打分等于拿一份错序列去比中心向量。
		if kongFPHasNonASCIIDigit(answer.Text) {
			part.InvalidReason = KongProbeInvalidNonASCIIDigits
			result.Parts = append(result.Parts, part)
			continue
		}
		if len(numbers) < minimum {
			// 拒答或严重截断。留下记录但不计入平均。
			part.InvalidReason = KongProbeInsufficientDigits
			result.Parts = append(result.Parts, part)
			continue
		}
		scores, err := kongFPScoreOne(numbers, bank)
		if err != nil {
			// 打分失败（资料损坏、数值溢出）只作废这一份：数字序列仍然留在 Parts 里，
			// 是这次观测的证据。整体报错会把已解析的其它份也一起丢掉。
			part.InvalidReason = KongProbeInvalidScoreFailed
			result.Parts = append(result.Parts, part)
			continue
		}
		if len(scores) != modelCount {
			part.InvalidReason = KongProbeInvalidScoreFailed
			result.Parts = append(result.Parts, part)
			continue
		}
		part.Accepted = true
		part.Scores = scores
		result.Parts = append(result.Parts, part)
		valid = append(valid, scores)
	}

	if len(valid) == 0 {
		// 连同已解析的 Parts 一起返回：那里面是每份回答的原始数字序列，是这次观测唯一的证据。
		// 只返回错误会让「全部拒答或严重截断」这一类彻底无迹可查，而它恰恰是最该留样的一类。
		return result, fmt.Errorf("没有可用回答：全部拒答或严重截断")
	}

	combined := make([]float64, modelCount)
	for _, scores := range valid {
		for i, s := range scores {
			combined[i] += s
		}
	}
	for i := range combined {
		combined[i] /= float64(len(valid))
	}

	tier := len(valid)
	if tier > 3 {
		tier = 3
	}
	tierKey := fmt.Sprintf("%d", tier)
	calibration, ok := bank.Calibration[tierKey]
	if !ok {
		return nil, fmt.Errorf("校准资料缺少档 %q", tierKey)
	}

	scaled := make([]float64, modelCount)
	for i, v := range combined {
		scaled[i] = calibration.Beta * v
	}
	// beta 是有限正数也可能把分数乘到溢出（1e308 × 分数）。溢出后 softmax 给出 NaN，而 NaN 参与
	// 任何比较都为假——「概率 >= 置信度」恒不成立，表现成永远拿不到合格票且一处不报错。
	if err := kongFPRequireFinite(scaled, "温度缩放后的分数"); err != nil {
		return result, err
	}
	probabilities := kongFPSoftmax(scaled)
	if err := kongFPRequireFinite(probabilities, "softmax 概率"); err != nil {
		return result, err
	}

	candidates := make([]KongFingerprintCandidate, modelCount)
	for i, id := range bank.Robust.ModelOrder {
		candidates[i] = KongFingerprintCandidate{
			Model:       id,
			DisplayName: bank.DisplayName(id),
			Probability: probabilities[i],
			Score:       combined[i],
		}
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		return candidates[a].Probability > candidates[b].Probability
	})

	result.UsedAnswers = len(valid)
	result.CalibrationTier = tierKey
	result.Beta = calibration.Beta
	result.Candidates = candidates
	result.Prediction = candidates[0].Model
	result.Probability = candidates[0].Probability
	return result, nil
}
