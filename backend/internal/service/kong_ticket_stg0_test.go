//go:build unit

package service

// stg0（上游回报 model）的判据与两条路径上的处置。
//
// 这一组的重点是**两个方向都不能错**：放过一次上游明说的降智，与把一张好票判死，是同级的缺陷。

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestKongStg0VerdictThreeStates(t *testing.T) {
	const model = "gpt-6-astra"
	accept, warnings, err := KongParseStg0Accept([]string{model, "gpt-5.6-sol"},
		"gpt-5.6-sol:gpt-6-sol")
	if err != nil {
		t.Fatalf("解析白名单: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("合法配置不该有告警: %v", warnings)
	}

	cases := []struct {
		name      string
		requested string
		reported  string
		want      KongStg0Verdict
	}{
		{"完全一致", model, model, KongStg0Pass},
		{"大小写不同仍是同一个", model, "GPT-6-ASTRA", KongStg0Pass},
		// 实测形态：上游会回带日期快照的名字。按字面相等判会把它当降智，于是上游每换一次快照就
		// 把该模型的票全部作废——那是无谓拒服。
		{"日期快照后缀", model, model + "-2026-03-17", KongStg0Pass},
		{"白名单里的升级投放", "gpt-5.6-sol", "gpt-6-sol", KongStg0Pass},
		{"升级投放的快照后缀", "gpt-5.6-sol", "gpt-6-sol-2026-08-01", KongStg0Pass},
		// 生产上真实发生过的那一条：请求 astra、上游回 luna。
		{"回了另一个模型", model, "gpt-5.6-luna", KongStg0Fail},
		// 方向是单向的：sol 接受 gpt-6-sol，不等于 astra 接受它。
		{"别的模型的白名单不串用", model, "gpt-6-sol", KongStg0Fail},
		// 后缀不是数字开头 = 另一个模型，不是快照。少了这一条，纯前缀匹配会把它放过去。
		{"非数字后缀不算快照", model, model + "-mini", KongStg0Fail},
		// 没有分隔符的延长名同样不是快照。用 HasPrefix(reported, base) 判会把它当成一致，
		// 而 astral 与 astra 完全可以是两个模型。
		{"无分隔符的延长名", model, model + "l", KongStg0Fail},
		// 只有分隔符、后面空着：既不是快照也不是别的模型，但它确实不等于请求的那个。
		{"空后缀", model, model + "-", KongStg0Fail},
		{"没观测到", model, "", KongStg0Unknown},
		{"只有空白", model, "   ", KongStg0Unknown},
		// 不知道请求的是什么就判不了一致性，那是"没测出来"而不是"测出不合格"。
		{"请求模型未知", "", model, KongStg0Unknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accept.Verdict(tc.requested, tc.reported); got != tc.want {
				t.Errorf("Verdict(%q, %q) = %v, want %v", tc.requested, tc.reported, got, tc.want)
			}
		})
	}
}

// 短基名不得吞掉以它为前缀的**另一个模型**。这条单独立出来：它是纯前缀匹配最危险的失效方式
// ——门控 `gpt-6` 时把所有 `gpt-6-*` 都当成自己，整道门等于不存在。
func TestKongStg0ShortBaseDoesNotSwallowOtherModels(t *testing.T) {
	accept := KongStg0Accept{"gpt-6": nil}
	for _, reported := range []string{"gpt-6-astra", "gpt-6-luna", "gpt-6-mini"} {
		if v := accept.Verdict("gpt-6", reported); v != KongStg0Fail {
			t.Errorf("Verdict(gpt-6, %q) = %v, want Fail", reported, v)
		}
	}
	// 而快照后缀仍然要放行。
	if v := accept.Verdict("gpt-6", "gpt-6-2026-03-17"); v != KongStg0Pass {
		t.Errorf("快照后缀应当放行，实得 %v", v)
	}
}

