//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// 校准资料里出现 null 必须拒绝加载。
//
// json.Unmarshal 把数值数组里的 null 解成 0，而 0 是合法的中心分量——维度校验与有限性校验都看不出
// 区别。一个缺失值被当成 0 参与打分，足以把归因从一个模型翻到另一个。
func TestKongParseFingerprintBankRejectsNull(t *testing.T) {
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("内置资料应当加载成功: %v", err)
	}
	if bank == nil {
		t.Fatal("内置资料为空")
	}

	// 从内置资料出发，只把一个中心分量换成 null。
	raw, err := json.Marshal(bank)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("反序列化: %v", err)
	}
	robust, _ := doc["robust"].(map[string]any)
	hellinger, _ := robust["hellinger"].(map[string]any)
	centroids, _ := hellinger["centroids"].([]any)
	if len(centroids) == 0 {
		t.Fatal("内置资料缺少 hellinger 中心")
	}
	firstRow, _ := centroids[0].([]any)
	if len(firstRow) == 0 {
		t.Fatal("中心行为空")
	}
	firstRow[0] = nil
	patched, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("重新序列化: %v", err)
	}
	if _, err := kongParseFingerprintBank(patched, "测试"); err == nil {
		t.Error("含 null 的校准资料必须拒绝加载")
	} else if !strings.Contains(err.Error(), "null") {
		t.Errorf("错误信息应当指出 null，实际: %v", err)
	}

	// 不参与打分的字段允许 null：官方实现对同一份资料照常加载、归因结果一字不差，整份拒绝
	// 会把官方能用的资料判成不可加载（指纹测试随之整体不可用）。
	firstRow[0] = 0.0
	calibration, _ := doc["calibration"].(map[string]any)
	for _, v := range calibration {
		tier, ok := v.(map[string]any)
		if !ok {
			continue
		}
		tier["cv_accuracy"] = nil
	}
	if len(calibration) == 0 {
		t.Fatal("内置资料缺少 calibration")
	}
	tolerated, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("重新序列化: %v", err)
	}
	if _, err := kongParseFingerprintBank(tolerated, "测试"); err != nil {
		t.Errorf("不参与打分的字段为 null 不该拒绝加载: %v", err)
	}
	// 大小写别名同样要挡住：encoding/json 匹配字段名不区分大小写，`"CENTROIDS"` 照样会被解进
	// Centroids——按 gjson 路径扫只认精确小写，那个键就整段漏检。
	hellinger["CENTROIDS"] = hellinger["centroids"]
	delete(hellinger, "centroids")
	aliasRow, _ := hellinger["CENTROIDS"].([]any)
	aliasFirst, _ := aliasRow[0].([]any)
	aliasFirst[0] = nil
	aliased, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("重新序列化: %v", err)
	}
	if _, err := kongParseFingerprintBank(aliased, "测试"); err == nil {
		t.Error("大小写别名下的 null 必须拒绝加载")
	}
	aliasFirst[0] = 0.0
	hellinger["centroids"] = hellinger["CENTROIDS"]
	delete(hellinger, "CENTROIDS")

	// 温度参与打分，它的 null 必须拒绝。
	for _, v := range calibration {
		tier, ok := v.(map[string]any)
		if !ok {
			continue
		}
		tier["beta"] = nil
	}
	betaNull, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("重新序列化: %v", err)
	}
	if _, err := kongParseFingerprintBank(betaNull, "测试"); err == nil {
		t.Error("beta 为 null 必须拒绝加载")
	}
	for _, v := range calibration {
		if tier, ok := v.(map[string]any); ok {
			tier["beta"] = 1.0
			delete(tier, "cv_accuracy")
		}
	}
	zeroed, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("重新序列化: %v", err)
	}
	if _, err := kongParseFingerprintBank(zeroed, "测试"); err != nil {
		t.Errorf("合法的数值 0 不该被拒绝: %v", err)
	}
}

// models 与 robust.model_order 必须逐项一致。
//
// 归因结果按 model_order 标注（中心矩阵的行序就是它），官方 fingerprint.py 按 models 标注。两份
// 序列不一致时双方对同一个分数向量给出不同的模型名——只把两个标签对调，同一份资料同一个样本就会
// 把一个模型的分数报成另一个，而 HasModel 照样通过。
func TestKongParseFingerprintBankRequiresModelOrderMatch(t *testing.T) {
	bank, err := KongFingerprintBankLoad()
	if err != nil {
		t.Fatalf("内置资料应当加载成功: %v", err)
	}
	raw, err := json.Marshal(bank)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("反序列化: %v", err)
	}
	robust, _ := doc["robust"].(map[string]any)
	order, _ := robust["model_order"].([]any)
	if len(order) < 2 {
		t.Skip("内置资料只有一个模型，对调无意义")
	}

	// 1) 对调 model_order 的两项
	swapped := append([]any(nil), order...)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	robust["model_order"] = swapped
	patched, _ := json.Marshal(doc)
	if _, err := kongParseFingerprintBank(patched, "测试"); err == nil {
		t.Error("model_order 与 models 顺序不一致必须拒绝加载")
	}

	// 2) 重复 id
	dup := append([]any(nil), order...)
	dup[1] = dup[0]
	robust["model_order"] = dup
	patched, _ = json.Marshal(doc)
	if _, err := kongParseFingerprintBank(patched, "测试"); err == nil {
		t.Error("model_order 含重复 id 必须拒绝加载")
	}

	// 3) 数量不符
	robust["model_order"] = order[:len(order)-1]
	patched, _ = json.Marshal(doc)
	if _, err := kongParseFingerprintBank(patched, "测试"); err == nil {
		t.Error("model_order 与 models 数量不符必须拒绝加载")
	}

	// 4) 还原后仍能加载
	robust["model_order"] = order
	patched, _ = json.Marshal(doc)
	if _, err := kongParseFingerprintBank(patched, "测试"); err != nil {
		t.Errorf("还原后应当加载成功: %v", err)
	}
}
