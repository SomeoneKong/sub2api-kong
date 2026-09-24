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

// 账号指纹测试对上游的访问：经账号自己的代理，用账号自己的凭据向 codex responses 端点发一份挑战，读回
// 完整正文与上游回报的模型。设计见 DESIGN-openai-fingerprint-test.md。
//
// 抽成接口是为了让测试流程可测：流程与判定不该被"真打上游"这件事绑住。

// KongUpstreamAnswer 是一份指纹挑战的结果。
type KongUpstreamAnswer struct {
	Text string
	// ReportedModel 是上游在响应里回报的本次实际使用的 model。为空表示没观测到——那是「没测出来」，
	// 不是「一致」。
	ReportedModel string
	StatusCode    int
	LatencyMs     int
	// OutputTokens 为空表示上游没给 usage——那是「未知」，不是「确定为 0」。
	OutputTokens *int
}

// KongFingerprintUpstream 是指纹测试对上游的依赖面。
type KongFingerprintUpstream interface {
	// ResolveProxyURL 把代理 id 解析成 proxy URL。nil 表示直连（返回空串）。
	// 解析失败必须返回错误而不是退回直连——那会把请求送出服务器的真实 IP。
	ResolveProxyURL(ctx context.Context, proxyID *int64) (string, error)
	// RunChallenge 经 proxyURL 发一份指纹挑战，不带任何票。
	//
	// 返回值的约定：发送失败时 answer 为 nil；上游非 2xx 时 answer 带状态码、err 非空；2xx 之后读流出错
	// 时 answer 带着已经读到的正文与回报的模型、err 非空。
	RunChallenge(ctx context.Context, account *Account, proxyURL, model string, challenge KongFingerprintChallenge) (*KongUpstreamAnswer, error)
}

type kongFingerprintUpstream struct {
	httpUpstream HTTPUpstream
	proxyRepo    ProxyRepository
	tlsProfiles  *TLSFingerprintProfileService
}

// NewKongFingerprintUpstream 创建上游访问层。
func NewKongFingerprintUpstream(httpUpstream HTTPUpstream, proxyRepo ProxyRepository, tlsProfiles *TLSFingerprintProfileService) KongFingerprintUpstream {
	return &kongFingerprintUpstream{
		httpUpstream: httpUpstream,
		proxyRepo:    proxyRepo,
		tlsProfiles:  tlsProfiles,
	}
}

