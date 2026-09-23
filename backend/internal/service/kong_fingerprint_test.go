package service

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// 归因算法是从 ModelTrace 的 Python 实现移植过来的，而移植偏差不会报错、不会崩——它只会把
// astra 判成别的，然后把好票全拒掉，表象是「一直拿不到合格票」。所以必须有 golden file 锁住：
// testdata 里每条样本都带 Python 侧算出的解析个数与归因结果，Go 侧必须逐条复现。

type kongFPGoldenRow struct {
	ChallengeID   string  `json:"challenge_id"`
	ExpectedCount int     `json:"expected_count"`
	Text          string  `json:"text"`
	Parsed        int     `json:"parsed"`
	Best          string  `json:"best"`
	BestP         float64 `json:"best_p"`
	Top           [][]any `json:"top"`
	ServedModel   string  `json:"served_model"`
}

func kongFPLoadGolden(t *testing.T) []kongFPGoldenRow {
	t.Helper()
	f, err := os.Open("testdata/kong_fingerprint_golden.jsonl")
	if err != nil {
		t.Fatalf("打开 golden file: %v", err)
	}
	defer func() { _ = f.Close() }()

	var rows []kongFPGoldenRow
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row kongFPGoldenRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("解析 golden 行: %v", err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取 golden file: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("golden file 为空")
	}
	return rows
}

// 校准资料的维度校验是算法正确性的前提：维度对不上时打分会算出一个「看起来正常」的结果，
// 而不是报错。
func TestKongFingerprintBankLoads(t *testing.T) {
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载内置校准资料: %v", err)
	}
	if len(bank.Robust.ModelOrder) == 0 {
		t.Fatal("model_order 为空")
	}
	if !bank.HasModel("gpt-6-astra") {
		t.Error("校准资料里没有 gpt-6-astra——准入判定依赖它在库中，否则「判为 astra」不成立")
	}
	if bank.Robust.OrderedBlocks.Weight <= 0 || bank.Robust.OrderedBlocks.Weight >= 1 {
		t.Errorf("有序块权重 %v 超出 (0,1)", bank.Robust.OrderedBlocks.Weight)
	}
	version := bank.Version()
	if version["digest"] == "" || version["built_at"] == "" {
		t.Errorf("资料版本标识不完整: %v", version)
	}
}

// 数字解析必须与 Python 侧逐条一致。它错了，后面所有特征都错，而且错得很隐蔽。
func TestKongFingerprintParseMatchesGolden(t *testing.T) {
	for _, row := range kongFPLoadGolden(t) {
		numbers := kongFPParseNumbers(row.Text)
		if len(numbers) != row.Parsed {
			t.Errorf("%s: 解析到 %d 个数字，Python 侧是 %d", row.ChallengeID, len(numbers), row.Parsed)
		}
		for _, n := range numbers {
			if n < kongFPValueMin || n > kongFPValueMax {
				t.Fatalf("%s: 解析出越界值 %d", row.ChallengeID, n)
			}
		}
	}
}

// 归因结果必须与 Python 侧一致：预测模型完全相同，概率在浮点容差内相同。
// golden 里的概率保留 4 位小数，所以容差取 2e-4（舍入误差 5e-5 + 计算误差）。
func TestKongFingerprintAttributeMatchesGolden(t *testing.T) {
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	const tolerance = 2e-4

	for _, row := range kongFPLoadGolden(t) {
		result, err := KongFingerprintAttribute([]KongFingerprintAnswer{
			{Text: row.Text, ExpectedCount: row.ExpectedCount},
		}, bank)
		if err != nil {
			t.Fatalf("%s: 归因失败: %v", row.ChallengeID, err)
		}
		if result.Prediction != row.Best {
			t.Errorf("%s: 预测 %q，Python 侧是 %q", row.ChallengeID, result.Prediction, row.Best)
			continue
		}
		if diff := math.Abs(result.Probability - row.BestP); diff > tolerance {
			t.Errorf("%s: 概率 %.6f，Python 侧是 %.4f，差 %.6f", row.ChallengeID, result.Probability, row.BestP, diff)
		}
		if result.CalibrationTier != "1" {
			t.Errorf("%s: 单份回答应当用档 \"1\"，实际 %q", row.ChallengeID, result.CalibrationTier)
		}
		if result.UsedAnswers != 1 {
			t.Errorf("%s: 有效份数应为 1，实际 %d", row.ChallengeID, result.UsedAnswers)
		}

		// top-3 的顺序与概率也要对上——只比对第一名会放过「第二三名算错」这类偏差。
		for i, entry := range row.Top {
			if i >= len(result.Candidates) {
				break
			}
			wantModel, _ := entry[0].(string)
			wantP, _ := entry[1].(float64)
			got := result.Candidates[i]
			if got.Model != wantModel {
				t.Errorf("%s: 第 %d 名是 %q，Python 侧是 %q", row.ChallengeID, i+1, got.Model, wantModel)
			}
			if diff := math.Abs(got.Probability - wantP); diff > tolerance {
				t.Errorf("%s: 第 %d 名概率 %.6f，Python 侧是 %.4f", row.ChallengeID, i+1, got.Probability, wantP)
			}
		}
	}
}

