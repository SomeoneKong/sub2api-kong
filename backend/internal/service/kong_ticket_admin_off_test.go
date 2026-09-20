//go:build unit

package service

import (
	"context"
	"testing"
	"time"
)

// kongStubAdminAccounts 是管理面用到的账号访问替身（KongAccountAccess）。
type kongStubAdminAccounts struct {
	views []KongAccountView
}

func (a *kongStubAdminAccounts) ListTicketCapableAccounts(_ context.Context) ([]KongAccountView, error) {
	return a.views, nil
}

func (a *kongStubAdminAccounts) GetAccountView(_ context.Context, accountID int64) (*KongAccountView, error) {
	for i := range a.views {
		if a.views[i].ID == accountID {
			return &a.views[i], nil
		}
	}
	return nil, nil
}

func (a *kongStubAdminAccounts) UpdateAccountExtra(_ context.Context, _ int64, _ map[string]any) error {
	return nil
}

func (a *kongStubAdminAccounts) GetProxyState(_ context.Context, _ int64) (KongTicketProxyState, error) {
	return KongTicketProxyState{Exists: true}, nil
}

// off 模式也要输出逐模型信息。
//
// off 与 observe 的差别只是不主动探测/取票，票仍然被动收下（设计 §2.3），所以候选数、取样长度、
// 历史诊断、乃至上次开 full 时留下的合格票都已经在库里——把它们藏起来等于让运维在"这个账号
// 现在到底有没有票"上无据可查。这里钉住的是那个早退不会被重新引入。
//
// 同时钉住反面：off 从不取票，"出口静默"与"下次可取票"对它没有意义，不能顺手一起显示。
func TestKongAdminOffModeStillReportsModels(t *testing.T) {
	now := time.Now()
	repo := newKongStubRepo()
	// 上次开着 full 时留下的合格票：它在 off 下不会被注入，但确实还在库里、还没过期。
	repo.setCurrent(1, "gpt-6-astra", &KongTicket{
		ID: 9, AccountID: 1, Model: "gpt-6-astra", Source: KongTicketSourceFetch,
		Status: KongTicketStatusVerified, ExpiresAt: now.Add(30 * time.Minute),
		FingerprintModel: kongStrPtr("gpt-6-astra"), FingerprintP: kongFloatPtr(0.99),
		FingerprintProbs: map[string]float64{"gpt-6-astra": 0.99},
	})
	lastUsed := now.Add(-10 * time.Minute)
	repo.lastEgressUsed = &lastUsed

	proxyID := int64(7)
	accounts := &kongStubAdminAccounts{views: []KongAccountView{{
		ID: 1, Name: "acc", Platform: PlatformOpenAI, Ready: true,
		Extra: map[string]any{
			KongTicketModeKey:    string(KongTicketModeOff),
			KongTicketEgressKey:  string(KongTicketEgressProxy),
			KongTicketProxyIDKey: proxyID,
		},
	}}}

	admin := NewKongTicketAdminService(repo, accounts, KongDefaultTicketParams(),
		[]string{"gpt-6-astra"}, KongTicketAccept{}, 0.9)

	list, err := admin.Overview(context.Background(), now)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 个账号，实得 %d", len(list))
	}
	status := list[0]
	if status.Mode != KongTicketModeOff {
		t.Fatalf("模式应为 off，实为 %s", status.Mode)
	}
	if len(status.Models) != 1 {
		t.Fatalf("off 也要逐模型输出，实得 %d 条", len(status.Models))
	}
	if status.Models[0].CurrentTicket == nil {
		t.Fatal("库里那张未过期的合格票必须显示出来")
	}
	if status.Models[0].CurrentTicket.ID != 9 {
		t.Fatalf("显示的应是 9 号票，实为 %d", status.Models[0].CurrentTicket.ID)
	}
	if status.EgressIdleSeconds != nil {
		t.Fatalf("off 从不取票，不该报出口静默，实得 %d", *status.EgressIdleSeconds)
	}
	if status.NextFetchAllowedAt != nil {
		t.Fatalf("off 从不取票，不该报下次可取票时刻，实得 %v", *status.NextFetchAllowedAt)
	}
}
