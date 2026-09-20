package service

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

// 编排层测试共用的仓储替身。
//
// 它记录调用而不只是吞掉：本功能相当多的缺陷形态是「该写的没写、写失败没人管」，替身如果
// 什么都不留痕，这类问题在测试里就看不见。

type kongStubRepo struct {
	// stg0Stats / stg0Err 由用例预置，供 Stg0Stats 原样交回（聚合逻辑测不到，见该方法注释）。
	stg0Stats []*KongStg0Stats
	stg0Err   error
	mu        sync.Mutex

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
	// insertProbesHook 在探测落库时回调，供测试模拟"落库期间前提变了"。
	insertProbesHook func()
	setStatusOK      *bool
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

// kongStubTicketListMax 必须与仓储的 kongTicketListMax 同值（那个在别的包里、不可见）。两处
// 分叉就会让列表的边界行为在测试里与生产不一致。
const kongStubTicketListMax = 200

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

// snapshotOf 按票 id 生成一份返回快照：**所有字段都以权威状态为准**。
//
// 生产的 `SELECT` / `UPDATE ... RETURNING` 返回的是库里那一行，不可能出现"用 A 行判断条件、返回
// B 行字段"。桩若只覆盖 status/skip 而让 expires_at、captured_at、source 沿用预置对象，就会在
// 那几个字段上与生产分叉——而调用方正是据它们判期限与来源的。
func (r *kongStubRepo) snapshotOf(id int64) *KongTicket {
	t := r.ticketByID(id)
	if t == nil {
		return nil
	}
	clone := *t
	if st := r.tickets[id]; st != nil {
		clone.AccountID, clone.Model = st.AccountID, st.Model
		clone.Status, clone.ExpiresAt, clone.SkipUntilNew = st.Status, st.ExpiresAt, st.SkipUntilNew
		clone.CapturedAt, clone.Source = st.CapturedAt, st.Source
	}
	return &clone
}

// VerifiedTicketsElsewhere 与生产同条件：别的账号、同模型、verified、未过期、未 skip，每账号取
// 最晚过期的一张，且**带回归因分布**（调用方要按当前白名单重判）。
func (r *kongStubRepo) VerifiedTicketsElsewhere(_ context.Context, excludeAccountID int64, model string) ([]*KongTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	best := map[int64]*KongTicket{}
	for id := range r.tickets {
		st := r.tickets[id]
		if st == nil || st.AccountID == excludeAccountID || st.Model != model {
			continue
		}
		if st.Status != KongTicketStatusVerified || st.SkipUntilNew || !st.ExpiresAt.After(now) {
			continue
		}
		snap := r.snapshotOf(id)
		if snap == nil {
			// 只登记了状态、没有票对象的用例：按生产口径补一个最小快照，归因分布留空——那正是
			// "按当前白名单判不合格"的情形。
			snap = &KongTicket{ID: id, AccountID: st.AccountID, Model: st.Model,
				Status: st.Status, ExpiresAt: st.ExpiresAt, CapturedAt: st.CapturedAt}
		}
		if cur, ok := best[st.AccountID]; !ok || snap.ExpiresAt.After(cur.ExpiresAt) {
			best[st.AccountID] = snap
		}
	}
	out := make([]*KongTicket, 0, len(best))
	for _, t := range best {
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}

// TicketByID 按 (账号, id) 找。**account_id 参与匹配**是生产侧的权限边界，桩不照做就测不出
// "换个 id 去验别人的票"这条。
func (r *kongStubRepo) TicketByID(_ context.Context, accountID, id int64) (*KongTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.snapshotOf(id)
	if t == nil || t.AccountID != accountID {
		return nil, nil
	}
	return t, nil
}

// PrepareTicketForManualVerify 按 id 准备重验。生产条件：**只看期限**（过期即不动，返回零行）；
// rejected 复位成 unverified，别的状态不动；跳过标记一律清掉。
func (r *kongStubRepo) PrepareTicketForManualVerify(_ context.Context, id int64) (*KongTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.tickets[id]
	if st == nil || !st.ExpiresAt.After(time.Now()) {
		return nil, nil
	}
	if st.Status == KongTicketStatusRejected {
		st.Status = KongTicketStatusUnverified
		r.revived = append(r.revived, id)
	}
	st.SkipUntilNew = false
	return r.snapshotOf(id), nil
}

// ListTickets 列一个账号名下的票，最近采集的在前——含已过期与已拒的，同生产。
func (r *kongStubRepo) ListTickets(_ context.Context, accountID int64, limit int) ([]*KongTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 生产侧把 0、负数与超上限都归一化到同一个上限（见仓储的 ListTickets）。桩不照做，边界上的
	// 行数差异就测不出来。
	if limit <= 0 || limit > kongStubTicketListMax {
		limit = kongStubTicketListMax
	}
	seen := map[int64]bool{}
	var out []*KongTicket
	collect := func(t *KongTicket) {
		if t == nil || seen[t.ID] {
			return
		}
		snap := r.snapshotOf(t.ID)
		if snap == nil || snap.AccountID != accountID {
			return
		}
		seen[t.ID] = true
		out = append(out, snap)
	}
	for _, list := range r.current {
		for _, t := range list {
			collect(t)
		}
	}
	for _, list := range r.candidates {
		for _, t := range list {
			collect(t)
		}
	}
	for _, t := range r.inserted {
		collect(t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CapturedAt.After(out[j].CapturedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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

// TicketConclusions 按生产口径实现：只认提交了归因的最终事件（`fingerprint_model` 非空），每张票取
// 事件时间最新的一条，缺 `stg` 键算 stg1。
//
// 桩也照这个口径走，否则"未提交的收尾不该改写来源"这条在桩上测不出来——生产是在 SQL 里筛的。
func (r *kongStubRepo) TicketConclusions(_ context.Context, accountID int64, ticketIDs []int64) (map[int64]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := map[int64]bool{}
	for _, id := range ticketIDs {
		want[id] = true
	}
	out := map[int64]int{}
	newest := map[int64]time.Time{}
	for _, e := range r.events {
		switch {
		case e == nil || e.TicketID == nil || !want[*e.TicketID]:
			continue
		case e.AccountID != accountID || e.EventType != KongEventVerify:
			continue
		case e.Detail["final"] != true:
			continue
		case e.FingerprintModel == nil || *e.FingerprintModel == "":
			continue
		}
		if at, seen := newest[*e.TicketID]; seen && !e.CreatedAt.After(at) {
			continue
		}
		newest[*e.TicketID] = e.CreatedAt
		if raw, ok := e.Detail["stg"].(float64); ok {
			out[*e.TicketID] = int(raw)
		} else {
			out[*e.TicketID] = 1
		}
	}
	return out, nil
}

// ListEvents 要按生产口径过滤与排序。
//
// 原来它把 filter 整个忽略、原序返回全部事件，于是两类差异测不出来：`FinalOnly` 漏筛会把"某一份挑战
// 失败"当成本次验证的结论（生产侧那条筛选正是为此加的），而升序返回会让"同一张票取最新那条结论"的
// 代码在桩上恰好取到最旧的一条——方向反了还全绿。
func (r *kongStubRepo) ListEvents(_ context.Context, filter *KongTicketEventFilter) ([]*KongTicketEvent, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	match := func(e *KongTicketEvent) bool {
		if e == nil {
			return false
		}
		if filter == nil {
			return true
		}
		if len(filter.AccountIDs) > 0 && !slices.Contains(filter.AccountIDs, e.AccountID) {
			return false
		}
		if len(filter.Models) > 0 && !slices.Contains(filter.Models, e.Model) {
			return false
		}
		if len(filter.EventTypes) > 0 && !slices.Contains(filter.EventTypes, e.EventType) {
			return false
		}
		if filter.FinalOnly && e.Detail["final"] != true {
			return false
		}
		if filter.Since != nil && e.CreatedAt.Before(*filter.Since) {
			return false
		}
		if filter.Until != nil && !e.CreatedAt.Before(*filter.Until) {
			return false
		}
		return true
	}
	var out []*KongTicketEvent
	for i := len(r.events) - 1; i >= 0; i-- {
		// 倒着遍历：生产是 `ORDER BY created_at DESC, id DESC`，插入序即 id 序。
		if match(r.events[i]) {
			out = append(out, r.events[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	total := len(out)
	if filter != nil {
		if filter.Offset > 0 {
			if filter.Offset >= len(out) {
				return nil, total, nil
			}
			out = out[filter.Offset:]
		}
		if filter.Limit > 0 && filter.Limit < len(out) {
			out = out[:filter.Limit]
		}
	}
	return out, total, nil
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
	hook := r.insertProbesHook
	r.mu.Unlock()
	// 落库**期间**前提会变（模式被切走、代理被改、调度资格被取消）。这个钩子让那一段时间在测试里
	// 真的存在——只在保存前后做断言，看不出"保存开始时成立、提交时已失效"这条路径。
	if hook != nil {
		hook()
	}
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

// Stg0Stats 返回用例预置的汇总。
//
// ⚠️ **聚合逻辑在这里是测不到的**：生产实现是一条打 usage_logs 的 SQL（两次 GROUP BY、
// FILTER、有效模型表达式），桩只是把预置值原样交回。所以这个方法只够测上层的装配与展示——
// "分母取错了列""FILTER 写反了"这类错误只能在真库上发现（口径见 PG-VERIFY.md）。
//
// 桩不忠实已经在本子系统上咬过多次（最近一次是 fused 列压根没落库而测试全绿），所以这里把
// 边界写明，而不是让后来人以为统计被测过了。
func (r *kongStubRepo) Stg0Stats(_ context.Context, models []string, _ time.Time) ([]*KongStg0Stats, error) {
	if r.stg0Err != nil {
		return nil, r.stg0Err
	}
	want := map[string]bool{}
	for _, m := range models {
		want[m] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*KongStg0Stats, 0, len(r.stg0Stats))
	for _, s := range r.stg0Stats {
		// 照生产口径按模型过滤：用例传的门控集合变了、预置数据没跟着变时，这一条能让它露出来。
		if s != nil && want[s.Model] {
			clone := *s
			out = append(out, &clone)
		}
	}
	return out, nil
}
