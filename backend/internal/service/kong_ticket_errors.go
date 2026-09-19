package service

import (
	"errors"
)

// KongErrUpstreamNotAttempted 表示上游请求**还没送出去**就失败了（构造请求阶段的本地错误，
// 例如缺凭据）。
//
// 它必须与「发出去了但失败」区分开：出口的静默是按 IP 积累的，只有真的发了包才会被清零。把本地
// 失败也算成一次出口活动，会让共用同一出口的其它账号凭空多等一个静默期，而那段静默其实还在。
type KongErrUpstreamNotAttempted struct{ Err error }

func (e *KongErrUpstreamNotAttempted) Error() string {
	return "upstream request not attempted: " + e.Err.Error()
}

func (e *KongErrUpstreamNotAttempted) Unwrap() error { return e.Err }

// KongIsUpstreamNotAttempted 报告这个错误是否发生在请求送出之前。
func KongIsUpstreamNotAttempted(err error) bool {
	var target *KongErrUpstreamNotAttempted
	return errors.As(err, &target)
}

// KongErrTicketRejected 表示一张票按既定规则被拒收（目前只有长度黑名单）。
//
// 它是**预期结果**而不是故障：拒收时已经写过事件，调用方不该再记一条持久化失败。
type KongErrTicketRejected struct{ Reason string }

func (e *KongErrTicketRejected) Error() string { return "ticket rejected: " + e.Reason }

// KongIsExpectedTicketRejection 报告这个错误是否为既定规则下的预期拒收。
func KongIsExpectedTicketRejection(err error) bool {
	var target *KongErrTicketRejected
	return errors.As(err, &target)
}
