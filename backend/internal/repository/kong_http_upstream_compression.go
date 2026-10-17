package repository

// OpenAI 上游不声明压缩（fork 专有，设计见 DESIGN-openai-request-bytes.md §6）。
//
// 官方 Codex 发 /responses 不带 Accept-Encoding；Go 的 transport 在请求没有这个头时会自动加
// `Accept-Encoding: gzip` 并透明解压，于是网关的请求比官方客户端多出一个可识别的头。chatgpt.com 的 SSE
// 本来就不压缩，去掉它对带宽没有影响。net/http 没有按单个请求关掉自动压缩的办法，只能在 transport 上开
// DisableCompression，所以按 profile 的连接池设置来做：OpenAI profile 的连接池不自动声明压缩。上游主动
// 发来 Content-Encoding 时仍由 decompressResponseBody 解开。

// kongPoolKeySuffix 把"不声明压缩"算进连接池的配置键，以及 TLS 指纹路径的缓存键。
//
// TLS 指纹路径的缓存键固定用默认协议模式、不含 profile：不加这个后缀，同账号同代理交替发 OpenAI 与默认
// profile 的 TLS 请求时，配置键不符会让两边互相删掉对方的客户端重建，而不是两个连接池并存。普通路径的
// 缓存键含协议模式（OpenAI profile 总是 openai_*），本来就分开。
func kongPoolKeySuffix(settings poolSettings) string {
	if settings.kongDisableCompression {
		return "|kong_no_compress"
	}
	return ""
}
