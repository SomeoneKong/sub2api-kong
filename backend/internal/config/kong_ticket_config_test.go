package config

import (
	"testing"

	"github.com/spf13/viper"
)

// 票据配置必须走**完整加载入口**来测，不能只测 applyKongTicketEnvOverrides。
//
// 理由是一次真实的缺陷：批量取票开关注册了默认值，于是 viper 会把环境变量按 `bool` 解码——
// `" false "`（带空格）或 `yes` 这类值会让整个 `viper.Unmarshal` 失败，`main.go` 据此终止启动，
// 哪怕一个账号都没开票据功能。而按值覆盖的容错写在解码之后，永远轮不到。
func TestKongTicketBatchFetchEnvParsing(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		raw  string
		want bool
	}{
		{name: "未设置取内置默认", set: false, want: DefaultKongTicketBatchFetchAllModels},
		{name: "显式 false", set: true, raw: "false", want: false},
		{name: "显式 true", set: true, raw: "true", want: true},
		{name: "带空格的 false 仍然生效", set: true, raw: "  false  ", want: false},
		{name: "0 视为 false", set: true, raw: "0", want: false},
		// 笔误不该拒绝启动，也不该被解读成"关闭"——那个方向是悄悄退回旧行为。
		{name: "非法值回退到默认且不报错", set: true, raw: "yes", want: DefaultKongTicketBatchFetchAllModels},
		{name: "空串回退到默认", set: true, raw: "", want: DefaultKongTicketBatchFetchAllModels},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			t.Setenv("CONFIG_FILE", "")
			t.Setenv("DATA_DIR", "")
			t.Setenv("JWT_SECRET", "")
			if tc.set {
				t.Setenv(KongTicketBatchFetchEnv, tc.raw)
			}

			cfg, err := LoadForBootstrap()
			if err != nil {
				t.Fatalf("配置加载失败（配置笔误不该让网关起不来）: %v", err)
			}
			if got := cfg.Gateway.KongCodexTicket.BatchFetchAllModels; got != tc.want {
				t.Fatalf("batch_fetch_all_models = %v，应为 %v", got, tc.want)
			}
		})
	}
}

// 融合取票开关走同一条规范化路径。单列一条用例是因为两个开关共用 normalizeKongTicketBool 之后，
// "只有其中一个被规范化"这种回归不会被另一条用例发现。
func TestKongTicketFusedFingerprintEnvParsing(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		raw  string
		want bool
	}{
		{name: "未设置取内置默认", set: false, want: DefaultKongTicketFetchFusedFingerprint},
		{name: "显式 false", set: true, raw: "false", want: false},
		{name: "带空格的 false 仍然生效", set: true, raw: " false ", want: false},
		// 与批量取票同一条判断：笔误不该拒绝启动，也不该被解读成"关闭"。
		{name: "非法值回退到默认且不报错", set: true, raw: "maybe", want: DefaultKongTicketFetchFusedFingerprint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			t.Setenv("CONFIG_FILE", "")
			t.Setenv("DATA_DIR", "")
			t.Setenv("JWT_SECRET", "")
			if tc.set {
				t.Setenv(KongTicketFusedFingerprintEnv, tc.raw)
			}

			cfg, err := LoadForBootstrap()
			if err != nil {
				t.Fatalf("配置加载失败（配置笔误不该让网关起不来）: %v", err)
			}
			if got := cfg.Gateway.KongCodexTicket.FetchFusedFingerprint; got != tc.want {
				t.Fatalf("fetch_fused_fingerprint = %v，应为 %v", got, tc.want)
			}
		})
	}
}

// 接受白名单的空串语义：viper 的 AutomaticEnv 忽略空环境变量，所以"显式设空"必须靠单字段覆盖
// 兑现——否则它会被当成未设置、仍取含 gpt-5.5 的默认值，而那个方向是**放宽**判据。
func TestKongTicketAcceptExtraEnvParsing(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  bool
		raw  string
		want string
	}{
		{name: "未设置取内置默认", set: false, want: DefaultKongTicketAcceptExtra},
		{name: "显式空串清空白名单", set: true, raw: "", want: ""},
		{name: "显式取值", set: true, raw: "gpt-6-astra:gpt-6-astra", want: "gpt-6-astra:gpt-6-astra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			t.Setenv("CONFIG_FILE", "")
			t.Setenv("DATA_DIR", "")
			t.Setenv("JWT_SECRET", "")
			if tc.set {
				t.Setenv(KongTicketAcceptExtraEnv, tc.raw)
			}

			cfg, err := LoadForBootstrap()
			if err != nil {
				t.Fatalf("配置加载失败: %v", err)
			}
			if got := cfg.Gateway.KongCodexTicket.AcceptExtra; got != tc.want {
				t.Fatalf("accept_extra = %q，应为 %q", got, tc.want)
			}
		})
	}
}
