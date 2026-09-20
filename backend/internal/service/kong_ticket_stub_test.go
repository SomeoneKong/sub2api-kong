package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// 编排层测试共用的仓储替身。
//
// 它记录调用而不只是吞掉：本功能相当多的缺陷形态是「该写的没写、写失败没人管」，替身如果
// 什么都不留痕，这类问题在测试里就看不见。

type kongStubRepo struct {
	mu sync.Mutex

	current      map[string][]*KongTicket
	candidates   map[string][]*KongTicket
	nextID       int64
	inserted     []*KongTicket
	events       []*KongTicketEvent
	probes       []*KongFingerprintProbe
	revoked      []int64
	skipped      []int64
	skippedBulk  []string
	skipBoundary time.Time
	byState      map[string]int64
	cleared      int
	statusSets   []kongStubStatusSet

	lastEgressUsed *time.Time
	lastCooldown   *time.Time
	lastEventAt    map[string]*time.Time

	// 注入的错误，用来验证「写失败不被静默吞掉」。
	insertEventErr  error
	insertTicketErr error
	insertProbesErr error
	setStatusOK     *bool
	// commitErr 让用例模拟「资格与最终事件的提交失败」。
	commitErr error
	// tickets 是**按票 id 保存的真实状态**：status / expires_at / skip_until_new。
	//
	// 桩必须把生产 SQL 的条件也模拟出来，否则那些条件的错误在测试里根本看不见——「先 skip 后提交
	// 必然更新 0 行」这条回归就是靠桩不忠实才全绿通过的。current / candidate 两张表只决定「读到
	// 哪一张」，是否真的可用由这里的状态决定。
	tickets map[int64]*kongStubTicketState
	// revived 记下被手工复位过的票 id。
	revived []int64
}

// kongStubTicketState 是一张票在桩里的状态，字段与生产表一一对应。
type kongStubTicketState struct {
	AccountID    int64
	Model        string
	Status       string
	ExpiresAt    time.Time
	SkipUntilNew bool
	CapturedAt   time.Time
	Source       string
}

type kongStubStatusSet struct {
	ID               int64
	Status           string
	FingerprintModel string
	Probability      float64
	Probs            map[string]float64
}

func newKongStubRepo() *kongStubRepo {
	return &kongStubRepo{
		current:     map[string][]*KongTicket{},
		candidates:  map[string][]*KongTicket{},
		lastEventAt: map[string]*time.Time{},
		byState:     map[string]int64{},
		tickets:     map[int64]*kongStubTicketState{},
	}
}

func kongStubKey(accountID int64, model string) string {
	return fmt.Sprintf("%d\x00%s", accountID, model)
}

