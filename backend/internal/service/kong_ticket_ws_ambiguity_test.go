//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// WS 帧的顶层歧义必须与 HTTP 侧同源判定。
//
// gjson 取第一个重复键、encoding/json 取最后一个，上游按哪个解释我们不知道。WS 上这个分歧比
// HTTP 更宽：它还能改变**帧分类**，`type` 被读成别的事件类型时整条准入根本不会被触发。
func TestKongWSFrameAmbiguity(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"正常帧", `{"type":"response.create","model":"gpt-6-astra"}`, ""},
		{"重复 model", `{"type":"response.create","model":"gpt-5.6-sol","model":"gpt-6-astra"}`, "duplicate_key:model"},
		{"重复 type", `{"type":"session.update","type":"response.create","model":"gpt-6-astra"}`, "duplicate_key:type"},
		{"重复 client_metadata", `{"type":"response.create","client_metadata":{"a":1},"client_metadata":{"b":2}}`, "duplicate_key:client_metadata"},
		// 与判定无关的重复键不拦：那不会改变任何一步的结果，拦它只会误伤业务。
		{"无关键重复", `{"type":"response.create","foo":1,"foo":2}`, ""},
		// 非 JSON 与非对象：上游拿到同一份字节也读不出字段，构不成分歧。passthrough 允许的
		// 二进制帧正落在这里，不能被误拒。
		{"非 JSON", "\x00\x01\x02binary", ""},
		{"顶层是数组", `[{"type":"response.create"}]`, ""},
		{"截断", `{"type":"response.create"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kongWSFrameAmbiguity([]byte(tc.payload)); got != tc.want {
				t.Errorf("kongWSFrameAmbiguity = %q, want %q", got, tc.want)
			}
		})
	}
}

// 歧义帧对受保护账号拒服，对其余账号保持原业务语义。
func TestKongGuardWSFrameByAccount(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	full := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	observe := kongTestAccount(2, KongTicketModeObserve, KongTicketEgressDirect)
	accounts.set(full)
	accounts.set(observe)
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)
	g := NewKongTicketGateway(svc, []string{"gpt-6-astra"})

	ambiguous := []byte(`{"type":"response.create","model":"gpt-5.6-sol","model":"gpt-6-astra"}`)
	if err := g.GuardWSFrame(full, ambiguous); !KongIsTicketDenied(err) {
		t.Errorf("受保护账号的歧义帧必须拒服，得到 %v", err)
	}
	if err := g.GuardWSFrame(observe, ambiguous); err != nil {
		t.Errorf("observe 账号照常服务，得到 %v", err)
	}
	if err := g.GuardWSFrame(full, []byte(`{"type":"response.create","model":"gpt-6-astra"}`)); err != nil {
		t.Errorf("正常帧不该被拒：%v", err)
	}
}

// 注入之后必须确认实际出站字节里的票**唯一**且就是我们给的那张。
//
// sjson 只改第一处：客户端自带票据键时，写完可能仍留着它那一份，而按末键语义解码出来的是客户端
// 的票——attempt 却宣称注入了合格票，后置守卫也就永远不会报错。
func TestKongPrepareWSTurnRejectsUnverifiableInjection(t *testing.T) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	proxyID := int64(100)
	account.ProxyID = &proxyID
	accounts.set(account)
	state := strings.Repeat("a", 292)
	repo.current[kongStubKey(1, "gpt-6-astra")] = &KongTicket{
		ID: 7, AccountID: 1, Model: "gpt-6-astra", State: state,
		Status: KongTicketStatusVerified, ExpiresAt: time.Now().Add(time.Hour),
		// 归因结果要齐：当前票的查询条件里带着当前目标与阈值，缺了它就不是一张可用的票。
		FingerprintModel: kongStrPtr("gpt-6-astra"), FingerprintP: kongFloatPtr(0.99),
	}
	svc := kongTestService(t, repo, &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}}, accounts)
	g := NewKongTicketGateway(svc, []string{"gpt-6-astra"})

	// 客户端在同一个 metadata 对象里塞了两个票据键：sjson 改掉第一个，第二个仍是客户端的。
	dup := []byte(`{"type":"response.create","model":"gpt-6-astra","client_metadata":{"` +
		kongWSTurnStateMetadataKey + `":"x","` + kongWSTurnStateMetadataKey + `":"y"}}`)
	if _, attempt, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-astra", dup); !KongIsTicketDenied(err) {
		t.Errorf("票不唯一时必须拒服，得到 attempt=%v err=%v", attempt, err)
	}

	// 客户端自带一个票据键、只有一份时，覆盖后是唯一且等于我们的票，应当放行。
	single := []byte(`{"type":"response.create","model":"gpt-6-astra","client_metadata":{"` +
		kongWSTurnStateMetadataKey + `":"client-own"}}`)
	next, attempt, err := g.PrepareWSTurn(context.Background(), account, "gpt-6-astra", single)
	if err != nil || attempt == nil || attempt.Grant == nil {
		t.Fatalf("覆盖客户端自带票应当成功：attempt=%v err=%v", attempt, err)
	}
	if got := gjson.GetBytes(next, "client_metadata."+kongWSTurnStateMetadataKey).String(); got != state {
		t.Errorf("注入后的票 = %q, want %q", got, state)
	}
}

// 票据错误不得计入账号故障统计：本地拒服与交付拦截都不是上游健康失败。
func TestKongTicketDenialIsNotAccountFailure(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
	for _, err := range []error{
		&KongErrTicketDenied{Reason: "window_closed"},
		&KongErrDeliveryBlocked{AccountID: 1, Model: "gpt-6-astra"},
		wrapOpenAIWSKongTicketError(&KongErrTicketDenied{Reason: "window_closed"}),
	} {
		if tripped := svc.ReportOpenAIAccountScheduleResult(account, "gpt-6-astra", false, nil, err); tripped {
			t.Errorf("票据错误 %T 不该触发账号健康降级", err)
		}
	}
}