// 白名单的取值**不校验是否在指纹校准资料里**：升级投放的目标（gpt-6-sol）压根不在里面，照
// accept_extra 那样校验会把一条正确配置当笔误忽略掉。
func TestKongStg0AcceptKeepsValuesOutsideFingerprintBank(t *testing.T) {
	accept, warnings, err := KongParseStg0Accept([]string{"gpt-5.6-sol"}, "gpt-5.6-sol:gpt-7-sol-preview")
	if err != nil {
		t.Fatalf("解析白名单: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("不在校准资料里不是笔误，不该告警: %v", warnings)
	}
	if v := accept.Verdict("gpt-5.6-sol", "gpt-7-sol-preview"); v != KongStg0Pass {
		t.Errorf("配了就该接受，实得 %v", v)
	}
}

// 键不是门控模型 → 忽略并告警（死配置）；格式错 → 报错（整条没被解析成任何东西，静默忽略会让人
// 以为配上了）。
func TestKongStg0AcceptConfigErrors(t *testing.T) {
	_, warnings, err := KongParseStg0Accept([]string{"gpt-6-astra"}, "gpt-9-nope:gpt-6-astra")
	if err != nil {
		t.Fatalf("死配置不该拒启动: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("死配置要告警一次，实得 %v", warnings)
	}
	if _, _, err := KongParseStg0Accept([]string{"gpt-6-astra"}, "缺冒号"); err == nil {
		t.Error("格式错必须报错")
	}
}

// 验票路径：注入被接受了、上游却回报别的模型 → 票判死，且**不再发后面的挑战**。
//
// 后半句是分层的全部收益：一道零成本的门已经给出结论，再烧两份长答案不会改变它。
func TestKongStg0MismatchInVerifyRejectsTicketAndSkipsRest(t *testing.T) {
	const model = kongBatchAstra
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		answers:    kongVerifyAnswers(),
		// 上游在回答里声明它给的是另一个模型。
		reportedModel: "gpt-5.6-luna",
	}
	svc, repo := kongBatchSetup(t, up, false)

	if _, err := svc.EnsureTicket(context.Background(), 1, model); err != nil {
		t.Fatalf("准备票: %v", err)
	}
	if up.challengeCall != 1 {
		t.Fatalf("stg0 判死之后不该继续发挑战，实发 %d 次", up.challengeCall)
	}
	var rejected bool
	for _, st := range repo.tickets {
		if st != nil && st.Status == KongTicketStatusRejected {
			rejected = true
		}
	}
	if !rejected {
		t.Error("上游明说给了别的模型，这张票必须作废")
	}
	// 事件要带上回报值，否则无从判断是真降智还是该往 stg0_accept 加一条。
	var found bool
	for _, ev := range repo.events {
		if ev.EventType != KongEventVerify || ev.Outcome != KongOutcomeFailure {
			continue
		}
		if ev.Detail["reason"] == "stg0_model_mismatch" && ev.Detail["reported_model"] == "gpt-5.6-luna" {
			found = true
		}
	}
	if !found {
		t.Error("stg0 判死要留一条带回报值的事件")
	}
	// **冷却必须落在结论提交之后**：`failCooldown` 会同步写一条冷却事件，那是一次数据库往返。放在最后
	// 一道前提复核与提交之间，等于在那段时间里改代理/切模式都不会再被核一次，而提交只看票状态、期限与
	// 跳过标记。用事件顺序钉住它——这是从外部唯一能观测到的执行次序。
	finalIdx, cooldownIdx := -1, -1
	for i, ev := range repo.events {
		switch {
		case ev == nil:
		case ev.EventType == KongEventVerify && ev.Detail != nil && ev.Detail["final"] == true:
			finalIdx = i
		case ev.EventType == KongEventCooldown:
			cooldownIdx = i
		}
	}
	if finalIdx < 0 || cooldownIdx < 0 {
		t.Fatalf("应当既有最终结论事件又有冷却事件，实得 final=%d cooldown=%d", finalIdx, cooldownIdx)
	}
	if cooldownIdx < finalIdx {
		t.Error("冷却写在结论提交之前：那在最后一道复核与提交之间开了一个没人再核的写库窗口")
	}
}

// 没观测到回报值时**照常走 stg1**：据没测出来的东西作废票就是无谓拒服。
func TestKongStg0UnknownDoesNotBlockVerification(t *testing.T) {
	const model = kongBatchAstra
	up := &kongStubUpstream{
		proxyState:    KongTicketProxyState{Exists: true},
		fetchState:    strings.Repeat("a", 292),
		answers:       kongVerifyAnswers(),
		reportedModel: "", // 上游没给这个字段
	}
	svc, repo := kongBatchSetup(t, up, false)

	if _, err := svc.EnsureTicket(context.Background(), 1, model); err != nil {
		t.Fatalf("准备票: %v", err)
	}
	// stg1 要跑满（这份合成答案归因不落在 astra 上，三份都发完才收尾）——关键是它**跑了**，
	// 没有被 stg0 在第一份就拦掉。
	if up.challengeCall != len(KongFingerprintChallenges()) {
		t.Errorf("stg0 未观测到时 stg1 应当照常跑完，实发 %d 次", up.challengeCall)
	}
	// 而且没有任何一条 stg0 的判死事件。
	for _, ev := range repo.events {
		if ev.Detail != nil && ev.Detail["reason"] == "stg0_model_mismatch" {
			t.Error("没观测到回报值不该产出 stg0 判死事件")
		}
	}
}

// 融合样本的 stg0 语义**与常规挑战相反**：取票那次请求本来就没带票（票是它的产物），所以上游回报
// 别的模型说的是"无票时会被降智"——那正是需要票的理由，不是刚取到的票不合格。
//
// 处置因此是丢弃这一份样本、让 leader 按常规路径重跑，**绝不能判死票**。
func TestKongStg0MismatchOnFusedDiscardsSampleNotTicket(t *testing.T) {
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		fetchState: strings.Repeat("a", 292),
		answers:    kongVerifyAnswers(),
		fusedText:  kongVerifyAnswers()[0].Text,
		// 取票那次（无票状态）被降智，而带票的常规挑战正常。
		fusedReportedModel: "gpt-5.6-luna",
	}
	svc, repo := kongFusedSetup(t, up, false)

	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol); err != nil {
		t.Fatalf("取票: %v", err)
	}
	// 票不该被这一份样本判死。
	for _, st := range repo.tickets {
		if st != nil && st.Status == KongTicketStatusRejected {
			t.Fatal("融合样本的 stg0 不合格不能作废票——那次请求本来就没带票")
		}
	}
	// 而这一份样本要留档、标明丢弃原因、且不计入平均。
	var discarded bool
	for _, p := range repo.probes {
		if p != nil && p.Fused && p.DiscardedReason == "fused_stg0_mismatch" {
			discarded = true
			if p.CountedInAverage {
				t.Error("被丢弃的样本必须不计入平均，否则离线重算会把它混回去")
			}
		}
	}
	if !discarded {
		t.Error("融合样本要留档并标出 fused_stg0_mismatch")
	}
	// leader 有请求在等，值得按常规路径重跑（带票、走流量出口）。
	if up.challengeCall == 0 {
		t.Error("丢弃融合样本后应当走常规挑战")
	}
}