func (u *kongFingerprintUpstream) ResolveProxyURL(ctx context.Context, proxyID *int64) (string, error) {
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

// kongChallengeTimeout 是一份挑战的期限。生产上单份挑战 p95 约 82 秒。
const kongChallengeTimeout = 180 * time.Second

// kongFingerprintAccessToken 取账号上存着的 access token，口径与"测试连接"相同。不经 token provider：
// 那条路会刷新凭据、读实时缓存，凭据不可用时还会把账号置为错误状态——测试不该改账号状态，一次测试的
// 各份也都该用开始时读到的同一份凭据。
func kongFingerprintAccessToken(account *Account) (string, error) {
	token := strings.TrimSpace(account.GetOpenAIAccessToken())
	if token == "" {
		return "", fmt.Errorf("账号 %d 没有可用的访问令牌", account.ID)
	}
	return token, nil
}

// buildCodexRequest 构造一个 codex responses 请求。
//
// 头部照上游探测请求的口径补齐：originator 与 UA 首段必须配套、version 不得低于下限，否则上游直接
// 404。这几条都由 applyOpenAICodexProbeHeaders / enforceCodexIdentityHeadersWithUA 收口。
func (u *kongFingerprintUpstream) buildCodexRequest(ctx context.Context, account *Account, model, prompt string) (*http.Request, error) {
	if account == nil {
		return nil, fmt.Errorf("账号为空")
	}
	token, err := kongFingerprintAccessToken(account)
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
	enforceCodexIdentityHeadersWithUA(req.Header, account.GetOpenAIUserAgent())
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// RunChallenge 发一份指纹挑战并读回完整正文。
func (u *kongFingerprintUpstream) RunChallenge(ctx context.Context, account *Account, proxyURL, model string, challenge KongFingerprintChallenge) (*KongUpstreamAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, kongChallengeTimeout)
	defer cancel()

	req, err := u.buildCodexRequest(ctx, account, model, challenge.Prompt)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	resp, err := u.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, u.tlsProfiles.ResolveTLSProfile(account))
	if err != nil {
		return nil, fmt.Errorf("挑战请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	answer := &KongUpstreamAnswer{StatusCode: resp.StatusCode}
	if resp.StatusCode >= 400 {
		answer.LatencyMs = int(time.Since(started).Milliseconds())
		return answer, fmt.Errorf("挑战请求返回 %d", resp.StatusCode)
	}
	sse, err := kongReadCodexSSEText(resp.Body)
	answer.OutputTokens = sse.OutputTokens
	answer.LatencyMs = int(time.Since(started).Milliseconds())
	answer.Text = sse.Text
	// 即使流出错也带回已读到的回报值：它在 response.created 就到了，"上游明说给了别的模型"这个结论
	// 不因正文残缺而失效。
	answer.ReportedModel = sse.ReportedModel
	if err != nil {
		return answer, err
	}
	return answer, nil
}

// kongSSEAnswer 是一次 SSE 读取的产物。
//
// 用结构体而不是多返回值：Text 与 ReportedModel 都是 string，相邻的同类型返回值调用方错序了也能
// 编译过，而那会把回答正文当成回报的模型名。
type kongSSEAnswer struct {
	Text string
	// OutputTokens 为空表示上游没给 usage——那是「未知」，不是「确定为 0」。
	OutputTokens *int
	// ReportedModel 是上游回报的本次实际使用的 model。为空表示流里没有这个字段。
	ReportedModel string
}

// kongReadCodexSSEText 从 codex 的 SSE 流里累积回答正文，并取出完成事件带的输出 token 数与上游
// 回报的 model。
//
// 事件口径与上游一致：`response.output_text.delta` 带文本增量，`response.completed` / `response.done`
// 表示正常结束，`response.failed` / `error` 是失败。**不吞错误**——半截的回答会被归因当成截断
// 处理，但「流中途报错」和「模型只答了一半」是两回事，前者要能看见。
func kongReadCodexSSEText(body io.Reader) (kongSSEAnswer, error) {
	var text strings.Builder
	var outputTokens *int
	var reportedModel string
	reader := bufio.NewReaderSize(body, 64*1024)
	// 收尾时统一组装：下面每条失败路径都要带回已经读到的部分（正文用于诊断，model 与 tokens 说明
	// 额度花在哪了），逐处手写三个字段必漏。
	result := func() kongSSEAnswer {
		return kongSSEAnswer{Text: text.String(), OutputTokens: outputTokens, ReportedModel: reportedModel}
	}

	// 流首可能有一个 UTF-8 BOM。不吃掉它，第一行就变成 "\ufeffdata: {...}"，`data:` 前缀匹配不上
	// ——那一整个事件被当作未知字段行忽略，正文少掉第一段。而归因只看「数字够不够」，少一段仍然够，
	// 于是一份被静默截断的回答照样会被计入归因。
	if head, err := reader.Peek(3); err == nil && bytes.Equal(head, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = reader.Discard(3)
	}

	completed := false
	// 按 SSE 的事件边界收集：**一个事件可以有多行 `data:`**，规范要求把它们用换行连起来再当成
	// 一份载荷。逐行各自解码是错的——多行事件的每一行都不是完整 JSON，于是每一行都解码失败。
	var dataLines []string
	// 解码失败必须让**这一份挑战**失败，不能 continue。丢掉一个 delta 事件只会让正文少几个数字，
	// 而归因只看「数字够不够」——217 个数字和 218 个数字都够，于是一份被静默损坏的回答照样能
	// 计入归因。证据不完整时唯一正确的处置是不给结论。
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
		eventType, _ := event["type"].(string)
		// 回报的 model：**终止事件的声明覆盖先前的，中途事件只在还没有值时记一次**。
		//
		// 终止事件的判定复用上游的 isUpstreamResponseModelTerminalEvent，不自己列——它覆盖
		// completed / done / failed / incomplete / cancelled / canceled 六种，而手写枚举漏掉
		// failed 与 incomplete 的后果是：上游在 created 里说 astra、在 failed 里说 luna 时，
		// 我们只看得见 astra，明确的降档证据被丢掉。
		//
		// 上游的口径是「只有终止事件报告实际处理的档位」，中途事件里那个是预告，所以先后顺序不能反。
		if m := kongExtractReportedModel(event); m != "" {
			if isUpstreamResponseModelTerminalEvent(eventType) || reportedModel == "" {
				reportedModel = m
			}
		}
		switch eventType {
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
			// 继续等 EOF——保持连接时能一直拖到挑战的 180 秒期限。
			return fmt.Errorf("上游报告 response.incomplete: %s", kongExtractIncompleteReason(event))
		case "error":
			return fmt.Errorf("上游报告 error: %s", kongExtractSSEError(event, ""))
		}
		return nil
	}

	skipLF := false
	for {
		line, terminated, err := kongReadSSELine(reader, &skipLF)
		if err != nil {
			return result(), fmt.Errorf("读取 SSE 流: %w", err)
		}
		if !terminated {
			// 到了流末尾。最后一行没有换行符时它仍然算这个事件的一部分。
			//
			// 这里**派发**而不是直接判失败：证据完整性由 JSON 本身保证——流被中途掐断时最后那份
			// 载荷必然是残缺 JSON，flush 会解码失败；整段事件缺失时 `completed` 永远不到，下面那条
			// 检查会拦住。所以唯一被「不派发」额外拒掉的情形是「载荷完整、只少一个收尾空行」，
			// 那种流的证据是完整的，判它失败只会在上游不发末尾空行时让每份挑战都失败。
			if line != "" {
				if strings.HasPrefix(line, "data:") {
					dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				}
			}
			if err := flush(); err != nil {
				return result(), err
			}
			break
		}
		if line == "" {
			// **空行**才是事件边界。只含空格或制表符的行属于「未知字段行」，按 SSE 规范要忽略
			// ——按 TrimSpace 判会把它当成边界，于是一个拆成多行 data 的合法事件被提前解码，
			// 报 JSON 截断、整份挑战作废。
			if err := flush(); err != nil {
				return result(), err
			}
			if completed {
				// 正常完成事件已到，不再等 EOF：继续读只会把一份完整回答拖到读超时。
				return result(), nil
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
		return result(), fmt.Errorf("SSE 流在 response.completed 之前结束")
	}
	return result(), nil
}

// kongSSEMaxLine 是 SSE 单行的读取上限。
const kongSSEMaxLine = 8 << 20

// kongReadSSELine 读一行，三种换行都认（`\r\n` / `\n` / 单独的 `\r`）。
//
// terminated 为假表示这一行没有换行符就到了流末尾——调用方据此判断「事件被截断」，而不是把它
// 当成完整的最后一行。SSE 规范明确三种都是合法行终止符，只认 `\n` 会把 CR-only 的流整份读成
// 一行、每个 `data:` 前缀都匹配不上。
//
// skipLF 跨调用保存"上一行以 `\r` 结束"：CRLF 里的 LF 要在**下一次读取**时吃掉，否则会被读成一个
// 空行、当成事件边界。不能在读到 `\r` 时就去看下一个字节——CR-only 的流里完整事件之后可能暂时没有
// 数据，那一看会一直阻塞到后续字节或读超时，已经到达的完成事件交不出去。
func kongReadSSELine(reader *bufio.Reader, skipLF *bool) (line string, terminated bool, err error) {
	var buf []byte
	for {
		b, readErr := reader.ReadByte()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return string(buf), false, nil
			}
			return string(buf), false, readErr
		}
		if *skipLF {
			*skipLF = false
			if b == '\n' {
				continue
			}
		}
		switch b {
		case '\n':
			return string(buf), true, nil
		case '\r':
			*skipLF = true
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

// kongExtractReportedModel 取事件里上游回报的 model（`response.model`）。
//
// 口径与 kongExtractOutputTokens 一致：字段缺失或类型不对一律当"没观测到"返回空串，绝不猜。空串在
// 判定里算"少一项证据"而不是"对不上"（见 kong_fingerprint_tester.go）。
//
// 长度上限与上游的 observer 一致（upstreamResponseModelMaxLength）：异常长的值只可能是上游回了
// 别的东西，截断后入库会让事件里躺着一段无法解释的文本。
func kongExtractReportedModel(event map[string]any) string {
	response, ok := event["response"].(map[string]any)
	if !ok {
		return ""
	}
	model, ok := response["model"].(string)
	if !ok {
		return ""
	}
	model = strings.TrimSpace(model)
	if model == "" || len(model) > upstreamResponseModelMaxLength {
		return ""
	}
	return model
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