// 拒答与严重截断必须被判为无效，而不是拿残缺序列去归因得出一个像样的结论。
func TestKongFingerprintRejectsInsufficientAnswers(t *testing.T) {
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	cases := []struct {
		name          string
		text          string
		expectedCount int
	}{
		{name: "完全拒答", text: "抱歉，我不能完成这个任务。", expectedCount: 300},
		{name: "只有少量数字", text: "1, 2, 3, 4, 5", expectedCount: 300},
		{name: "严重截断：不足期望值的 55%", text: kongFPRepeatNumbers(100), expectedCount: 300},
		{name: "未声明期望长度时仍需满 80 个", text: kongFPRepeatNumbers(79), expectedCount: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := KongFingerprintAttribute([]KongFingerprintAnswer{
				{Text: c.text, ExpectedCount: c.expectedCount},
			}, bank)
			if err == nil {
				t.Error("残缺回答被接受了——这会让一次拒答变成一个看起来正常的档位结论")
			}
		})
	}

	// 边界：恰好满 80 个且未声明期望长度时应当被接受。
	result, err := KongFingerprintAttribute([]KongFingerprintAnswer{
		{Text: kongFPRepeatNumbers(80), ExpectedCount: 0},
	}, bank)
	if err != nil {
		t.Fatalf("恰好 80 个数字应当被接受: %v", err)
	}
	if result.UsedAnswers != 1 {
		t.Errorf("有效份数 = %d，want 1", result.UsedAnswers)
	}
}

func kongFPRepeatNumbers(n int) string {
	out := make([]byte, 0, n*4)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',', ' ')
		}
		v := i%kongFPValueMax + 1
		out = append(out, []byte(itoaKongFP(v))...)
	}
	return string(out)
}

func itoaKongFP(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// 份数档必须按**有效**份数选，不是按传入份数：平均会压低分数方差，温度要跟着变。
// 用档 "1" 去解释两份的平均分，概率标定就是错的。
func TestKongFingerprintCalibrationTierUsesValidCount(t *testing.T) {
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	rows := kongFPLoadGolden(t)
	if len(rows) < 2 {
		t.Skip("golden 样本不足两条")
	}

	// 两份有效 + 一份拒答 → 档仍应是 "2"
	result, err := KongFingerprintAttribute([]KongFingerprintAnswer{
		{Text: rows[0].Text, ExpectedCount: rows[0].ExpectedCount},
		{Text: "我无法完成这个任务。", ExpectedCount: 300},
		{Text: rows[1].Text, ExpectedCount: rows[1].ExpectedCount},
	}, bank)
	if err != nil {
		t.Fatalf("归因失败: %v", err)
	}
	if result.UsedAnswers != 2 {
		t.Errorf("有效份数 = %d，want 2", result.UsedAnswers)
	}
	if result.CalibrationTier != "2" {
		t.Errorf("校准档 = %q，want \"2\"", result.CalibrationTier)
	}
	if result.Beta != bank.Calibration["2"].Beta {
		t.Errorf("beta = %v，want %v", result.Beta, bank.Calibration["2"].Beta)
	}
	// 作废的那份也要留下记录：作废率本身是信号。
	if len(result.Parts) != 3 {
		t.Fatalf("应当为 3 份回答各留一条记录，实际 %d", len(result.Parts))
	}
	if result.Parts[1].Accepted || result.Parts[1].InvalidReason == "" {
		t.Error("拒答的那份应当被标记为无效并带上原因")
	}
}

// 非 ASCII 十进制数字必须让该份整体作废，不能被悄悄拆成别的整数。
//
// Go 的 `\d` 只认 ASCII，官方 Python 实现认整个 Unicode Nd。按 ASCII 口径解析时，全角数字会被
// 当成分隔符，`1１3` 变成 `1` 和 `3` 两个整数而 Python 得到一个 `113`——序列长度与取值全变，
// 归因照样给出高置信结论。这里锁住的是「宁可不用这一份」的取舍。
func TestKongFingerprintRejectsNonASCIIDigits(t *testing.T) {
	if !kongFPHasNonASCIIDigit("1２3") {
		t.Fatal("全角数字应当被识别出来")
	}
	if kongFPHasNonASCIIDigit("[1, 2, 3]") {
		t.Error("纯 ASCII 数字不该被判为非 ASCII")
	}

	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("加载校准资料: %v", err)
	}
	// 一份足量但含全角数字的回答：必须作废，而不是算出一个结论。
	numbers := make([]string, 0, 260)
	for i := 0; i < 260; i++ {
		numbers = append(numbers, fmt.Sprintf("%d", (i*37)%355+1))
	}
	text := "[" + strings.Join(numbers, ",") + ",１２３]"

	result, err := KongFingerprintAttribute([]KongFingerprintAnswer{{Text: text, ExpectedCount: 218}}, bank)
	if err == nil {
		t.Error("含全角数字的唯一一份回答不该产出结论")
	}
	if result == nil || len(result.Parts) != 1 {
		t.Fatalf("应当留下一份观测记录，得到 %+v", result)
	}
	if result.Parts[0].InvalidReason != KongProbeInvalidNonASCIIDigits {
		t.Errorf("作废原因 = %q, want %q", result.Parts[0].InvalidReason, KongProbeInvalidNonASCIIDigits)
	}
}
