package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// openAICodexTurnStateHeader 是 Codex 的回合状态头。上游在响应头中铸造该
// 不透明 blob，客户端在同一回合的后续请求中原样回带（codex-rs 侧从
// /responses SSE、/responses/compact JSON 与 WS 握手三种响应中捕获，见
// codex-api/src/sse/responses.rs 与 endpoint/compact.rs）。
const openAICodexTurnStateHeader = "x-codex-turn-state"

// turn-state blob 是上游在"出站身份"（含 #5553 指纹收敛改写后的
// installation/session/thread 标识）下铸造的，同账号回放自洽；跨账号回放
// （failover 换号后客户端仍回带旧账号的 blob）是代理链独有、真实 Codex
// 永远不会产生的矛盾信号。出站守卫据此剥离已知异账号的回带值。
//
// **溯源按 blob 记，不按会话记。** 记"这个会话最近由谁铸造"表达不了要判的那件事：同一个
// session-id 下可以并存父子线程（执行作用域正是为此把 thread 纳入键），一次 failover 也会让同一会话
// 先后持有两个账号的 blob。那时"最近账号"会同时造成两种错判——旧账号的 blob 因为最近账号变了而被
// **放行**，本账号自己的 blob 又因为最近账号是别人而被**误剥**。按 blob 指纹记，两边都能判准，代价
// 是条目数从"每会话一条"变成"每个已交付 blob 一条"（仍受 TTL 与清扫约束）。
type openAICodexTurnStateOrigin struct {
	accountID int64
	expiresAt time.Time
}

// openAICodexTurnStateKey 是溯源表键：会话 seed + blob 指纹。
//
// 指纹取 sha256 前 8 字节：**绝不存 blob 原值**，它是可注入的凭据；16 个 hex 足以在一小时 TTL 内区分
// 是不是同一个 blob。seed 仍然保留在键里，让不同租户/会话的记录互不可见。
func openAICodexTurnStateKey(seed, state string) string {
	state = strings.TrimSpace(state)
	if seed == "" || state == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(state))
	return seed + "\x00" + hex.EncodeToString(sum[:8])
}

// openAICodexTurnStateSeed 返回溯源表键：API Key + 客户端原始会话标识。
// 客户端会话标识取自请求头（与指纹收敛的 thread 派生同源，见
// extractClientSessionID），确保同一下游会话的记录/守卫两侧使用同一键。
// 无会话标识时返回空串，表示不做跟踪（保持透传现状）。
func openAICodexTurnStateSeed(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	sessionID := extractClientSessionID(c.Request.Header)
	if sessionID == "" {
		return ""
	}
	return strconv.FormatInt(getAPIKeyIDFromContext(c), 10) + "\x00" + sessionID
}

// relayOpenAICodexTurnState 将上游响应中的 turn-state 显式写入下游响应头，
// 并记录铸造账号。必须在响应头提交点调用（WriteHeader 之前、且确认本次
// 上游响应就是将要写回客户端的响应之后）。上游无该头时主动清除 writer 上
// 可能残留的上一 failover attempt 的值——否则换号后旧账号的 blob 会粘到
// 新账号的响应上，这正是本文件要防止的跨账号矛盾。
func (s *OpenAIGatewayService) relayOpenAICodexTurnState(c *gin.Context, account *Account, upstream http.Header) {
	if c == nil || c.Writer == nil {
		return
	}
	canonical := http.CanonicalHeaderKey(openAICodexTurnStateHeader)
	state := extractOpenAICodexTurnState(upstream)
	if state == "" {
		c.Writer.Header().Del(canonical)
		return
	}
	c.Writer.Header().Set(canonical, state)
	s.noteOpenAICodexTurnStateProvenance(c, account, state)
}

// stageOpenAICodexTurnState 将上游 turn-state 暂存到延迟提交的响应头集合
// （首输出守卫路径先缓存头、见到首个输出事件才提交）。此处**不**记录铸造
// 账号：该 attempt 仍可能在首输出超时后 failover，暂存头会被整体丢弃，
// 客户端从未收到该 blob。溯源必须在真正提交时记录，见
// noteStagedOpenAICodexTurnStateCommitted。
func stageOpenAICodexTurnState(dst *http.Header, upstream http.Header) {
	if dst == nil {
		return
	}
	canonical := http.CanonicalHeaderKey(openAICodexTurnStateHeader)
	state := extractOpenAICodexTurnState(upstream)
	if state == "" {
		if *dst != nil {
			dst.Del(canonical)
		}
		return
	}
	if *dst == nil {
		*dst = http.Header{}
	}
	dst.Set(canonical, state)
}