// 断流不得绕过 stg0。
//
// 上游只要「先在 response.created 里声明别的模型、然后把流掐断」，就能让这一份挑战走进 runErr
// 分支；若 stg0 排在它之后，这一份连带已经拿到的回报值一起被跳过，后两份正常作答、stg1 放行
// ——**净效果是放行降智**。回报值在流首就到了，它的结论不因正文残缺而失效。
func TestKongStg0MismatchSurvivesStreamError(t *testing.T) {
	for _, readErr := range []string{"unexpected EOF", "SSE 事件解码失败", "上游报告 response.failed"} {
		t.Run(readErr, func(t *testing.T) {
			up := &kongStubUpstream{
				proxyState:       KongTicketProxyState{Exists: true},
				fetchState:       strings.Repeat("a", 292),
				answers:          kongVerifyAnswers(),
				reportedModel:    "gpt-5.6-luna",
				partialAnswerErr: errors.New(readErr),
			}
			svc, repo := kongBatchSetup(t, up, false)

			if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchAstra); err != nil {
				t.Fatalf("准备票: %v", err)
			}
			if up.challengeCall != 1 {
				t.Errorf("stg0 已判死，不该继续发挑战，实发 %d 次", up.challengeCall)
			}
			var rejected bool
			for _, st := range repo.tickets {
				if st != nil && st.Status == KongTicketStatusRejected {
					rejected = true
				}
			}
			if !rejected {
				t.Error("上游明说给了别的模型，票必须作废——断流不能成为绕过它的方式")
			}
		})
	}
}

// 没观测到 model 的读流失败仍按原策略处理（继续下一份挑战），不因为上面那条改动而变成拒票。
func TestKongStreamErrorWithoutModelKeepsRetrying(t *testing.T) {
	up := &kongStubUpstream{
		proxyState:       KongTicketProxyState{Exists: true},
		fetchState:       strings.Repeat("a", 292),
		answers:          kongVerifyAnswers(),
		reportedModel:    "", // 上游没给这个字段
		partialAnswerErr: errors.New("unexpected EOF"),
	}
	svc, _ := kongBatchSetup(t, up, false)

	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchAstra); err != nil {
		t.Fatalf("准备票: %v", err)
	}
	if up.challengeCall != len(KongFingerprintChallenges()) {
		t.Errorf("没测出 model 的读流失败应当照常重试，实发 %d 次", up.challengeCall)
	}
}

// 验证期间前提变了（换了流量代理），stg0 的结论必须作废——它测的是已经不存在的环境。
//
// CommitVerification 的状态/期限条件挡不住这个：它看不到账号的 mode、调度资格与出口配置。
func TestKongStg0RejectHonorsPreconditionRecheck(t *testing.T) {
	const model = kongBatchAstra
	// 两个时点都要挡住：挑战发出之后、以及**探测落库期间**。落库要走一次数据库往返，那段时间足够
	// 人工改配置；只在保存之前复核，等于拿一个「保存开始时成立」的前提去作废票。
	for _, when := range []string{"挑战期间换代理", "落库期间换代理"} {
		t.Run(when, func(t *testing.T) {
			up := &kongStubUpstream{
				proxyState:    KongTicketProxyState{Exists: true},
				fetchState:    strings.Repeat("a", 292),
				answers:       kongVerifyAnswers(),
				reportedModel: "gpt-5.6-luna",
			}
			repo := newKongStubRepo()
			idleSince := time.Now().Add(-40 * time.Minute)
			repo.lastEgressUsed = &idleSince
			accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
			account := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
			proxyID := int64(100)
			account.ProxyID = &proxyID
			accounts.set(account)
			svc := kongBatchServiceWith(t, repo, up, accounts, false, false)
			moveProxy := func() {
				moved := kongTestAccount(1, KongTicketModeFull, KongTicketEgressDirect)
				other := int64(101)
				moved.ProxyID = &other
				accounts.set(moved)
			}
			if when == "挑战期间换代理" {
				svc.upstream = &kongChallengeHook{kongStubUpstream: up, before: moveProxy}
			} else {
				repo.insertProbesHook = moveProxy
			}

			if _, err := svc.EnsureTicket(context.Background(), 1, model); err != nil {
				t.Fatalf("准备票: %v", err)
			}
			for _, st := range repo.tickets {
				if st != nil && st.Status == KongTicketStatusRejected {
					t.Fatal("前提已失效，stg0 不得据旧环境的结论判死票")
				}
			}
			// 不得推进冷却、不得淘汰同期候选：这一次什么都没测出来。
			if len(repo.skippedBulk) > 0 {
				t.Errorf("前提失效不该淘汰候选，实得 %v", repo.skippedBulk)
			}
			for _, ev := range repo.events {
				if ev != nil && ev.EventType == KongEventCooldown {
					t.Error("前提失效不该推进取票冷却")
				}
			}
			// 该留一条"没测出来"的记录，而不是静默丢弃。
			var inconclusive bool
			for _, ev := range repo.events {
				if ev.EventType == KongEventVerify && ev.Outcome == KongOutcomeInconclusive {
					if ev.Detail["stg"] == 0 || ev.Detail["reported_model"] == "gpt-5.6-luna" {
						inconclusive = true
					}
				}
			}
			if !inconclusive {
				t.Error("前提失效要留一条带回报值的 inconclusive")
			}
			// 观测只能入库一遍：落库之后的那条出口若走 failBeforeConclusion 会再存一次，同一份
			// 探测在真实仓储（普通 INSERT，不去重）里就成了两行。
			if n := len(repo.probes); n != 1 {
				t.Errorf("探测记录数 = %d, want 1（不得重复入库）", n)
			}
		})
	}
}

