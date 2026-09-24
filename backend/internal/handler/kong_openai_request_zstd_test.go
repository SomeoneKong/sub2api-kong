package handler

import (
	"os"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 本包测试不压缩出站请求体：上游的转发测试记下发往 chatgpt.com 的正文后按明文 JSON 断言，压缩由
// service 包的测试覆盖。开关在首次使用时才读取，init 里设好即对全部测试生效；本文件不带 build tag，
// 两种口径都生效。
func init() {
	if err := os.Setenv(service.KongOpenAIRequestZstdEnv, "false"); err != nil {
		panic(err)
	}
}