// noteStagedOpenAICodexTurnStateCommitted 在暂存响应头真正写入下游时记录
// 铸造账号——只有此刻客户端才确定收到了该 blob，溯源表才与客户端持有的
// 值一致（否则被 failover 丢弃的 attempt 会污染溯源，导致后续误剥离）。
func (s *OpenAIGatewayService) noteStagedOpenAICodexTurnStateCommitted(c *gin.Context, account *Account, staged http.Header) {
	if staged == nil {
		return
	}
	state := strings.TrimSpace(staged.Get(openAICodexTurnStateHeader))
	if state == "" {
		return
	}
	s.noteOpenAICodexTurnStateProvenance(c, account, state)
}

// openAIWSTurnStateMetadataKey 是原生 WS 上 turn-state 的载体键：客户端放在 `response.create` 帧的
// `client_metadata` 里上送，上游放在带内 metadata 事件的 headers 里下发。
//
// WS 与 HTTP 的通道是对称的，只是载体不同（口径取自 codex 客户端源码）：
//
//	                客户端 → 上游                              上游 → 客户端
//	HTTP   `x-codex-turn-state` 请求头                        响应头
//	WS     payload 的 `client_metadata["x-codex-turn-state"]`  带内 metadata 事件的 headers
const openAIWSTurnStateMetadataKey = "x-codex-turn-state"

// isOpenAIWSTurnStateMetadataEvent 判断一帧是不是带内 turn-state 的载体事件。
//
// **两种拼写都要认。** 原生 WS 上游发的是 `codex.response.metadata`（实测线上帧得到），SSE 上则是
// 不带前缀的 `response.metadata`。只认后者，WS 这条路一帧也匹配不上——state 明明就在帧里，只是类型名
// 对不上，调用点全部静默 return。
//
// 不用「剥掉 codex. 前缀」的写法：上游同一条连接上还发 `codex.rate_limits`、
// `responsesapi.websocket_timing` 这类带外事件，按前缀泛化等于把未知事件也当成载体。
func isOpenAIWSTurnStateMetadataEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.metadata", "codex.response.metadata":
		return true
	default:
		return false
	}
}

// openAIWSTurnStateFromEvent 从带内 metadata 事件里取 turn-state。
//
// 两处都要看且键名大小写不敏感：codex 先读 `response.headers`、再回落到顶层 `headers`
// （codex-rs/codex-api/src/sse/responses.rs）。只认一处会在另一种形态下漏掉它。
func openAIWSTurnStateFromEvent(payload []byte) string {
	for _, path := range []string{"response.headers", "headers"} {
		headers := gjson.GetBytes(payload, path)
		if !headers.IsObject() {
			continue
		}
		found := ""
		headers.ForEach(func(key, value gjson.Result) bool {
			if strings.EqualFold(key.String(), openAIWSTurnStateMetadataKey) {
				found = strings.TrimSpace(value.String())
				return false
			}
			return true
		})
		if found != "" {
			return found
		}
	}
	return ""
}

func extractOpenAICodexTurnState(upstream http.Header) string {
	if upstream == nil {
		return ""
	}
	return strings.TrimSpace(upstream.Get(openAICodexTurnStateHeader))
}

// noteOpenAICodexTurnStateProvenance 记录（这个 blob → 铸造账号）。
//
// **只在 blob 确实要交付给客户端时调用**：没送出去的（被后续守卫拦下、attempt 被放弃、写失败）不记，
// 否则溯源表会声称客户端持有一个它其实从没见过的值，导致后续误剥离。
func (s *OpenAIGatewayService) noteOpenAICodexTurnStateProvenance(c *gin.Context, account *Account, state string) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	key := openAICodexTurnStateKey(openAICodexTurnStateSeed(c), state)
	if key == "" {
		return
	}
	s.openaiCodexTurnStateOrigins.Store(key, openAICodexTurnStateOrigin{
		accountID: account.ID,
		expiresAt: time.Now().Add(s.openAIWSSessionStickyTTL()),
	})
	s.sweepOpenAICodexTurnStateOrigins()
}