// 融合读正文失败时，**回报的 model 要带回来并落进事件**（它在流首就到了），但残缺正文不得被送去归因。
//
// 只断言"样本被丢弃"是不够的：上一版从读取层到编排层的传递断在中间，事件里的 reported_model 一直是
// 空的，而那条由上游自己给出的声明是这次调用唯一留下的判据——没有它，事后分不清真降智与"该往
// stg0_accept 加一条"。leader 与批内非触发模型两处构造都要覆盖。
func TestKongFusedReadFailureStillCarriesReportedModel(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "leader"
		if batch {
			name = "批内非触发模型"
		}
		t.Run(name, func(t *testing.T) {
			up := &kongStubUpstream{
				proxyState:         KongTicketProxyState{Exists: true},
				fetchState:         strings.Repeat("a", 292),
				answers:            kongVerifyAnswers(),
				fusedReadErr:       "unexpected EOF",
				fusedReportedModel: "gpt-5.6-luna",
			}
			svc, repo := kongFusedSetup(t, up, batch)

			if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchSol); err != nil {
				t.Fatalf("取票: %v", err)
			}
			// 票照常入库：取票花的是一整段出口静默，读正文失败不能连票一起丢。
			if len(repo.inserted) == 0 {
				t.Fatal("读正文失败不该让票丢掉")
			}
			// 融合样本留档并标出丢弃原因，且不计入平均。
			var carried bool
			for _, p := range repo.probes {
				if p != nil && p.Fused && p.DiscardedReason != "" {
					carried = true
					if p.CountedInAverage {
						t.Error("被丢弃的样本不得计入平均")
					}
				}
			}
			if !carried {
				t.Error("融合样本要留档并标出丢弃原因")
			}
			// 回报值必须能从**持久化的事件**里读回来。
			var reported bool
			for _, ev := range repo.events {
				if ev == nil || ev.EventType != KongEventVerify || ev.Detail == nil {
					continue
				}
				if ev.Detail["fused"] == true && ev.Detail["reported_model"] == "gpt-5.6-luna" {
					reported = true
				}
			}
			if !reported {
				t.Error("融合失败事件里读不回上游回报的 model")
			}
		})
	}
}

