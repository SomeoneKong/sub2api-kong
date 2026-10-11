package service

import (
	"context"
	"errors"
)

// KongWSTurnSchedulable 是原生 WebSocket 每一轮上送前的复核：连接绑定一个上游账号、不能中途换号，账号已被关调度
// （降档哨兵或人工）就要求客户端重连重选。与计划功能是否启用无关。读不到账号时按建连时的对象判断，账号已删除
// 则不放行。设计见 DESIGN-account-downgrade-guard.md §3.5。
func (s *OpenAIGatewayService) KongWSTurnSchedulable(ctx context.Context, account *Account) bool {
	if s == nil || account == nil || !account.IsOpenAIOAuthLike() {
		return true
	}
	current, err := s.kongPlanAccount(ctx, account.ID)
	switch {
	case errors.Is(err, ErrAccountNotFound):
		return false
	case err != nil || current == nil:
		return account.Schedulable
	}
	return current.Schedulable
}

// SetKongDowngradeGuard 持有降档哨兵，只为退出时能停止它；传 nil 等于不启用。
func (s *OpenAIGatewayService) SetKongDowngradeGuard(g *KongDowngradeGuard) {
	if s != nil {
		s.kongDowngradeGuard = g
	}
}

// StopKongDowngradeGuard 在进程退出时停止哨兵：取消在途的探测与指纹测试并等待协程退出。
func (s *OpenAIGatewayService) StopKongDowngradeGuard() {
	if s != nil && s.kongDowngradeGuard != nil {
		s.kongDowngradeGuard.Stop()
	}
}