// openAIWSClientTurnStateForAccount 返回客户端回带的 turn-state 里**本账号可以用**的那一份。
//
// 与 HTTP 侧同一件事：已知由别的账号铸造的就不要往上游发。WS 上原先完全没有这道守卫——三条通路都是
// 直接读 upgrade 请求头往上游握手头里放——于是 failover 换号之后，上一个账号铸造的 blob 会被原样发到
// 新账号的连接上。
//
// **只读不改 `c.Request.Header`。** HTTP 侧那个守卫剥的是**出站请求**的头（一次 attempt 一个对象），
// 而客户端请求是整个请求生命周期里所有 attempt 共用的那一份：在它上面 Del，等于让 B 这次的剥离把值
// 永久抹掉，后面重试回 A 时连自己合法的 blob 都拿不到了（passthrough 没有粘滞兜底，直接丢上下文）。
func (s *OpenAIGatewayService) openAIWSClientTurnStateForAccount(c *gin.Context, account *Account) string {
	if c == nil || c.Request == nil {
		return ""
	}
	state := strings.TrimSpace(c.GetHeader(openAICodexTurnStateHeader))
	if state == "" {
		return ""
	}
	if s.openAICodexTurnStateIsForeign(c, account, state) {
		return ""
	}
	return state
}

// openAICodexTurnStateIsForeign 判断这个 blob 是否**已知**由别的账号铸造。
//
// 查无来源、记录已过期、没有会话标识都返回 false（不剥）：无从判定来源就剥，等于无谓丢掉客户端的
// 对话上下文。这条口径与 HTTP 侧的守卫一致，两边共用同一张溯源表。
func (s *OpenAIGatewayService) openAICodexTurnStateIsForeign(c *gin.Context, account *Account, state string) bool {
	if s == nil || account == nil {
		return false
	}
	key := openAICodexTurnStateKey(openAICodexTurnStateSeed(c), state)
	if key == "" {
		return false
	}
	raw, ok := s.openaiCodexTurnStateOrigins.Load(key)
	if !ok {
		return false
	}
	origin, ok := raw.(openAICodexTurnStateOrigin)
	if !ok {
		s.openaiCodexTurnStateOrigins.CompareAndDelete(key, raw)
		return false
	}
	if !origin.expiresAt.IsZero() && time.Now().After(origin.expiresAt) {
		s.openaiCodexTurnStateOrigins.CompareAndDelete(key, raw)
		return false
	}
	return origin.accountID != account.ID
}

// stripForeignOpenAIWSFrameTurnState 删掉**帧内**已知由别的账号铸造的 turn-state。
//
// 原生 WS 的上行载体有两个：连接级的 upgrade 请求头，以及**逐轮的帧内** `client_metadata`
// （见 openAIWSTurnStateMetadataKey）。只守住头等于只关了一半——客户端把旧账号的 blob 放在每个
// `response.create` 帧里照样能发出去。
//
// 返回可能是新分配的 payload；调用方必须用返回值。任何改写帧内 turn-state 的逻辑都必须排在它之后：
// 顺序反了，判定看到的"客户端那份"就是刚写进去的值。
//
// **载体有歧义时拒服，不替上游猜解析语义**：`client_metadata` 或那个键出现多次时，gjson 读到的是第一处、
// sjson 也只删第一处，而上游完全可能按末键解码——删掉第一处之后帧里反而只剩那个异账号的值，JSON 还变得
// 毫无歧义。
// 第二个返回值是**被剥掉的那个客户端原值**：剥完之后帧里就再也读不到它了，调用方要留痕只能从这里拿。
func (s *OpenAIGatewayService) stripForeignOpenAIWSFrameTurnState(c *gin.Context, account *Account, payload []byte) ([]byte, string, error) {
	if s == nil || len(payload) == 0 {
		return payload, "", nil
	}
	// 先做一次廉价排除：这条检查对**每一帧**都要跑（分类之前就得跑，否则重复 `type` 就能绕过），而帧里
	// 多半压根没有这个键，没必要为它整份解析 JSON。
	// 注意转义：这一步比的是**原始字节**，而解析器读的是解码后的键——`x-codex-turn-stat\u0065` 是一段
	// 合法 JSON，字面串比不中，解码出来却仍是那个键。所以带反斜杠的帧一律落到完整遍历，不能在这里放过。
	if !bytes.Contains(payload, []byte(openAIWSTurnStateMetadataKey)) && bytes.IndexByte(payload, '\\') < 0 {
		return payload, "", nil
	}
	states, unique := openAIWSFrameTurnStates(payload)
	if len(states) == 0 {
		return payload, "", nil
	}
	foreign := ""
	for _, state := range states {
		if s.openAICodexTurnStateIsForeign(c, account, state) {
			foreign = state
			break
		}
	}
	if foreign == "" {
		return payload, "", nil
	}
	if !unique {
		return payload, "", errors.New("client_metadata 的 turn-state 载体有歧义，且其中含已知由别的账号铸造的值")
	}
	next, err := sjson.DeleteBytes(payload, "client_metadata."+openAIWSTurnStateMetadataKey)
	if err != nil {
		return payload, "", fmt.Errorf("剥离异账号 turn-state 失败: %w", err)
	}
	// 剥离是一次安全动作，不该静默：没有这条日志，运维看不出客户端正在回带别的账号的凭据。
	logOpenAIWSModeWarn(
		"foreign_turn_state_stripped account_id=%d state_len=%d",
		account.ID,
		len(foreign),
	)
	return next, foreign, nil
}