// 零 mismatch 是最常见的情况，它的序列化结果必须是 `[]` 而不是 `null`——前端按数组读它。
func TestKongStg0StatsSerializesEmptyListAsArray(t *testing.T) {
	stats := &KongStg0Stats{
		AccountID: 1, Model: kongBatchAstra, Total: 11550,
		TopReported: []KongStg0Reported{},
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if !strings.Contains(string(encoded), `"top_reported":[]`) {
		t.Errorf("空列表要序列化成 []，实得 %s", encoded)
	}
	// 反面：留成 nil 就会变 null，那会让前端读 .length 时打崩整个页面。
	nilled := &KongStg0Stats{AccountID: 1, Model: kongBatchAstra, Total: 1}
	encoded, err = json.Marshal(nilled)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if !strings.Contains(string(encoded), `"top_reported":null`) {
		t.Errorf("这条断言在提醒：nil 切片确实会变 null，所以仓储必须显式给空切片，实得 %s", encoded)
	}
}

// 取到降智档票（312，长度黑名单）是**按规则拒收**，不是存储故障。
//
// 两者都要推进退避（网络活动已发生，而且上游正在发 312，立刻重试只会再拿一张），但归因必须分开：
// 记成 store_ticket_failed 会让排查往"数据库坏了"的方向走，而真实原因是上游给了什么。
//
// observe 路径一直用 KongIsExpectedTicketRejection 做这个区分，fetch 路径此前没做——同一件事在两条
// 路径上归因不一致。
func TestKongDenylistedTicketIsRejectionNotStoreFailure(t *testing.T) {
	up := &kongStubUpstream{
		proxyState: KongTicketProxyState{Exists: true},
		// 312 在长度黑名单里。
		fetchState: strings.Repeat("a", 312),
		answers:    kongVerifyAnswers(),
	}
	svc, repo := kongBatchSetup(t, up, false)

	if _, err := svc.EnsureTicket(context.Background(), 1, kongBatchAstra); err == nil {
		// 拿不到票是预期的；这里只要求它别静默成功。
		t.Log("取票未返回错误，继续检查事件归因")
	}

	var cooldownReasons []string
	for _, ev := range repo.events {
		if ev.EventType == KongEventCooldown && ev.Detail != nil {
			if r, ok := ev.Detail["reason"].(string); ok {
				cooldownReasons = append(cooldownReasons, r)
			}
		}
	}
	if len(cooldownReasons) == 0 {
		t.Fatal("取到 312 之后必须推进退避，否则下一个请求立刻再取一张 312")
	}
	for _, r := range cooldownReasons {
		if r == "store_ticket_failed" {
			t.Error("按规则拒收被记成存储故障——那会让排查方向指向数据库，而真实原因是上游给了降智档票")
		}
		if r != "ticket_rejected" {
			t.Errorf("冷却原因 = %q, want ticket_rejected", r)
		}
	}
	// 长度黑名单那条 observe 事件照旧要有：它带着 state_len，是"上游给了什么"的直接证据。
	var sawDenylist bool
	for _, ev := range repo.events {
		if ev.Detail != nil && ev.Detail["reason"] == "state_len_denylisted" {
			sawDenylist = true
			if ev.StateLen == nil || *ev.StateLen != 312 {
				t.Error("那条事件要带上票长度，否则看不出上游给的是哪一档")
			}
		}
	}
	if !sawDenylist {
		t.Error("缺少 state_len_denylisted 事件")
	}
}

// 详情页要能区分"结论来自哪一层"：stg0 是上游自己回报的 model，不是一次指纹测量。
//
// stg0 判死票时会把回报值当归因结果写进票行（p=1、单点分布），那是为了让下游统一按概率工作。但页面
// 照 p 显示就成了"指纹归因，置信度 1.00"——把一句声明呈现成确定性测量。来源取自该票已提交的最终验证
// 事件，**不按 p == 1 猜**：stg1 的概率也可以恰好是 1。
func TestKongTicketDetailDistinguishesStg0Conclusion(t *testing.T) {
	const model = kongBatchAstra
	repo, _, svc := kongManualFixture(t, model)
	now := time.Now()
	stg0Ticket := kongSeedCurrentTicket(repo, model, now)
	stg1Ticket := kongSeedCandidate(repo, model, 21, 5*time.Minute)
	noEventTicket := kongSeedCandidate(repo, model, 22, 6*time.Minute)

	// 已提交归因的最终事件：两条提交路径都会填 FingerprintModel（stg0 填回报值、stg1 填 argmax）。
	final := func(ticketID int64, at time.Time, fingerprint string, detail map[string]any) *KongTicketEvent {
		detail["final"] = true
		ev := &KongTicketEvent{
			AccountID: 1, Model: model, EventType: KongEventVerify,
			Outcome: KongOutcomeFailure, TicketID: &ticketID, CreatedAt: at, Detail: detail,
		}
		if fingerprint != "" {
			ev.FingerprintModel = kongStrPtr(fingerprint)
		}
		return ev
	}
	ctx := context.Background()
	// 同一张票先有一条 stg1 结论、后被重验为 stg0：取最新那条，顺序由事件时间决定。
	if err := repo.InsertEvent(ctx, final(stg0Ticket.ID, now.Add(-10*time.Minute), model,
		map[string]any{"reason": "not_target_model", "probability": 0.42})); err != nil {
		t.Fatalf("写事件: %v", err)
	}
	if err := repo.InsertEvent(ctx, final(stg0Ticket.ID, now.Add(-5*time.Minute), "gpt-5.6-luna",
		map[string]any{"reason": "stg0_model_mismatch", "stg": float64(0), "reported_model": "gpt-5.6-luna"})); err != nil {
		t.Fatalf("写事件: %v", err)
	}
	// **之后的人工重验以 inconclusive 收尾**：那也是一条最终事件，但它没有提交归因，票行仍挂着 stg0
	// 的回报值。据它改写来源会让页面把这张票重新显示成"指纹归因 p=1.00"。
	if err := repo.InsertEvent(ctx, final(stg0Ticket.ID, now.Add(-time.Minute), "",
		map[string]any{"reason": "no_valid_answer"})); err != nil {
		t.Fatalf("写事件: %v", err)
	}
	if err := repo.InsertEvent(ctx, final(stg1Ticket.ID, now.Add(-2*time.Minute), model,
		map[string]any{"reason": "not_target_model", "probability": 1.0})); err != nil {
		t.Fatalf("写事件: %v", err)
	}
	// **一张票被反复重验**：已提交的那条结论之后又追加了四条未提交归因的最终事件。按"先取一批再在应用层
	// 筛"的做法，那批里一条已提交的都不剩，来源于是丢失、页面落回概率展示分支。
	for i := 0; i < 4; i++ {
		if err := repo.InsertEvent(ctx, final(stg0Ticket.ID, now.Add(time.Duration(i)*time.Second), "",
			map[string]any{"reason": "no_valid_answer"})); err != nil {
			t.Fatalf("写事件: %v", err)
		}
	}
	// 非最终事件不得参与判定：它是"某一份挑战失败"，不是本次验证的结论。
	nonFinal := &KongTicketEvent{
		AccountID: 1, Model: model, EventType: KongEventVerify, Outcome: KongOutcomeInconclusive,
		TicketID: &noEventTicket.ID, CreatedAt: now,
		Detail: map[string]any{"reason": "fused_unreadable", "stg": float64(0)},
	}
	if err := repo.InsertEvent(ctx, nonFinal); err != nil {
		t.Fatalf("写事件: %v", err)
	}

	views := &kongStubAdminAccounts{views: []KongAccountView{
		{ID: 1, Ready: true, Extra: map[string]any{KongTicketModeKey: string(KongTicketModeFull)}},
	}}
	admin := NewKongTicketAdminService(repo, views, KongDefaultTicketParams(),
		[]string{model}, svc.accept, svc.stg0, svc.confidence)
	admin.SetTicketService(svc)

	page, err := admin.TicketDetail(ctx, 1)
	if err != nil {
		t.Fatalf("票据详情: %v", err)
	}
	// 解引用成可读的值：`%v` 打指针只会给出地址，排查时看不出实际来源。
	got := map[int64]string{}
	for _, row := range page.Tickets {
		if row.Stg == nil {
			got[row.ID] = "未知"
			continue
		}
		got[row.ID] = strconv.Itoa(*row.Stg)
	}
	if got[stg0Ticket.ID] != "0" {
		t.Errorf("#%d 的结论来自 stg0（之后那条 inconclusive 没提交归因，不该改写来源），实得 %s",
			stg0Ticket.ID, got[stg0Ticket.ID])
	}
	// 概率恰好是 1 的 stg1 结论不能被当成 stg0——这正是"不按 p 猜"的理由。
	if got[stg1Ticket.ID] != "1" {
		t.Errorf("#%d 的结论来自 stg1（概率恰好 1.0），实得 %s", stg1Ticket.ID, got[stg1Ticket.ID])
	}
	// 只有非最终事件的票仍是"来源未知"：那条事件不是结论。
	if got[noEventTicket.ID] != "未知" {
		t.Errorf("#%d 没有已提交的结论，来源应当未知，实得 %s", noEventTicket.ID, got[noEventTicket.ID])
	}
}

// 取样行回答的是「现在还在采什么样」，判据必须覆盖取样的**每一个**出口。
//
// 线上现象：账号连续几天只能取到 312（被长度黑名单挡在验证之前，写 observe/skipped），之后取到一张
// 292 并进了验证流。那次取票只写 fetch 事件，于是这一行仍停在几天前那条 312 上——它声称"现在被黑
// 名单挡着、没在采样"，而实际此刻采到的是 292 且正在验证。这一行的全部用途就是这个信号，信号本身
// 反了比没有更糟。
func TestKongLastSampleReflectsLatestFetch(t *testing.T) {
	const model = kongBatchAstra
	repo, _, svc := kongManualFixture(t, model)
	now := time.Now()
	ticket := kongSeedCurrentTicket(repo, model, now)
	ctx := context.Background()

	denylisted := 312
	accepted := 292
	events := []*KongTicketEvent{
		// 昨天：收到 312，被长度黑名单挡在验证之前。
		{
			AccountID: 1, Model: model, EventType: KongEventObserve, Outcome: KongOutcomeSkipped,
			StateLen: &denylisted, CreatedAt: now.Add(-12 * time.Hour),
			Detail: map[string]any{"reason": "state_len_denylisted"},
		},
		// 此刻：取到 292，直接进验证流——这一步只写 fetch。
		{
			AccountID: 1, Model: model, EventType: KongEventFetch, Outcome: KongOutcomeSuccess,
			StateLen: &accepted, TicketID: &ticket.ID, CreatedAt: now.Add(-time.Minute),
			Detail: map[string]any{},
		},
		// 验证事件不是取样：它属于结论侧（诊断行），不该顶替取样行。
		{
			AccountID: 1, Model: model, EventType: KongEventVerify, Outcome: KongOutcomeFailure,
			TicketID: &ticket.ID, CreatedAt: now,
			Detail: map[string]any{"reason": "candidate_not_accepted", "final": true},
		},
	}
	for _, ev := range events {
		if err := repo.InsertEvent(ctx, ev); err != nil {
			t.Fatalf("写事件: %v", err)
		}
	}

	views := &kongStubAdminAccounts{views: []KongAccountView{
		{ID: 1, Ready: true, Extra: map[string]any{KongTicketModeKey: string(KongTicketModeFull)}},
	}}
	admin := NewKongTicketAdminService(repo, views, KongDefaultTicketParams(),
		[]string{model}, svc.accept, svc.stg0, svc.confidence)
	admin.SetTicketService(svc)

	status, err := admin.statusOf(ctx, &views.views[0], now)
	if err != nil {
		t.Fatalf("查账号状态: %v", err)
	}
	if len(status.Models) != 1 {
		t.Fatalf("模型数 = %d, want 1", len(status.Models))
	}
	sample := status.Models[0].LastSample
	if sample == nil {
		t.Fatal("取样行不该为空：刚取到过票")
	}
	// 解引用再打印：`%v` 打指针只会给出地址，看不出实际停在哪一张票上。
	gotLen := "无"
	if sample.StateLen != nil {
		gotLen = strconv.Itoa(*sample.StateLen)
	}
	if gotLen != strconv.Itoa(accepted) {
		t.Errorf("取样长度应当是最近一次取到的 %d，实得 %s——页面会显示成仍被黑名单挡着",
			accepted, gotLen)
	}
	if sample.Reason == "state_len_denylisted" {
		t.Error("取样原因仍是长度黑名单，那是十二小时前那张票的处置")
	}
	if sample.Outcome != KongOutcomeSuccess {
		t.Errorf("取样结果 = %q, want %q", sample.Outcome, KongOutcomeSuccess)
	}
}

// 内置默认的 stg0 白名单：格式必须能解析，方向必须只放开"升级投放"。
//
// 这张表每加一项都要能说清"为什么这个替换不算降智"。默认收 `gpt-5.6-sol → gpt-6-sol`：上游把更新的
// 模型投到旧模型名上，拿到的是更好的东西，判死票等于为了一次升级白搭一整段出口静默。反方向（回报
// 更旧或更小的模型）绝不能被这张表放过——那正是要拦的降智。
func TestKongStg0DefaultAcceptOnlyAllowsUpgrade(t *testing.T) {
	gated := []string{"gpt-5.6-sol", "gpt-6-astra"}
	accept, warnings, err := KongParseStg0Accept(gated, config.DefaultKongTicketStg0Accept)
	if err != nil {
		t.Fatalf("内置默认必须能解析：%v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("内置默认不该产生告警：%v", warnings)
	}

	for _, tc := range []struct {
		name      string
		requested string
		reported  string
		want      KongStg0Verdict
	}{
		{"升级投放被接受", "gpt-5.6-sol", "gpt-6-sol", KongStg0Pass},
		{"升级投放带日期快照", "gpt-5.6-sol", "gpt-6-sol-2026-03-17", KongStg0Pass},
		// 下面几条是这张表**不能**放过的：都是上游明说给了别的（更旧或更小）模型。
		{"降到 5.5 仍判死", "gpt-5.6-sol", "gpt-5.5", KongStg0Fail},
		{"降到 mini 仍判死", "gpt-5.6-sol", "gpt-5-mini", KongStg0Fail},
		// 白名单是逐模型的：给 sol 开的项不该顺带放宽 astra。
		{"astra 收到 sol 仍判死", "gpt-6-astra", "gpt-6-sol", KongStg0Fail},
		{"astra 收到 5.6-sol 仍判死", "gpt-6-astra", "gpt-5.6-sol", KongStg0Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accept.Verdict(tc.requested, tc.reported); got != tc.want {
				t.Errorf("Verdict(%q, %q) = %v, want %v", tc.requested, tc.reported, got, tc.want)
			}
		})
	}
}

// 统计侧按白名单拆分的三条不变量：两份相加等于 Mismatch、判据与验票同源（快照后缀也算接受）、
// 未列进明细的部分归入未接受。
//
// 拆分错了不会报错，只会让页面长期标红或长期不标红——前者把这块读数变成背景噪音，后者放过降智。
func TestKongStg0ClassifyStatsSplitsByAcceptList(t *testing.T) {
	accept, warnings, err := KongParseStg0Accept([]string{"gpt-5.6-sol"}, "gpt-5.6-sol:gpt-6-sol")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("解析白名单: err=%v warnings=%v", err, warnings)
	}

	t.Run("白名单内与快照后缀都算已接受", func(t *testing.T) {
		stats := &KongStg0Stats{
			Model: "gpt-5.6-sol", Total: 700, Mismatch: 140,
			TopReported: []KongStg0Reported{
				{Model: "gpt-6-sol", Count: 132},
				// 上游换快照：审计口径把它记成不一致，而验票的 Verdict 认它是同一个模型。
				{Model: "gpt-5.6-sol-2026-09-20", Count: 6},
				{Model: "gpt-5.5", Count: 2},
			},
		}
		accept.ClassifyStats(stats)
		if stats.MismatchAccepted != 138 || stats.MismatchUnaccepted != 2 {
			t.Errorf("已接受 138 / 未接受 2，实得 %d / %d", stats.MismatchAccepted, stats.MismatchUnaccepted)
		}
		if stats.MismatchAccepted+stats.MismatchUnaccepted != stats.Mismatch {
			t.Errorf("两份相加必须等于 Mismatch=%d，实得 %d", stats.Mismatch,
				stats.MismatchAccepted+stats.MismatchUnaccepted)
		}
		// 未接受的排最前：它是唯一要人动手的那条，排在 132 次的已接受投放后面就会被翻页埋掉。
		if stats.TopReported[0].Model != "gpt-5.5" || stats.TopReported[0].Accepted {
			t.Errorf("未接受的回报值要排第一且标记为未接受，实得 %+v", stats.TopReported[0])
		}
	})

	t.Run("明细没覆盖到的部分归入未接受", func(t *testing.T) {
		// 回报值种类超过仓储上限时，剩余的量只有汇总里有。它是未知——按已接受处理会把一次真正的
		// 降智显示成中性色。
		stats := &KongStg0Stats{
			Model: "gpt-5.6-sol", Total: 100, Mismatch: 50,
			TopReported: []KongStg0Reported{{Model: "gpt-6-sol", Count: 30}},
		}
		accept.ClassifyStats(stats)
		if stats.MismatchAccepted != 30 || stats.MismatchUnaccepted != 20 {
			t.Errorf("已接受 30 / 未接受 20，实得 %d / %d", stats.MismatchAccepted, stats.MismatchUnaccepted)
		}
	})

	t.Run("明细多于汇总时不产生负数", func(t *testing.T) {
		// 汇总与明细是两次查询，之间可能又落进新行。负值会让页面算出负百分比。
		stats := &KongStg0Stats{
			Model: "gpt-5.6-sol", Total: 10, Mismatch: 3,
			TopReported: []KongStg0Reported{{Model: "gpt-6-sol", Count: 5}},
		}
		accept.ClassifyStats(stats)
		if stats.MismatchAccepted != 3 || stats.MismatchUnaccepted != 0 {
			t.Errorf("夹到 Mismatch=3 且未接受为 0，实得 %d / %d",
				stats.MismatchAccepted, stats.MismatchUnaccepted)
		}
	})

	t.Run("截断发生在排序之后", func(t *testing.T) {
		stats := &KongStg0Stats{Model: "gpt-5.6-sol", Total: 1000, Mismatch: 606}
		for i := 0; i < 6; i++ {
			stats.TopReported = append(stats.TopReported,
				KongStg0Reported{Model: "gpt-6-sol-2026-0" + strconv.Itoa(i+1) + "-01", Count: 100})
		}
		stats.TopReported = append(stats.TopReported, KongStg0Reported{Model: "gpt-5.5", Count: 6})
		accept.ClassifyStats(stats)
		if len(stats.TopReported) != kongStg0TopReportedMax {
			t.Fatalf("要截断到 %d 条，实得 %d", kongStg0TopReportedMax, len(stats.TopReported))
		}
		if stats.TopReported[0].Model != "gpt-5.5" {
			t.Errorf("未接受的那条不能被高频的已接受投放挤掉，实得 %+v", stats.TopReported)
		}
		if stats.MismatchUnaccepted != 6 {
			t.Errorf("未接受 6，实得 %d", stats.MismatchUnaccepted)
		}
	})
}

