package service

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

// OpenAI 请求体快速路径（设计见 DESIGN-openai-request-body-fastpath.md）。
//
// 两件事：在昂贵的请求体处理前加判否门，只在能证明原函数一定原样返回时跳过；让同一份请求体上的顶层查找
// 只扫一遍。两者都不改任何一道处理的改写逻辑，会改写或会报错的请求照常走原代码。

// KongOpenAIBodyFastpathEnv 是总开关：不设置为开，false 关闭，写错按关闭处理并记错误日志。
const KongOpenAIBodyFastpathEnv = "KONG_OPENAI_BODY_FASTPATH"

// KongOpenAIBodyFastpathOffEnv 按名字关闭其中几项，逗号分隔；未知名字记错误日志后忽略。
const KongOpenAIBodyFastpathOffEnv = "KONG_OPENAI_BODY_FASTPATH_OFF"

type kongFastpathItem int

const (
	kongFastpathBootstrap kongFastpathItem = iota
	kongFastpathLite
	kongFastpathOrphan
	kongFastpathTrigger
	kongFastpathAlias
	kongFastpathLookaround
	kongFastpathIndex
	kongFastpathMetadata
	kongFastpathItemIDs
	kongFastpathImage
	kongFastpathItemCount
)

var kongFastpathItemNames = [kongFastpathItemCount]string{
	kongFastpathBootstrap:  "bootstrap",
	kongFastpathLite:       "lite",
	kongFastpathOrphan:     "orphan",
	kongFastpathTrigger:    "trigger",
	kongFastpathAlias:      "alias",
	kongFastpathLookaround: "lookaround",
	kongFastpathIndex:      "index",
	kongFastpathMetadata:   "metadata",
	kongFastpathItemIDs:    "item_ids",
	kongFastpathImage:      "image",
}

type kongFastpathSettings struct {
	enabled [kongFastpathItemCount]bool
}

func kongParseFastpathSettings(value string, present bool, offList string) (kongFastpathSettings, []error) {
	var settings kongFastpathSettings
	var errs []error
	enabled := true
	if present {
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s 必须是 true / false，得到 %q", KongOpenAIBodyFastpathEnv, value))
			parsed = false
		}
		enabled = parsed
	}
	if !enabled {
		return settings, errs
	}
	for i := range settings.enabled {
		settings.enabled[i] = true
	}
	for _, raw := range strings.Split(offList, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		found := false
		for i, known := range kongFastpathItemNames {
			if name == known {
				settings.enabled[i] = false
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("%s 里有未知的名字 %q，已忽略", KongOpenAIBodyFastpathOffEnv, raw))
		}
	}
	return settings, errs
}

var kongFastpathSettingsFromEnv = sync.OnceValue(func() kongFastpathSettings {
	value, present := os.LookupEnv(KongOpenAIBodyFastpathEnv)
	settings, errs := kongParseFastpathSettings(value, present, os.Getenv(KongOpenAIBodyFastpathOffEnv))
	for _, err := range errs {
		slog.Error("kong 请求体快速路径：开关写法有误", "error", err)
	}
	var active []string
	for i, on := range settings.enabled {
		if on {
			active = append(active, kongFastpathItemNames[i])
		}
	}
	slog.Info("kong 请求体快速路径", "active", strings.Join(active, ","))
	return settings
})

// kongFastpathSettingsCurrent 取当前开关；测试替换它来固定开关。
var kongFastpathSettingsCurrent = func() kongFastpathSettings { return kongFastpathSettingsFromEnv() }

func kongFastpathOn(item kongFastpathItem) bool {
	return kongFastpathSettingsCurrent().enabled[item]
}

// KongOpenAIBodyFastpathBootstrapEnabled 供 handler 的 bootstrap 门判断是否开启。
func KongOpenAIBodyFastpathBootstrapEnabled() bool {
	return kongFastpathOn(kongFastpathBootstrap)
}

// kongBytesView 把字节切片当作只读字符串视图，不复制。对它调用 gjson.Get 与 gjson.GetBytes 的结果相同，只是
// 结果里的 Raw 与切片共用内存；依赖"请求体不被原地修改"的不变量。
func kongBytesView(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}