// openAIWSFrameTurnStates 收集帧内**所有** client_metadata 里的 turn-state 值。
//
// unique 表示"只有唯一一处、且它就是 gjson 按路径读到的那一个"——只有这种情形才能用 sjson 精确删除。
// 重复的 `client_metadata`、重复的 state 键、或值藏在第二个 client_metadata 里（按路径读不到）都算不唯一。
func openAIWSFrameTurnStates(raw []byte) (states []string, unique bool) {
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return nil, false
	}
	metaCount := 0
	root.ForEach(func(key, item gjson.Result) bool {
		if key.String() != "client_metadata" || !item.IsObject() {
			return true
		}
		metaCount++
		item.ForEach(func(metaKey, metaItem gjson.Result) bool {
			if metaKey.String() != openAIWSTurnStateMetadataKey {
				return true
			}
			if state := strings.TrimSpace(metaItem.String()); state != "" {
				states = append(states, state)
			}
			return true
		})
		return true
	})
	if len(states) != 1 || metaCount != 1 {
		return states, false
	}
	byPath := strings.TrimSpace(gjson.GetBytes(raw, "client_metadata."+openAIWSTurnStateMetadataKey).String())
	return states, byPath == states[0]
}

// stripForeignOpenAIWSMapTurnState 是 HTTP→WS 那条路的同款判定（那里的 payload 是 map）。
//
// **不就地改内层 map**：`client_metadata` 与客户端原始请求体是同一个对象（顶层只做过浅拷贝，见
// buildOpenAIWSCreatePayload），就地删会把客户端本次带来的值永久抹掉——failover 重试回原账号时，连它
// 自己合法的那份也没了。
// 返回被剥掉的那个客户端原值（理由同字节帧那版）。
func (s *OpenAIGatewayService) stripForeignOpenAIWSMapTurnState(c *gin.Context, account *Account, payload map[string]any) string {
	if s == nil || payload == nil {
		return ""
	}
	meta, ok := payload["client_metadata"].(map[string]any)
	if !ok || meta == nil {
		return ""
	}
	raw, ok := meta[openAIWSTurnStateMetadataKey]
	if !ok {
		return ""
	}
	state, ok := raw.(string)
	if !ok || strings.TrimSpace(state) == "" {
		return ""
	}
	state = strings.TrimSpace(state)
	if !s.openAICodexTurnStateIsForeign(c, account, state) {
		return ""
	}
	next := make(map[string]any, len(meta))
	for k, v := range meta {
		if k == openAIWSTurnStateMetadataKey {
			continue
		}
		next[k] = v
	}
	payload["client_metadata"] = next
	logOpenAIWSModeWarn(
		"foreign_turn_state_stripped account_id=%d state_len=%d carrier=map",
		account.ID,
		len(state),
	)
	return state
}