// 空白名单（未配置 stg0_accept、或功能未启用时的 nil 表）下，一切不一致都是未接受——默认极性是拒绝。
func TestKongStg0ClassifyStatsDefaultsToUnaccepted(t *testing.T) {
	for name, accept := range map[string]KongStg0Accept{"nil 表": nil, "空表": {}} {
		t.Run(name, func(t *testing.T) {
			stats := &KongStg0Stats{
				Model: "gpt-5.6-sol", Total: 10, Mismatch: 4,
				TopReported: []KongStg0Reported{{Model: "gpt-6-sol", Count: 4}},
			}
			accept.ClassifyStats(stats)
			if stats.MismatchAccepted != 0 || stats.MismatchUnaccepted != 4 {
				t.Errorf("没配白名单时全部算未接受，实得 %d / %d",
					stats.MismatchAccepted, stats.MismatchUnaccepted)
			}
			if stats.TopReported[0].Accepted {
				t.Error("没配白名单时回报值不得标成已接受")
			}
		})
	}
}

// 概览页拿到的统计必须已经按白名单拆过。
//
// 这条覆盖的是「规则只落在部分落点」那类缺陷：分类逻辑本身有测试，但管理服务没拿到白名单（装配漏传、
// 或某个挂统计的入口绕过了 stg0Index）时，页面会把每一次已接受的投放重新标成降智——那是无谓告警，
// 与放过降智同级。
func TestKongAdminOverviewClassifiesStg0ByAcceptList(t *testing.T) {
	now := time.Now()
	model := "gpt-5.6-sol"
	repo := newKongStubRepo()
	repo.stg0Stats = []*KongStg0Stats{{
		AccountID: 1, Model: model, Total: 691, Mismatch: 132,
		TopReported: []KongStg0Reported{{Model: "gpt-6-sol", Count: 132}},
	}}
	accounts := &kongStubAdminAccounts{views: []KongAccountView{{
		ID: 1, Name: "acc", Platform: PlatformOpenAI, Ready: true,
		Extra: map[string]any{KongTicketModeKey: string(KongTicketModeOff)},
	}}}
	stg0, _, err := KongParseStg0Accept([]string{model}, model+":gpt-6-sol")
	if err != nil {
		t.Fatalf("解析白名单: %v", err)
	}
	admin := NewKongTicketAdminService(repo, accounts, KongDefaultTicketParams(),
		[]string{model}, KongTicketAccept{}, stg0, 0.9)

	list, err := admin.Overview(context.Background(), now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	got := list[0].Models[0].Stg0
	if got == nil {
		t.Fatal("统计没挂到这一行上")
	}
	if got.MismatchAccepted != 132 || got.MismatchUnaccepted != 0 {
		t.Errorf("132 次投放全在白名单内：已接受 132 / 未接受 0，实得 %d / %d",
			got.MismatchAccepted, got.MismatchUnaccepted)
	}
	if len(got.TopReported) != 1 || !got.TopReported[0].Accepted {
		t.Errorf("回报值要标成已接受，实得 %+v", got.TopReported)
	}

	// 白名单当前内容也要能被页面读到——看不到它，运维无法判断一行红字是没配上还是口径不同。
	view, fingerprint := admin.AcceptView()
	if len(view[model]) != 1 || view[model][0] != "gpt-6-sol" {
		t.Errorf("stg0 白名单视图应含 %s → gpt-6-sol，实得 %v", model, view)
	}
	if len(fingerprint) != 0 {
		t.Errorf("本例没配归因白名单，视图应为空，实得 %v", fingerprint)
	}
	// 视图是快照：改它不得影响判定。
	view[model][0] = "tampered"
	again, _ := admin.AcceptView()
	if again[model][0] != "gpt-6-sol" {
		t.Error("AcceptView 必须返回拷贝，否则调用方能改掉生效中的白名单")
	}
}
