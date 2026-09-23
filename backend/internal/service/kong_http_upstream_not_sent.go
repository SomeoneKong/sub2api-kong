package service

// 这个类型属于**传输层契约**，却刻意放在本 fork 自己的文件里。
//
// 理由是提交切分：本功能的两个提交按冲突面切开——第一个全是新增的自有文件（永不冲突），第二个只
// 有接入上游既有文件的那几行。把它写进 http_upstream_port.go（上游文件）会让第一个提交引用第二个
// 提交才有的符号，那个提交就不能独立构建了。上游侧只留「在发包前的失败点包上这个标记」那两行。

// HTTPUpstreamNotSentError 表示请求在**真正发包之前**就失败了。
//
// 主机校验不通过、客户端池取不到连接（缓存满且无可淘汰项）、transport 构造失败都属于这一类：
// 一个字节都没出去。调用方据此区分「本地失败」与「发出去了但失败」——票据子系统靠这个区分决定
// 要不要推进出口活动 A，而 A 是按 IP 积累的静默，把本地失败也算进去会让共用同一出口的其它账号
// 凭空多等一整个静默窗口。
//
// 判定必须由传输层给出，不能靠错误字符串猜：那既脆弱，也分不清「拨号后失败」这种确实发过包的情况。
type HTTPUpstreamNotSentError struct{ Err error }

// Error 原样返回内层错误的文本，**不加前缀**。
//
// 这个类型包在共享的传输路径上（`Do` / `DoWithTLS` 是整个网关所有上游请求的出口），加前缀会改掉
// 每一条 SSRF 拦截与连接池耗尽的日志与对外错误文案——为了本功能的一个判定去改全局文案不划算。
// 它只需要能被 errors.As 认出来，文案保持原样。
func (e *HTTPUpstreamNotSentError) Error() string { return e.Err.Error() }

func (e *HTTPUpstreamNotSentError) Unwrap() error { return e.Err }
