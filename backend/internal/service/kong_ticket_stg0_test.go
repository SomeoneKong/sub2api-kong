//go:build unit

package service

// stg0（上游回报 model）的判据与两条路径上的处置。
//
// 这一组的重点是**两个方向都不能错**：放过一次上游明说的降智，与把一张好票判死，是同级的缺陷。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
