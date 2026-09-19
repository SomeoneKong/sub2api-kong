package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// 票据子系统主动发起的两种上游请求。设计见 DESIGN-codex-ticket.md §4.2。
//
// 抽成接口是为了让编排层可测：取票与验证都要真打上游，编排的状态机不该被这件事绑住。

// KongUpstreamProbe 是一次取票的结果。
type KongUpstreamProbe struct {
	// State 是响应头里的票。空表示上游没下发——它认为本次请求已带有效票。
	State      string
	StatusCode int
	// IdleSecondsAtStart 由调用方填，仅为写事件方便。
	IdleSecondsAtStart *int64
}

// KongUpstreamAnswer 是一次指纹挑战的结果。
type KongUpstreamAnswer struct {
	Text string
	// EchoedState 非空表示上游**又下发了一张票**，也就是本次注入没被接受——那么这次回答的
	// 档位反映的不是被验证的那张票，不能用来判定它合格。
	EchoedState string
	StatusCode  int
	LatencyMs   int
	// OutputTokens 为空表示上游没给 usage——那是「未知」，不是「确定为 0」。把未知记成 0 会让
	// 「这次探测烧了多少额度」这类统计得出与事实相反的结论。
	OutputTokens *int
}

// KongTicketUpstream 是票据子系统对上游的依赖面。
type KongTicketUpstream interface {
	// ResolveProxyURL 把代理 id 解析成 proxy URL。nil 表示直连（返回空串）。
	// 解析失败必须返回错误而不是退回直连——那会把请求送出服务器的真实 IP。
	ResolveProxyURL(ctx context.Context, proxyID *int64) (string, error)
	// ProxyState 查代理此刻的存在性、启用状态与有效期。
	//
	// 与 ResolveProxyURL 分开是必须的：后者只拼连接串，拼得出来不代表这个代理还能用。只看
	// 「解析成功」会让到期与停用两个分支在生产里永远不可达——而那正是票据出口最常见的失效
	// 方式（上游的到期清扫只改 accounts.proxy_id，不认识我们的键）。
	ProxyState(ctx context.Context, proxyID *int64) (KongTicketProxyState, error)
	// FetchTurnState 经指定出口发一个最小请求，只为拿回响应头里的票。
	FetchTurnState(ctx context.Context, account *Account, egressProxyURL, model string) (*KongUpstreamProbe, error)
	// RunChallenge 经流量出口发一次指纹挑战。injectState 非空时带上它——验证必须带着被验证的
	// 那张票发出，否则测到的是另一次请求的状态。
	RunChallenge(ctx context.Context, account *Account, trafficProxyURL, model string, challenge KongFingerprintChallenge, injectState string) (*KongUpstreamAnswer, error)
}

type kongTicketUpstream struct {
	httpUpstream  HTTPUpstream
	tokenProvider *OpenAITokenProvider
	proxyRepo     ProxyRepository
	tlsProfiles   *TLSFingerprintProfileService
}

// NewKongTicketUpstream 创建上游访问层。
func NewKongTicketUpstream(httpUpstream HTTPUpstream, tokenProvider *OpenAITokenProvider, proxyRepo ProxyRepository, tlsProfiles *TLSFingerprintProfileService) KongTicketUpstream {
	return &kongTicketUpstream{
		httpUpstream:  httpUpstream,
		tokenProvider: tokenProvider,
		proxyRepo:     proxyRepo,
		tlsProfiles:   tlsProfiles,
	}
}

func (u *kongTicketUpstream) ResolveProxyURL(ctx context.Context, proxyID *int64) (string, error) {
	if proxyID == nil {
		return "", nil
	}
	proxy, err := u.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		return "", fmt.Errorf("读代理 %d: %w", *proxyID, err)
	}
	if proxy == nil {
		return "", fmt.Errorf("代理 %d 不存在", *proxyID)
	}
	url := proxy.URL()
	if strings.TrimSpace(url) == "" {
		return "", fmt.Errorf("代理 %d 没有可用的连接串", *proxyID)
	}
	return url, nil
}

func (u *kongTicketUpstream) ProxyState(ctx context.Context, proxyID *int64) (KongTicketProxyState, error) {
	if proxyID == nil {
		// 直连没有代理记录可查；「直连是否与业务出口重合」由 KongEvaluateEgress 判断。
		return KongTicketProxyState{Exists: true}, nil
	}
	proxy, err := u.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil {
		// 查不到与出错分不开时按「不存在」处理：那一侧的后果是停止主动取票，是保守方向。
		return KongTicketProxyState{Exists: false}, nil
	}
	return KongProxyStateOf(proxy, time.Now()), nil
}