// noteOpenAIWSCodexTurnStateDelivered 记录"上游给的这个 state 已经到了客户端手里"。
//
// 原生 WS 上这个 state 只能经**带内 metadata 事件**到达客户端（我们的 upgrade 响应早就发完了，
// 加不了头）。客户端会不会解析并在下次 upgrade 请求头上回带，取决于它的实现——所核查的 codex 版本
// 只认不带前缀的事件名，因此不回带；但溯源要按"已经交到客户端手里"记，不能赌客户端不用它。
// 不在这里记溯源，那道剥离守卫对**WS 铸造的** state 就是个空操作——溯源表里查无此项，直接放行。
//
// 与 HTTP 侧口径一致：**只在确实要写给客户端时记**，没送出去的（被拦截、客户端已断连）不记，否则
// 溯源表会污染成"客户端手里有一个它其实从没见过的值"，导致后续误剥离。
func (s *OpenAIGatewayService) noteOpenAIWSCodexTurnStateDelivered(c *gin.Context, account *Account, payload []byte) {
	if s == nil || c == nil || account == nil || len(payload) == 0 {
		return
	}
	if eventType, _, _ := parseOpenAIWSEventEnvelope(payload); !isOpenAIWSTurnStateMetadataEvent(eventType) {
		return
	}
	state := openAIWSTurnStateFromEvent(payload)
	if state == "" {
		return
	}
	s.noteOpenAICodexTurnStateProvenance(c, account, state)
}

// guardOpenAICodexTurnStateEcho 出站守卫：客户端回带的 turn-state 若已知由
// 其他账号铸造则剥离，同账号或无溯源记录时保持原样。只剥离、不注入——
// /responses 路径的客户端是真实 Codex，会按自身回合语义自行回带；服务端
// 注入是 Claude 兼容桥（无法回带的客户端）的专属行为。
func (s *OpenAIGatewayService) guardOpenAICodexTurnStateEcho(c *gin.Context, account *Account, h http.Header) {
	if s == nil || h == nil || account == nil {
		return
	}
	if strings.TrimSpace(h.Get(openAICodexTurnStateHeader)) == "" {
		return
	}
	if s.openAICodexTurnStateIsForeign(c, account, h.Get(openAICodexTurnStateHeader)) {
		h.Del(openAICodexTurnStateHeader)
	}
}

// sweepOpenAICodexTurnStateOrigins 机会式清扫过期溯源记录：每 256 次写入全量遍历一轮，防止仅靠读侧
// 惰性删除导致的慢泄漏（键无上界）。
//
// **不在调用方的 goroutine 里扫**：登记发生在下行交付路径上（WS 的上游 reader、SSE 的写出点），而条目
// 现在是每个已交付 blob 一条，全表遍历的耗时随流量线性涨——占着那条 goroutine 就会挤掉终帧与 usage。
// 单飞保证同一时刻只有一轮在扫，多个写入者不会并行全表扫描。
func (s *OpenAIGatewayService) sweepOpenAICodexTurnStateOrigins() {
	if s.openaiCodexTurnStateWrites.Add(1)%256 != 0 {
		return
	}
	if !s.openaiCodexTurnStateSweeping.CompareAndSwap(false, true) {
		return
	}
	sweep := s.sweepOpenAICodexTurnStateOriginsNow
	if hook := s.openaiCodexTurnStateSweepForTest.Load(); hook != nil {
		sweep = *hook
	}
	go func() {
		defer s.openaiCodexTurnStateSweeping.Store(false)
		sweep()
	}()
}

// setTurnStateSweepForTest 换掉实际的清扫实现，用来验证"触发点不占调用方 goroutine"与单飞。
//
// 有这个接缝才测得出异步性：只断言"过期条目最终消失"的话，把 go 去掉改成同步执行照样通过，而那正是要防的
// 回归（全表遍历占住下行交付 goroutine 会挤掉终帧与 usage）。与连接池的 setClientDialerForTest 同一范式。
func (s *OpenAIGatewayService) setTurnStateSweepForTest(sweep func()) {
	if sweep == nil {
		s.openaiCodexTurnStateSweepForTest.Store(nil)
		return
	}
	s.openaiCodexTurnStateSweepForTest.Store(&sweep)
}

func (s *OpenAIGatewayService) sweepOpenAICodexTurnStateOriginsNow() {
	now := time.Now()
	s.openaiCodexTurnStateOrigins.Range(func(key, value any) bool {
		origin, ok := value.(openAICodexTurnStateOrigin)
		if !ok || (!origin.expiresAt.IsZero() && now.After(origin.expiresAt)) {
			// 条件删除，理由同守卫里那处：Range 交出旧值之后，别的 goroutine 可能已经为同一个键写了
			// 新的有效记录，无条件删就把它带走了。
			s.openaiCodexTurnStateOrigins.CompareAndDelete(key, value)
		}
		return true
	})
}