// VerifiedTickets 必须照生产 SQL 的条件过滤：verified、未过期，**按 expires_at 降序返回全部**。
//
// **归因判据不在这里**——它由 KongTicketAccept 在 Go 侧判，桩若替它判一遍就成了第二套规则，
// 真实实现改了规则而桩没改时测试仍然全绿。但另外三条必须照做：status 与期限（撤销与过期要真的
// 影响读取），以及**返回多张并排好序**——只回一张的桩测不出「较新的票按当前白名单不合格、较旧
// 那张合格」这个场景，而那正是不能在 SQL 里截断的理由。
// poolOf 返回该 (账号, 模型) 下的全部票，**不分 current / candidate**——那两个 map 只是用例放票的
// 位置，生产里是同一张表。分开查会让「复位的候选验证成功后成为可用票」这条链在测试里永远断开。
func (r *kongStubRepo) poolOf(accountID int64, model string) []*KongTicket {
	key := kongStubKey(accountID, model)
	seen := map[int64]bool{}
	var out []*KongTicket
	for _, t := range r.current[key] {
		if t != nil && !seen[t.ID] {
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	for _, t := range r.candidates[key] {
		if t != nil && !seen[t.ID] {
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	// 本轮 InsertTicket 插进来的票同样要在池子里：生产侧 VerifiedTickets 查的是表，刚插入的行
	// 当然查得到。少了这一支，「取票 → 验证 → 回读当前票」这条链路在测试里永远断在最后一步
	// （表现成任务成功却拿不到票）。
	for _, t := range r.inserted {
		if t != nil && t.AccountID == accountID && t.Model == model && !seen[t.ID] {
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	return out
}

func (r *kongStubRepo) VerifiedTickets(_ context.Context, accountID int64, model string) ([]*KongTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var out []*KongTicket
	for _, t := range r.poolOf(accountID, model) {
		if t == nil {
			continue
		}
		// 状态与期限优先看桩登记的状态（撤销等改动只改那里），没登记的用票行自身的字段。
		status, expires := t.Status, t.ExpiresAt
		if st := r.stateOf(t); st != nil {
			status, expires = st.Status, st.ExpiresAt
		}
		if status != KongTicketStatusVerified || !expires.After(now) {
			continue
		}
		// 生产侧筛掉 skip：一张 verified 票被标 skip 只有一种来源——注入被上游拒绝，那张票已经
		// 不作数，不能再当当前票。
		if st := r.stateOf(t); st != nil && st.SkipUntilNew {
			continue
		}
		if t.SkipUntilNew && r.stateOf(t) == nil {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ExpiresAt.After(out[j].ExpiresAt) })
	return out, nil
}

// setCurrent 预置该 (账号, 模型) 下的 verified 票，可以给多张。
func (r *kongStubRepo) setCurrent(accountID int64, model string, tickets ...*KongTicket) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current[kongStubKey(accountID, model)] = tickets
}

// ticketByID 在预置的票里按 id 找，供落结论时把归因写回可被后续查询读到的那个对象。
func (r *kongStubRepo) ticketByID(id int64) *KongTicket {
	for _, list := range r.current {
		for _, t := range list {
			if t != nil && t.ID == id {
				return t
			}
		}
	}
	for _, list := range r.candidates {
		for _, t := range list {
			if t != nil && t.ID == id {
				return t
			}
		}
	}
	// 也要找本轮插入的票：否则「取票 → 验证 → 归因回写 → 按当前白名单授予资格」这条链路断在
	// 回写那一步，票会以空的归因分布留在库里，于是永远判不合格。
	for _, t := range r.inserted {
		if t != nil && t.ID == id {
			return t
		}
	}
	return nil
}

// stateOf 取一张票在桩里的状态；没登记过的票（用例直接塞进 current/candidate 的）返回 nil，
// 表示「不额外约束」——那些用例关心的不是状态机。
func (r *kongStubRepo) stateOf(t *KongTicket) *kongStubTicketState {
	if t == nil || t.ID == 0 {
		return nil
	}
	return r.tickets[t.ID]
}

func (r *kongStubRepo) NewestCandidate(_ context.Context, accountID int64, model string, now time.Time) (*KongTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 生产条件：unverified、未过期、未被跳过；**不筛来源、不看 minAge**，从整池里挑最晚采集的
	// 那张，与生产的 ORDER BY captured_at DESC 一致。
	var best *KongTicket
	var bestCaptured time.Time
	for _, t := range r.poolOf(accountID, model) {
		st := r.stateOf(t)
		status, expires, skip, captured := t.Status, t.ExpiresAt, t.SkipUntilNew, t.CapturedAt
		if st != nil {
			status, expires, skip, captured = st.Status, st.ExpiresAt, st.SkipUntilNew, st.CapturedAt
		}
		if status != KongTicketStatusUnverified || skip || !expires.After(now) {
			continue
		}
		if best == nil || captured.After(bestCaptured) {
			best, bestCaptured = t, captured
		}
	}
	return best, nil
}

func (r *kongStubRepo) OldestCandidate(_ context.Context, accountID int64, model string, minAge time.Duration, now time.Time) (*KongTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 生产条件：unverified、未过期、未被跳过，且 observed 来源要满足最小年龄（fetch 不受限）。
	// 从整池里挑**最早采集**的那张，与生产的 ORDER BY captured_at ASC 一致。
	var best *KongTicket
	var bestCaptured time.Time
	for _, t := range r.poolOf(accountID, model) {
		st := r.stateOf(t)
		status, expires, skip := t.Status, t.ExpiresAt, t.SkipUntilNew
		captured, source := t.CapturedAt, t.Source
		if st != nil {
			status, expires, skip = st.Status, st.ExpiresAt, st.SkipUntilNew
			captured, source = st.CapturedAt, st.Source
		}
		if status != KongTicketStatusUnverified || skip || !expires.After(now) {
			continue
		}
		if source == KongTicketSourceObserved && captured.After(now.Add(-minAge)) {
			continue
		}
		if best == nil || captured.Before(bestCaptured) {
			best, bestCaptured = t, captured
		}
	}
	return best, nil
}

func (r *kongStubRepo) InsertTicket(_ context.Context, t *KongTicket) (int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insertTicketErr != nil {
		return 0, false, r.insertTicketErr
	}
	// 与真实仓储一样按 (account, model, state) 去重：重复票返回既有 id 且 inserted=false。
	key := kongStubKey(t.AccountID, t.Model) + "|" + t.State
	if existing, ok := r.byState[key]; ok {
		return existing, false, nil
	}
	r.nextID++
	t.ID = r.nextID
	r.inserted = append(r.inserted, t)
	r.byState[key] = t.ID
	// 入库即登记状态，之后的读取与提交都按它判。
	r.tickets[t.ID] = &kongStubTicketState{
		AccountID: t.AccountID, Model: t.Model, Status: t.Status,
		ExpiresAt: t.ExpiresAt, SkipUntilNew: t.SkipUntilNew,
		CapturedAt: t.CapturedAt, Source: t.Source,
	}
	return t.ID, true, nil
}

// SkipCandidatesFor 除了记账，还要**真的把状态改掉**。
//
// 只记调用、让条件更新照常成功，会掩盖生产 SQL 的行为：那边 `skip_until_new = TRUE` 之后，提交
// 资格的条件（含 `skip_until_new = FALSE`）必然更新 0 行。这条差异曾经让一个真实回归——非目标
// 结论被自身的批量 skip 作废——在测试全绿的情况下溜过去。
func (r *kongStubRepo) SkipCandidatesFor(_ context.Context, accountID int64, model string, capturedBefore time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skippedBulk = append(r.skippedBulk, kongStubKey(accountID, model))
	r.skipBoundary = capturedBefore
	// 只标**这个账号这个模型**、边界之前、仍是 unverified 的票。跨账号一起标是桩自己的 bug，
	// 会让别的账号的提交莫名失败。
	for _, st := range r.tickets {
		if st.AccountID != accountID || st.Model != model {
			continue
		}
		if st.Status != KongTicketStatusUnverified || st.CapturedAt.After(capturedBefore) {
			continue
		}
		// 生产侧只淘汰 observed 候选：批内取回的 fetch 票是一整段出口静默换来的，不能被旧候选的
		// 失败连带标掉（见 SkipCandidatesFor 的注释）。桩不筛来源就测不出那条损害。
		if st.Source != KongTicketSourceObserved {
			continue
		}
		st.SkipUntilNew = true
	}
	return nil
}

func (r *kongStubRepo) SetTicketStatus(_ context.Context, id int64, status string, attr KongAttribution) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusSets = append(r.statusSets, kongStubStatusSet{ID: id, Status: status, FingerprintModel: attr.Model, Probability: attr.P, Probs: attr.Probs})
	if r.setStatusOK != nil {
		return *r.setStatusOK, nil
	}
	if st := r.tickets[id]; st != nil {
		// 与 CommitVerification 同一前置：rejected 排除，verified 可被重验改写。
		if st.Status == KongTicketStatusRejected {
			return false, nil
		}
		st.Status = status
	}
	// 归因要写回票对象本身，否则「提交完整分布 → 重新读票 → 按当前白名单授予资格」这条链路
	// 在测试里断开：读回来的票永远没有分布，于是永远判不合格。
	if t := r.ticketByID(id); t != nil {
		t.Status = status
		t.FingerprintModel = kongStrPtr(attr.Model)
		t.FingerprintP = kongFloatPtr(attr.P)
		t.FingerprintProbs = attr.Probs
	}
	return true, nil
}

// CommitVerification 在 stub 里按「条件更新 + 写事件」的同一语义记账：更新不成立时事件也不写。
func (r *kongStubRepo) CommitVerification(ctx context.Context, id int64, status string, attr KongAttribution, event *KongTicketEvent) (bool, error) {
	if r.commitErr != nil {
		return false, r.commitErr
	}
	// 生产条件：`status IN (unverified, verified) AND skip_until_new = FALSE AND
	// expires_at > now()`（verified 是为了人工重验当前票，rejected 排除）。
	// 逐票判——「任何账号被标过就全都提交失败」是桩自己的错，会掩盖真实的隔离问题。
	r.mu.Lock()
	st := r.tickets[id]
	blocked := st != nil && (st.Status == KongTicketStatusRejected || st.SkipUntilNew || !st.ExpiresAt.After(time.Now()))
	r.mu.Unlock()
	if blocked {
		return false, nil
	}
	updated, err := r.SetTicketStatus(ctx, id, status, attr)
	if err != nil || !updated {
		return updated, err
	}
	if err := r.InsertEvent(ctx, event); err != nil {
		// 真实实现里事务会把状态更新一起回滚，stub 照样要撤掉那条记账。
		r.mu.Lock()
		if n := len(r.statusSets); n > 0 {
			r.statusSets = r.statusSets[:n-1]
		}
		r.mu.Unlock()
		return false, err
	}
	return true, nil
}

func (r *kongStubRepo) TicketStatus(_ context.Context, id int64) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.tickets[id]; st != nil {
		return st.Status, nil
	}
	return "", nil
}

func (r *kongStubRepo) SkipCandidate(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skipped = append(r.skipped, id)
	if st := r.tickets[id]; st != nil {
		// 生产侧只对 unverified 生效：跳过标记是候选池的机制，打在正在服务的票上等于悄悄作废它。
		if st.Status != KongTicketStatusUnverified {
			return nil
		}
		st.SkipUntilNew = true
	}
	return nil
}

func (r *kongStubRepo) ClearSkipMarks(_ context.Context, accountID int64, model string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared++
	// 解除标记必须真的解除：不解除的话「新票到达后旧候选恢复可选」这条语义测不出来。
	for _, st := range r.tickets {
		if st.AccountID == accountID && st.Model == model {
			st.SkipUntilNew = false
		}
	}
	return nil
}

func (r *kongStubRepo) RevokeTicket(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked = append(r.revoked, id)
	// 撤销要影响后续读取，否则「撤销了却还能当当前票用」测不出来。
	if st := r.tickets[id]; st != nil {
		st.Status = KongTicketStatusRejected
	}
	return nil
}

func (r *kongStubRepo) CountTickets(_ context.Context, _ int64, _ string, _ string) (int, error) {
	return 0, nil
}

func (r *kongStubRepo) DeleteExpiredTickets(_ context.Context, _ time.Time, _ int) (int64, error) {
	return 0, nil
}

func (r *kongStubRepo) InsertEvent(_ context.Context, e *KongTicketEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insertEventErr != nil {
		return r.insertEventErr
	}
	r.events = append(r.events, e)
	return nil
}

func (r *kongStubRepo) ListEvents(_ context.Context, _ *KongTicketEventFilter) ([]*KongTicketEvent, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events, len(r.events), nil
}

func (r *kongStubRepo) LastEgressActivity(_ context.Context, _ string) (*time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastEgressUsed, nil
}

func (r *kongStubRepo) LastFailure(_ context.Context, _ int64, _ string) (*time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastCooldown, nil
}

func (r *kongStubRepo) LastEventAt(_ context.Context, _ int64, _ string, eventType string) (*time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.lastEventAt[eventType]; ok {
		return t, nil
	}
	return nil, nil
}

func (r *kongStubRepo) InsertProbes(_ context.Context, probes []*KongFingerprintProbe) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.insertProbesErr != nil {
		return r.insertProbesErr
	}
	r.probes = append(r.probes, probes...)
	return nil
}

func (r *kongStubRepo) ListProbesByVerification(_ context.Context, verificationID string) ([]*KongFingerprintProbe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*KongFingerprintProbe, 0, len(r.probes))
	for _, p := range r.probes {
		if p.VerificationID == verificationID {
			out = append(out, p)
		}
	}
	return out, nil
}

// eventTypes 返回已记录事件的类型序列，便于断言「该记的记了、不该记的没记」。
func (r *kongStubRepo) eventTypes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.EventType)
	}
	return out
}