// kongCodexProbePrompt 是取票用的最小 prompt。取票只要响应头，正文越短越省额度。
const kongCodexProbePrompt = "ok"

const (
	kongFetchTimeout     = 60 * time.Second
	kongChallengeTimeout = 180 * time.Second
)

// accessToken 按账号类型取 bearer。
//
// 口径必须与业务转发一致（见 openai_gateway_service.go 的 resolveOpenAI* 分支）：SetupToken 是
// 只用于推理的 bearer，没有 refresh 生命周期，走 OAuth 刷新流程一定被拒。管理面按
// UsesOpenAICodexProtocol 列账号，那个判定是收 SetupToken 的——两边不一致会让这类账号在界面上
// 配得出 full、实际永远取不到票，表现成该账号在门控模型上彻底不可用。
func (u *kongTicketUpstream) accessToken(ctx context.Context, account *Account) (string, error) {
	var token string
	switch account.Type {
	case AccountTypeSetupToken:
		if !account.IsOpenAIOAuthLike() {
			return "", fmt.Errorf("账号 %d 的 setup token 不走 codex 协议", account.ID)
		}
		token = account.GetOpenAIAccessToken()
	default:
		var err error
		token, err = u.tokenProvider.GetAccessToken(ctx, account)
		if err != nil {
			return "", fmt.Errorf("取访问令牌: %w", err)
		}
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("账号 %d 没有可用的访问令牌", account.ID)
	}
	return token, nil
}

// buildCodexRequest 构造一个 codex responses 请求。
//
// 头部照上游探测请求的口径补齐：originator 与 UA 首段必须配套、version 不得低于下限，否则
// 上游直接 404。这几条都由 applyOpenAICodexProbeHeaders / enforceCodexIdentityHeadersWithUA 收口。
func (u *kongTicketUpstream) buildCodexRequest(ctx context.Context, account *Account, model, prompt, injectState string) (*http.Request, error) {
	if account == nil {
		return nil, fmt.Errorf("账号为空")
	}
	token, err := u.accessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"model": model,
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": prompt,
			}},
		}},
		"stream":       true,
		"store":        false,
		"instructions": openai.DefaultInstructions,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("编码请求体: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Host = "chatgpt.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	applyOpenAICodexProbeHeaders(req.Header)
	setOpenAIChatGPTAccountHeaders(req.Header, account)
	if injectState != "" {
		req.Header.Set(openAICodexTurnStateHeader, injectState)
	}
	enforceCodexIdentityHeadersWithUA(req.Header, account.GetOpenAIUserAgent())
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

func (u *kongTicketUpstream) send(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	var profile = u.tlsProfiles.ResolveTLSProfile(account)
	return u.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, profile)
}

// FetchTurnState 只取响应头里的票。
//
// 请求刻意**不带**任何票：上游的规则是「带有效票就不下发、不带才下发」，带着票去取票只会拿回空。
func (u *kongTicketUpstream) FetchTurnState(ctx context.Context, account *Account, egressProxyURL, model string) (*KongUpstreamProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, kongFetchTimeout)
	defer cancel()

	req, err := u.buildCodexRequest(ctx, account, model, kongCodexProbePrompt, "")
	if err != nil {
		// 还没发包。调用方据此不推进出口活动 A——静默其实还在。
		return nil, &KongErrUpstreamNotAttempted{Err: err}
	}
	resp, err := u.send(req, egressProxyURL, account)
	if err != nil {
		// 传输层已经区分过「还没发包」：主机校验不通过、客户端池取不到连接都属于本地失败，
		// 一个字节都没出去，不能推进出口活动 A。
		var notSent *HTTPUpstreamNotSentError
		if errors.As(err, &notSent) {
			return nil, &KongErrUpstreamNotAttempted{Err: err}
		}
		return nil, fmt.Errorf("取票请求失败: %w", err)
	}
	defer func() {
		// 必须读干并关闭：连接池的在途计数依赖它，漏关会泄漏计数并阻止淘汰。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	probe := &KongUpstreamProbe{
		State:      extractOpenAICodexTurnState(resp.Header),
		StatusCode: resp.StatusCode,
	}
	if resp.StatusCode >= 400 {
		return probe, fmt.Errorf("取票请求返回 %d", resp.StatusCode)
	}
	return probe, nil
}

