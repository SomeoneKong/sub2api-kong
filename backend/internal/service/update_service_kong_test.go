// 这两个函数出错的症状只是"永远不提示新版"，不报错也不崩，所以必须有测试兜着。

package service

import "testing"

func TestParseVersionForkSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want [4]int
	}{
		{"0.2.5", [4]int{0, 2, 5, 0}},           // 无后缀，fork 段为 0
		{"v0.2.5", [4]int{0, 2, 5, 0}},          // 允许 v 前缀
		{"0.2.5-kong.1", [4]int{0, 2, 5, 1}},    // fork 序号
		{"v0.2.5-kong.10", [4]int{0, 2, 5, 10}}, // 两位序号，不是字典序
		{"0.2.5-rc1", [4]int{0, 2, 5, 0}},       // 后缀无 '.'，只比前三段
		{"0.2.5-kong.x", [4]int{0, 2, 5, 0}},    // 后缀非数字，同上
		{"1.2", [4]int{1, 2, 0, 0}},             // 段数不足
		{"", [4]int{0, 0, 0, 0}},                // 空串不 panic
	}
	for _, c := range cases {
		if got := parseVersion(c.in); got != c.want {
			t.Errorf("parseVersion(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCompareVersionsForkSuffix(t *testing.T) {
	cases := []struct {
		current, latest string
		want            int
	}{
		// 同一基线内的 fork 迭代
		{"0.2.5-kong.1", "0.2.5-kong.7", -1},
		{"0.2.5-kong.7", "0.2.5-kong.1", 1},
		{"0.2.5-kong.2", "0.2.5-kong.2", 0},
		// 带 fork 后缀者新于同基线的无后缀版本
		{"0.2.5-kong.1", "0.2.5", 1},
		{"0.2.5", "0.2.5-kong.1", -1},
		// 跨基线时前三段优先，fork 序号不能盖过它
		{"0.2.5-kong.9", "0.2.6", -1},
		{"0.2.6", "0.2.5-kong.9", 1},
		{"0.2.5-kong.9", "0.3.0-kong.1", -1},
		// 纯三段版本的常规比较
		{"0.2.4", "0.2.5", -1},
		{"0.2.5", "0.2.5", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.current, c.latest); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.current, c.latest, got, c.want)
		}
	}
}