func (r *kongStubRepo) countEvents(eventType string) int {
	n := 0
	for _, t := range r.eventTypes() {
		if t == eventType {
			n++
		}
	}
	return n
}

// kongTestAttr 造一个归因结论：argmax 是 astra、概率为 p，其余质量落到 sol。
func kongTestAttr(p float64) KongAttribution {
	return KongAttribution{
		Model: "gpt-6-astra", P: p,
		Probs: map[string]float64{"gpt-6-astra": p, "gpt-5.6-sol": 1 - p},
	}
}

// ReviveRejectedCandidate 照生产 SQL 的条件实现：只复位**未过期**且**已拒**的那一张，取最晚
// 过期的，并清掉 skip 标记。
//
// 桩少实现一个条件那个条件的错误就测不出来——已过期的票被复位会让"重验一张过期票"这种问题全绿
// 通过，而它在生产里会走到提交时才被期限条件挡掉、白烧三份挑战。
func (r *kongStubRepo) ReviveRejectedCandidate(_ context.Context, accountID int64, model string) (*KongTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	// 从整池里挑**最晚过期**的那张已拒且未过期的票，与生产的 ORDER BY expires_at DESC 一致。
	var best *KongTicket
	for _, t := range r.poolOf(accountID, model) {
		status, expires := t.Status, t.ExpiresAt
		if st := r.stateOf(t); st != nil {
			status, expires = st.Status, st.ExpiresAt
		}
		if status != KongTicketStatusRejected || !expires.After(now) {
			continue
		}
		if best == nil || expires.After(best.ExpiresAt) {
			best = t
		}
	}
	if best == nil {
		return nil, nil
	}
	// 状态表与票对象都要改：前者是后续条件判定的权威，后者是调用方拿到手直接用的那份。
	if st := r.stateOf(best); st != nil {
		st.Status = KongTicketStatusUnverified
		st.SkipUntilNew = false
	}
	best.Status = KongTicketStatusUnverified
	best.SkipUntilNew = false
	r.revived = append(r.revived, best.ID)
	return best, nil
}