// RunChallenge 发一次指纹挑战并读回完整正文。
func (u *kongTicketUpstream) RunChallenge(ctx context.Context, account *Account, trafficProxyURL, model string, challenge KongFingerprintChallenge, injectState string) (*KongUpstreamAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, kongChallengeTimeout)
	defer cancel()

	req, err := u.buildCodexRequest(ctx, account, model, challenge.Prompt, injectState)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	resp, err := u.send(req, trafficProxyURL, account)
	if err != nil {
		return nil, fmt.Errorf("挑战请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	answer := &KongUpstreamAnswer{
		StatusCode: resp.StatusCode,
		// 响应又带票 = 本次注入没被上游接受。这一条决定该次回答能不能用来判定那张票。
		EchoedState: extractOpenAICodexTurnState(resp.Header),
	}
	if resp.StatusCode >= 400 {
		return answer, fmt.Errorf("挑战请求返回 %d", resp.StatusCode)
	}
	text, outputTokens, err := kongReadCodexSSEText(resp.Body)
	answer.OutputTokens = outputTokens
	answer.LatencyMs = int(time.Since(started).Milliseconds())
	answer.Text = text
	if err != nil {
		return answer, err
	}
	return answer, nil
}

// kongReadCodexSSEText 从 codex 的 SSE 流里累积回答正文，并取出完成事件带的输出 token 数。
//
// 第二个返回值为空表示上游没给 usage。
//
// 事件口径与上游一致：`response.output_text.delta` 带文本增量，`response.completed` / `response.done`
// 表示正常结束，`response.failed` / `error` 是失败。**不吞错误**——半截的回答会被归因当成截断
// 处理，但「流中途报错」和「模型只答了一半」是两回事，前者要能看见。
func kongReadCodexSSEText(body io.Reader) (string, *int, error) {
	var text strings.Builder
	var outputTokens *int
	reader := bufio.NewReaderSize(body, 64*1024)

	// 流首可能有一个 UTF-8 BOM。不吃掉它，第一行就变成 "\ufeffdata: {...}"，`data:` 前缀匹配不上
	// ——那一整个事件被当作未知字段行忽略，正文少掉第一段。而归因只看「数字够不够」，少一段仍然够，
	// 于是一份被静默截断的回答照样能授予资格。
	if head, err := reader.Peek(3); err == nil && bytes.Equal(head, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = reader.Discard(3)
	}

	completed := false
	// 按 SSE 的事件边界收集：**一个事件可以有多行 `data:`**，规范要求把它们用换行连起来再当成
	// 一份载荷。逐行各自解码是错的——多行事件的每一行都不是完整 JSON，于是每一行都解码失败。
	var dataLines []string
	// 解码失败必须让**这一份挑战**失败，不能 continue。丢掉一个 delta 事件只会让正文少几个数字，
	// 而归因只看「数字够不够」——217 个数字和 218 个数字都够，于是一份被静默损坏的回答照样能
	// 授予资格。证据不完整时唯一正确的处置是不给结论。
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("SSE 事件解码失败，证据不完整: %w", err)
		}
		switch eventType, _ := event["type"].(string); eventType {
		case "response.output_text.delta":
			// 字段必须**存在且是字符串**。缺字段与类型不对是同一件事：这条事件本该携带一段正文
			// 而我们没拿到，正文已经不完整。合法的空字符串仍然接受。
			delta, ok := event["delta"]
			if !ok {
				return fmt.Errorf("SSE 文本增量事件缺少 delta 字段，证据不完整")
			}
			str, ok := delta.(string)
			if !ok {
				return fmt.Errorf("SSE 事件的 delta 不是字符串")
			}
			// strings.Builder.WriteString 的错误恒为 nil，但 errcheck 要求显式忽略。
			_, _ = text.WriteString(str)
		case "response.completed", "response.done":
			completed = true
			if n, ok := kongExtractOutputTokens(event); ok {
				outputTokens = &n
			}
		case "response.failed":
			return fmt.Errorf("上游报告 response.failed: %s", kongExtractSSEError(event, "response"))
		case "response.incomplete":
			// 合法的失败终端。当成未知事件忽略会丢掉原因（`max_output_tokens` 之类），而且函数会
			// 继续等 EOF——保持连接时能一直拖到挑战的 180 秒期限，白占一个验证槽。
			return fmt.Errorf("上游报告 response.incomplete: %s", kongExtractIncompleteReason(event))
		case "error":
			return fmt.Errorf("上游报告 error: %s", kongExtractSSEError(event, ""))
		}
		return nil
	}

	for {
		line, terminated, err := kongReadSSELine(reader)
		if err != nil {
			return text.String(), outputTokens, fmt.Errorf("读取 SSE 流: %w", err)
		}
		if !terminated {
			// 到了流末尾。最后一行没有换行符时它仍然算这个事件的一部分。
			//
			// 这里**派发**而不是直接判失败：证据完整性由 JSON 本身保证——流被中途掐断时最后那份
			// 载荷必然是残缺 JSON，flush 会解码失败；整段事件缺失时 `completed` 永远不到，下面那条
			// 检查会拦住。所以唯一被「不派发」额外拒掉的情形是「载荷完整、只少一个收尾空行」，
			// 那种流的证据是完整的，判它失败只会在上游不发末尾空行时把整个功能打死。
			if line != "" {
				if strings.HasPrefix(line, "data:") {
					dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				}
			}
			if err := flush(); err != nil {
				return text.String(), outputTokens, err
			}
			break
		}
		if line == "" {
			// **空行**才是事件边界。只含空格或制表符的行属于「未知字段行」，按 SSE 规范要忽略
			// ——按 TrimSpace 判会把它当成边界，于是一个拆成多行 data 的合法事件被提前解码，
			// 报 JSON 截断、整份挑战作废。那是无谓拒服。
			if err := flush(); err != nil {
				return text.String(), outputTokens, err
			}
			if completed {
				// 正常完成事件已到，不再等 EOF：继续读只会把一份完整回答拖到读超时。
				return text.String(), outputTokens, nil
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// `event:` / `id:` / `retry:` / 注释行（`:` 开头）以及任何未知字段一律忽略。
			continue
		}
		dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
	}
	if !completed {
		return text.String(), outputTokens, fmt.Errorf("SSE 流在 response.completed 之前结束")
	}
	return text.String(), outputTokens, nil
}

