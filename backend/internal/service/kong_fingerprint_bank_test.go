package service

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// 校准资料的维度错误是移植偏差里最危险的一种：算法照样算得出分数，只是那个分数已经不是这套
// 算法定义的分数了。功能启用后资料失效必须让**启动失败**，所以每一类实际参与计算的矩阵都要
// 在加载时校验到。
//
// 这些用例逐一损坏内置资料的一类字段，断言加载报错。它们同时防住一个反向错误：把校验写得过
// 严会让合法资料加载不进来，所以第一个用例断言原样资料能过。
func TestKongParseFingerprintBankRejectsBrokenDimensions(t *testing.T) {
	raw := kongFingerprintBankJSON

	if _, err := kongParseFingerprintBank(raw, "embedded"); err != nil {
		t.Fatalf("内置资料应当能加载: %v", err)
	}

	cases := []struct {
		name   string
		break_ func(m map[string]any)
	}{
		{
			name: "hellinger 干扰基行宽不对",
			break_: func(m map[string]any) {
				h := kongDigInto(m, "robust", "hellinger")
				h["nuisance_basis"] = []any{[]any{1.0}}
			},
		},
		{
			name: "hellinger 中心行宽不对",
			break_: func(m map[string]any) {
				h := kongDigInto(m, "robust", "hellinger")
				rows, _ := h["centroids"].([]any)
				rows[0] = []any{1.0}
			},
		},
		{
			name: "有序块中心行宽不对",
			break_: func(m map[string]any) {
				ob := kongDigInto(m, "robust", "ordered_blocks")
				rows, _ := ob["centroids"].([]any)
				rows[0] = []any{1.0}
			},
		},
		{
			name: "有序块干扰基行宽不对",
			break_: func(m map[string]any) {
				ob := kongDigInto(m, "robust", "ordered_blocks")
				ob["nuisance_basis"] = []any{[]any{1.0}}
			},
		},
		{
			name: "环境中心行宽不对",
			break_: func(m map[string]any) {
				ob := kongDigInto(m, "robust", "ordered_blocks")
				envs, _ := ob["environment_centroids"].([]any)
				first, _ := envs[0].([]any)
				first[0] = []any{1.0}
			},
		},
		{
			name: "有序块权重超出 [0,1]",
			break_: func(m map[string]any) {
				kongDigInto(m, "robust", "ordered_blocks")["weight"] = 1.5
			},
		},
		{
			// 字段缺失会被解成零值，与「有意关掉这一支」长得一样。带着数据却写 0 必须报错，
			// 否则会静默丢掉 0.25 的判据。
			name: "带有序块数据却把权重写成 0",
			break_: func(m map[string]any) {
				kongDigInto(m, "robust", "ordered_blocks")["weight"] = 0.0
			},
		},
		{
			name: "缺校准档",
			break_: func(m map[string]any) {
				calibration, _ := m["calibration"].(map[string]any)
				delete(calibration, "2")
			},
		},
		{
			name: "校准档 beta 非正",
			break_: func(m map[string]any) {
				kongDigInto(m, "calibration", "1")["beta"] = 0.0
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("解析内置资料: %v", err)
			}
			c.break_(m)
			broken, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("重新编码: %v", err)
			}
			if _, err := kongParseFingerprintBank(broken, "broken"); err == nil {
				t.Error("损坏的资料必须在加载时被拒绝——启用后资料失效要让启动失败，而不是算出一个看起来正常的错结论")
			}
		})
	}
}

func kongDigInto(m map[string]any, path ...string) map[string]any {
	cur := m
	for _, key := range path {
		cur, _ = cur[key].(map[string]any)
	}
	return cur
}

// scale 的数值损坏比维度损坏更危险：维度不符会报错，数值损坏会算出一个「看起来正常」的结论。
func TestKongParseFingerprintBankRejectsBrokenScale(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		// 负 scale 把标准分整体翻向，给出高置信的**错**模型。
		{"负数", -1.0},
		// 零会让除法产出 Inf。
		{"零", 0.0},
		{"null", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(kongFingerprintBankJSON, &m); err != nil {
				t.Fatalf("解析内置资料: %v", err)
			}
			h := kongDigInto(m, "robust", "hellinger")
			scale, _ := h["feature_scale"].([]any)
			for i := range scale {
				scale[i] = c.value
			}
			broken, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("重新编码: %v", err)
			}
			if _, err := kongParseFingerprintBank(broken, "broken"); err == nil {
				t.Error("scale 数值损坏必须在加载时被拒绝")
			}
		})
	}
}

// 极小正值是合法的有限正数，加载时不该被拒——凭空给 scale 设一个下限没有任何校准依据。
// 但它会让标准化溢出，所以**归因不得返回一个含 NaN 的成功结果**：那种结果会让「概率 >= 置信度」
// 恒为假，表现成永远拿不到合格票且一处都不报错。
func TestKongSubnormalScaleFailsLoudlyInsteadOfNaN(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal(kongFingerprintBankJSON, &m); err != nil {
		t.Fatalf("解析内置资料: %v", err)
	}
	scale, _ := kongDigInto(m, "robust", "hellinger")["feature_scale"].([]any)
	for i := range scale {
		scale[i] = 1e-320
	}
	patched, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("重新编码: %v", err)
	}
	bank, err := kongParseFingerprintBank(patched, "subnormal")
	if err != nil {
		t.Fatalf("极小正值不该导致加载失败: %v", err)
	}

	numbers := make([]string, 0, 260)
	for i := 0; i < 260; i++ {
		numbers = append(numbers, fmt.Sprintf("%d", (i*37)%355+1))
	}
	result, err := KongFingerprintAttribute(
		[]KongFingerprintAnswer{{Text: "[" + strings.Join(numbers, ",") + "]", ExpectedCount: 218}}, bank)
	if err == nil {
		t.Fatal("标准化溢出时必须报错，不能给出结论")
	}
	if result != nil && !math.IsNaN(result.Probability) && result.Probability != 0 {
		t.Errorf("不该产出概率 %v", result.Probability)
	}
	if result == nil || len(result.Parts) != 1 || result.Parts[0].InvalidReason != KongProbeInvalidScoreFailed {
		t.Errorf("该份应标为打分失败并保留数字，得到 %+v", result)
	}
}
