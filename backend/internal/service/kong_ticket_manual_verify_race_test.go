//go:build unit

package service

// 人工验票的时序护栏。
//
// 这里构造的是**先后交错**而不是并发：账号级槽位（kongVerifyOnlySlot）保证同一账号不会同时跑两拨
// 挑战，但它挡不住"验证进行中，别的路径改了库"这种交错，而每一步都要花真实额度（一张票最多三份
// 挑战），据一个别人写的状态往下走就是白烧。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func kongManualFixture(t *testing.T, model string) (*kongStubRepo, *kongStubUpstream, *KongTicketService) {
	repo := newKongStubRepo()
	accounts := &kongStubAccounts{accounts: map[int64]*Account{}}
	accounts.set(kongVerifyTestAccount())
	up := &kongStubUpstream{proxyState: KongTicketProxyState{Exists: true}, answers: kongVerifyAnswers()}
	svc := kongTestService(t, repo, up, accounts)
	svc.accept = KongTicketAccept{model: []string{model}}
	return repo, up, svc
}

// kongChallengeHook 在第一份挑战发出前插一个钩子，用来模拟"验证进行中，业务路径撤销了票"。
type kongChallengeHook struct {
	*kongStubUpstream
	before func()
}

func (u *kongChallengeHook) RunChallenge(ctx context.Context, a *Account, proxy, model string,
	c KongFingerprintChallenge, state string,
) (*KongUpstreamAnswer, error) {
	if u.before != nil {
		hook := u.before
		u.before = nil
		hook()
	}
	return u.kongStubUpstream.RunChallenge(ctx, a, proxy, model, c, state)
}

// 「别的路径撤销了票」不等于「本次测出真降档」，因此不能据此去验第二张。
//
// 时序：人工重验当前票期间，业务请求拿到上游重发的票并撤销了它；人工的挑战自己全部 429。这时库里
// 已是 rejected，但本次验证什么都没测出来——凭库状态往下走就会白烧一整张候选的额度。
func TestKongManualVerifyExternalRevocationIsNotDowngrade(t *testing.T) {
	const model = "gpt-5.6-sol"
	repo, up, svc := kongManualFixture(t, model)
	current := kongSeedCurrentTicket(repo, model, time.Now())
	kongSeedCandidate(repo, model, 21, time.Minute)
	up.answerErr = errors.New("429")
	svc.upstream = &kongChallengeHook{kongStubUpstream: up, before: func() {
		svc.RevokeUsedTicket(context.Background(), 1, model, current.ID)
	}}

	out, err := svc.TriggerVerify(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("立即验票: %v", err)
	}
	if len(out.Steps) != 1 {
		t.Fatalf("外部撤销被误判成真降档：挑战 %d 份、结果 %+v", up.challengeCall, out)
	}
}