// kongSSEMaxLine 是单行上限，与原先 bufio.Scanner 的口径一致。
const kongSSEMaxLine = 8 << 20

// kongReadSSELine 读一行，三种换行都认（`\r\n` / `\n` / 单独的 `\r`）。
//
// terminated 为假表示这一行没有换行符就到了流末尾——调用方据此判断「事件被截断」，而不是把它
// 当成完整的最后一行。SSE 规范明确三种都是合法行终止符，只认 `\n` 会把 CR-only 的流整份读成
// 一行、每个 `data:` 前缀都匹配不上。
func kongReadSSELine(reader *bufio.Reader) (line string, terminated bool, err error) {
	var buf []byte
	for {
		b, readErr := reader.ReadByte()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return string(buf), false, nil
			}
			return string(buf), false, readErr
		}
		switch b {
		case '\n':
			return string(buf), true, nil
		case '\r':
			// CRLF 里的 LF 要一并吃掉，否则下一轮会读到一个空行、被当成事件边界。
			if next, peekErr := reader.Peek(1); peekErr == nil && next[0] == '\n' {
				_, _ = reader.Discard(1)
			}
			return string(buf), true, nil
		default:
			if len(buf) >= kongSSEMaxLine {
				return string(buf), false, fmt.Errorf("SSE 单行超过 %d 字节", kongSSEMaxLine)
			}
			buf = append(buf, b)
		}
	}
}

// kongExtractIncompleteReason 取 response.incomplete 的原因。
func kongExtractIncompleteReason(event map[string]any) string {
	response, ok := event["response"].(map[string]any)
	if !ok {
		return "未给出原因"
	}
	details, ok := response["incomplete_details"].(map[string]any)
	if !ok {
		return "未给出原因"
	}
	if reason, ok := details["reason"].(string); ok && reason != "" {
		return reason
	}
	return "未给出原因"
}

// kongExtractOutputTokens 从完成事件里取 usage.output_tokens。
func kongExtractOutputTokens(event map[string]any) (int, bool) {
	response, ok := event["response"].(map[string]any)
	if !ok {
		return 0, false
	}
	usage, ok := response["usage"].(map[string]any)
	if !ok {
		return 0, false
	}
	raw, ok := usage["output_tokens"].(float64)
	if !ok || raw < 0 || raw != math.Trunc(raw) {
		return 0, false
	}
	return int(raw), true
}

func kongExtractSSEError(event map[string]any, nested string) string {
	source := event
	if nested != "" {
		if inner, ok := event[nested].(map[string]any); ok {
			source = inner
		}
	}
	if errData, ok := source["error"].(map[string]any); ok {
		if msg, ok := errData["message"].(string); ok && msg != "" {
			return msg
		}
	}
	return "未提供错误信息"
}
