package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 账号选择与 credits 的输入存储（fork 专有，设计见 DESIGN-openai-plan-dispatch.md）：发布、发布状态、
// 人工约束、慢速部分与调用方声明。
//
// 内存里存一份不可变状态，选号路径只读它，不访问数据库。所有写入经同一把锁串行：锁内先提交数据库事务，
// 再换上新的内存状态，成功响应在替换之后才返回。所以后提交的写入一定后替换，内存不会被较早的写入覆盖回去。
// 这以网关单实例为前提。写入结果不确定时（事务报错，但可能已提交），下一次读写前先从库里重新加载。

const (
	kongPlanKindPublish  = "publish"
	kongPlanKindDispatch = "dispatch"
	kongPlanKindControl  = "control"
	kongPlanKindForecast = "forecast"

	kongPlanWriteTimeout = 10 * time.Second
	kongPlanLoadTimeout  = 10 * time.Second
)

// KongPlanRow 是 kong_plan_dispatch 的一行。标量列只是 Content 里同名字段的副本。
type KongPlanRow struct {
	Kind       string
	Generation *int64
	Seq        *int64
	Revision   *int64
	SHA256     string
	Content    []byte
}

// KongPlanAccountKind 是动态校验要用的账号类型。
type KongPlanAccountKind struct {
	Platform string
	Type     string
}

// KongPlanRepository 是存储的数据库访问。每个写方法一个事务。
type KongPlanRepository interface {
	LoadKongPlan(ctx context.Context) ([]KongPlanRow, *KongPoolCaller, error)
	SaveKongPlanRows(ctx context.Context, rows ...KongPlanRow) error
	SaveKongPoolCaller(ctx context.Context, caller KongPoolCaller) error
	// KongPlanAccountKinds 返回给定编号中存在且未删除的账号；不在结果里的就是不存在或已删除。
	KongPlanAccountKinds(ctx context.Context, ids []int64) (map[int64]KongPlanAccountKind, error)
}

// KongPlanPublishState 是发布状态。LastSeq 与 LastSHA256 是最近一次接受的发布，清空兜底与快照时不清。
type KongPlanPublishState struct {
	Generation int64      `json:"generation"`
	Enabled    bool       `json:"enabled"`
	ChangedAt  *time.Time `json:"changed_at"`
	Note       string     `json:"note"`
	LastSeq    int64      `json:"last_seq"`
	LastSHA256 string     `json:"last_sha256"`
}

// KongPlanDispatch 是最近一次接受的发布。清空后快照为 nil、HasFallback 为 false，其余字段保留作记录。
type KongPlanDispatch struct {
	Generation  int64             `json:"generation"`
	Seq         int64             `json:"seq"`
	PlanVersion int64             `json:"plan_version"`
	CycleID     string            `json:"cycle_id"`
	SHA256      string            `json:"content_sha256"`
	AcceptedAt  time.Time         `json:"accepted_at"`
	HasFallback bool              `json:"has_fallback"`
	Fallback    []KongPlanEntry   `json:"fallback"`
	Snapshot    *KongPlanSnapshot `json:"snapshot"`
	IgnoredIDs  []int64           `json:"ignored_ids"`

	// 按账号编号查条目的下标；每次构造或改动后由 buildIndex 重建，之后只读。
	snapIdx map[int64]int
	fbIdx   map[int64]int
}

func (d *KongPlanDispatch) buildIndex() {
	d.snapIdx, d.fbIdx = map[int64]int{}, map[int64]int{}
	if d.Snapshot != nil {
		for i, e := range d.Snapshot.Accounts {
			d.snapIdx[e.ID] = i
		}
	}
	if d.HasFallback {
		for i, e := range d.Fallback {
			d.fbIdx[e.ID] = i
		}
	}
}

// entryFor 返回账号在所用输入里的条目；输入里没有这个账号时返回 nil。
func (st *kongPlanState) entryFor(id int64, in string) *KongPlanEntry {
	if st == nil || st.dispatch == nil {
		return nil
	}
	d := st.dispatch
	switch in {
	case KongPlanInUseSnapshot:
		if i, ok := d.snapIdx[id]; ok {
			return &d.Snapshot.Accounts[i]
		}
	case KongPlanInUseFallback:
		if i, ok := d.fbIdx[id]; ok {
			return &d.Fallback[i]
		}
	}
	return nil
}

// KongPlanHold 是一个生效中的锁存暂停。
type KongPlanHold struct {
	Trigger string    `json:"trigger"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"`
	Note    string    `json:"note"`
}

// KongPlanRelease 记一个已解除的触发编号。全部保留：截断会让旧编号在重放时复活。
type KongPlanRelease struct {
	Trigger    string    `json:"trigger"`
	ReleasedAt time.Time `json:"released_at"`
	Note       string    `json:"note"`
}

// KongPlanControl 是人工约束：边车按修订号同步的部分，加上锁存暂停。
type KongPlanControl struct {
	Revision       int64             `json:"revision"`
	CreditsMode    string            `json:"credits_mode"`
	Allow          []int64           `json:"allow"`
	Floor          float64           `json:"floor"`
	PerPoint       *float64          `json:"per_point"`
	OverflowMargin map[string]int    `json:"overflow_margin"`
	FileHold       *KongPlanFileHold `json:"file_hold"`
	SyncedSHA256   string            `json:"synced_sha256"`
	LatchedHolds   []KongPlanHold    `json:"latched_holds"`
	Releases       []KongPlanRelease `json:"releases"`
	UpdatedAt      *time.Time        `json:"updated_at"`
}

// KongPoolCaller 是调用方声明。
type KongPoolCaller struct {
	State            string
	DesiredPPPerHour *float64
	Until            time.Time
	Note             string
	APIKeyID         *int64
	UpdatedAt        time.Time
}

// kongPlanState 是不可变的整体状态：替换时整份换，已发布的切片与 map 不再修改。
type kongPlanState struct {
	publish  KongPlanPublishState
	dispatch *KongPlanDispatch
	control  KongPlanControl
	forecast *KongPlanForecast
	caller   *KongPoolCaller
}

func kongPlanDefaultState() *kongPlanState {
	return &kongPlanState{
		publish: KongPlanPublishState{Generation: 1, Enabled: true},
		control: KongPlanControl{
			CreditsMode:    KongPlanCreditsModeShadow,
			Allow:          []int64{},
			OverflowMargin: map[string]int{"default": 0},
		},
	}
}

// inUse 是网关此刻实际所用的输入。停用发布不影响它，只有清空会。
func (st *kongPlanState) inUse(now time.Time) string {
	if st == nil || st.dispatch == nil {
		return KongPlanInUseLegacy
	}
	if st.dispatch.Snapshot != nil && now.Before(st.dispatch.Snapshot.ExpiresAt) {
		return KongPlanInUseSnapshot
	}
	if st.dispatch.HasFallback {
		return KongPlanInUseFallback
	}
	return KongPlanInUseLegacy
}

// KongPlanStore 保存计划组件的全部输入。
type KongPlanStore struct {
	repo  KongPlanRepository
	now   func() time.Time
	mu    sync.Mutex
	stale atomic.Bool
	// reloading / reloadAt：选号路径发现 stale 时的后台重载，同一时刻只有一个，失败后至少隔 kongPlanReloadGap 再试。
	reloading atomic.Bool
	reloadAt  atomic.Int64
	state     atomic.Pointer[kongPlanState]
}

// NewKongPlanStore 创建存储；Start 之前状态为空，选号按现状调度。
func NewKongPlanStore(repo KongPlanRepository) *KongPlanStore {
	return &KongPlanStore{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

// Start 在启动时加载一次，最多等 kongPlanLoadTimeout，不让表锁之类的等待挡住网关启动。失败只记日志：之后每次
// 读写都会再试，加载成功之前选号按现状调度、接口返回 503。
func (s *KongPlanStore) Start(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, kongPlanLoadTimeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadedLocked(ctx); err != nil {
		log.Printf("kong plan: 启动时加载失败，之后读写时重试: %v", err)
	}
}

func (s *KongPlanStore) loadedLocked(ctx context.Context) (*kongPlanState, error) {
	if st := s.state.Load(); st != nil && !s.stale.Load() {
		return st, nil
	}
	st, err := s.load(ctx)
	if err != nil {
		return nil, infraerrors.ServiceUnavailable(KongPlanReasonUnavailable, "计划组件的状态暂时读不出来").WithCause(err)
	}
	s.state.Store(st)
	s.stale.Store(false)
	return st, nil
}

// kongPlanReloadGap 是选号路径触发的后台重载失败后，再次尝试前至少等待的时间。
const kongPlanReloadGap = 5 * time.Second

// current 给选号路径用：只读内存里的状态，不访问数据库。写入结果不确定（stale）之后，内存里可能还是那次写入之前的
// 状态；启动时首次加载失败则还没有状态。这两种情况都在后台重载一次、不阻塞请求；重载完成之前沿用现有状态（没有
// 状态时按现状调度）。
func (s *KongPlanStore) current() *kongPlanState {
	st := s.state.Load()
	if (st == nil || s.stale.Load()) && time.Now().UnixNano()-s.reloadAt.Load() >= int64(kongPlanReloadGap) && s.reloading.CompareAndSwap(false, true) {
		s.reloadAt.Store(time.Now().UnixNano())
		go func() {
			defer s.reloading.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), kongPlanLoadTimeout)
			defer cancel()
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, err := s.loadedLocked(ctx); err != nil {
				log.Printf("kong plan: 状态重载失败，稍后再试: %v", err)
			}
		}()
	}
	return st
}

// read 给只读接口用：状态在且不需要重载时不加锁。先看重载标记、再取状态：重载是先换状态、后清标记，
// 这个顺序保证看到"不需要重载"之后取到的状态不早于那次重载。
func (s *KongPlanStore) read(ctx context.Context) (*kongPlanState, error) {
	if !s.stale.Load() {
		if st := s.state.Load(); st != nil {
			return st, nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadedLocked(ctx)
}

func (s *KongPlanStore) load(ctx context.Context) (*kongPlanState, error) {
	rows, caller, err := s.repo.LoadKongPlan(ctx)
	if err != nil {
		return nil, err
	}
	st := kongPlanDefaultState()
	for _, row := range rows {
		var target any
		switch row.Kind {
		case kongPlanKindPublish:
			target = &st.publish
		case kongPlanKindDispatch:
			st.dispatch = &KongPlanDispatch{}
			target = st.dispatch
		case kongPlanKindControl:
			target = &st.control
		case kongPlanKindForecast:
			st.forecast = &KongPlanForecast{}
			target = st.forecast
		default:
			continue
		}
		// 内容读不出来时整体失败，不能退回缺省值：缺省的代次与修订号会让旧的写入重新被接受。
		if err := json.Unmarshal(row.Content, target); err != nil {
			return nil, fmt.Errorf("kong plan: %s 行内容无法解析: %w", row.Kind, err)
		}
	}
	if st.dispatch != nil {
		st.dispatch.buildIndex()
	}
	if st.control.Allow == nil {
		st.control.Allow = []int64{}
	}
	if st.control.OverflowMargin == nil {
		st.control.OverflowMargin = map[string]int{"default": 0}
	}
	st.caller = caller
	return st, nil
}

// commitLocked 在锁内保存并换上新状态。保存失败时标记重载：事务报错不代表一定没提交。
func (s *KongPlanStore) commitLocked(ctx context.Context, next *kongPlanState, save func(context.Context) error) error {
	// 请求断开不应打断进行中的事务。
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kongPlanWriteTimeout)
	defer cancel()
	if err := save(wctx); err != nil {
		s.stale.Store(true)
		return infraerrors.InternalServer("KONG_PLAN_WRITE_FAILED", "写入失败").WithCause(err)
	}
	s.state.Store(next)
	return nil
}

func (s *KongPlanStore) saveRows(rows ...KongPlanRow) func(context.Context) error {
	return func(ctx context.Context) error { return s.repo.SaveKongPlanRows(ctx, rows...) }
}

func kongPlanMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("kong plan: 序列化状态失败: %v", err))
	}
	return b
}

func kongPlanPublishRow(p KongPlanPublishState) KongPlanRow {
	gen, seq := p.Generation, p.LastSeq
	return KongPlanRow{Kind: kongPlanKindPublish, Generation: &gen, Seq: &seq, Content: kongPlanMarshal(p)}
}

func kongPlanDispatchRow(d *KongPlanDispatch) KongPlanRow {
	gen, seq := d.Generation, d.Seq
	return KongPlanRow{Kind: kongPlanKindDispatch, Generation: &gen, Seq: &seq, SHA256: d.SHA256, Content: kongPlanMarshal(d)}
}

func kongPlanControlRow(c KongPlanControl) KongPlanRow {
	rev := c.Revision
	return KongPlanRow{Kind: kongPlanKindControl, Revision: &rev, SHA256: c.SyncedSHA256, Content: kongPlanMarshal(c)}
}

func kongPlanConflict(reason, message string, md map[string]string) error {
	return infraerrors.Conflict(reason, message).WithMetadata(md)
}

func kongPlanPublishMetadata(p KongPlanPublishState) map[string]string {
	return map[string]string{
		"generation": strconv.FormatInt(p.Generation, 10),
		"enabled":    strconv.FormatBool(p.Enabled),
		"seq":        strconv.FormatInt(p.LastSeq, 10),
	}
}

// PerPoint 返回人工约束里的换算率（一点合多少 credits）；未知或状态还没加载时为 nil。换成本账号百分点还要乘账号的
// 套餐系数（AccountK）。
func (s *KongPlanStore) PerPoint() *float64 {
	st := s.current()
	if st == nil || st.control.PerPoint == nil {
		return nil
	}
	v := *st.control.PerPoint
	return &v
}

// AccountK 从最近一次保存的慢速部分取账号在 at 时刻的套餐系数。没有慢速部分、或其中没有这个账号时 ok 为 false。
// 慢速部分过期也照样取：时间线里写着已知的切换，边车停了也能按时刻取到正确的系数。
func (s *KongPlanStore) AccountK(id int64, at time.Time) (float64, bool) {
	k, _, ok := s.accountK(id, at)
	return k, ok
}

// AccountMultiple 返回账号在 at 时刻的规格倍数 = K × 单位倍数（老 pro 20、新 pro 10）。它与内部单位无关：单位从 x20
// 改成 x10 时所有账号的 K 一起翻倍，倍数不变。取不到的情形同 AccountK。
func (s *KongPlanStore) AccountMultiple(id int64, at time.Time) (float64, bool) {
	k, unit, ok := s.accountK(id, at)
	return k * unit, ok
}

// accountK 从同一份慢速部分取账号的套餐系数与单位倍数。
func (s *KongPlanStore) accountK(id int64, at time.Time) (k, unit float64, ok bool) {
	if s == nil {
		return 0, 0, false
	}
	st := s.current()
	if st == nil || st.forecast == nil {
		return 0, 0, false
	}
	for _, a := range st.forecast.Accounts {
		if a.ID == id {
			return a.kAt(at), st.forecast.unitMultiple(), true
		}
	}
	return 0, 0, false
}

// KongPlanDispatchResult 是发布的成功响应。
type KongPlanDispatchResult struct {
	Accepted          bool       `json:"accepted"`
	Idempotent        bool       `json:"idempotent"`
	Generation        int64      `json:"generation"`
	Seq               int64      `json:"seq"`
	AcceptedAt        time.Time  `json:"accepted_at"`
	SnapshotExpiresAt *time.Time `json:"snapshot_expires_at"`
	IgnoredIDs        []int64    `json:"ignored_ids"`
}

func kongPlanDispatchResult(d *KongPlanDispatch, idempotent bool) *KongPlanDispatchResult {
	r := &KongPlanDispatchResult{
		Accepted:   true,
		Idempotent: idempotent,
		Generation: d.Generation,
		Seq:        d.Seq,
		AcceptedAt: d.AcceptedAt,
		IgnoredIDs: d.IgnoredIDs,
	}
	if d.Snapshot != nil {
		at := d.Snapshot.ExpiresAt
		r.SnapshotExpiresAt = &at
	}
	if r.IgnoredIDs == nil {
		r.IgnoredIDs = []int64{}
	}
	return r
}

// PutDispatch 接受一次发布：状态检查（停用、代次、序号）在先，只对新序号做动态校验。
func (s *KongPlanStore) PutDispatch(ctx context.Context, req *KongPlanDispatchRequest) (*KongPlanDispatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadedLocked(ctx)
	if err != nil {
		return nil, err
	}
	pub := st.publish
	switch {
	case !pub.Enabled:
		return nil, kongPlanConflict(KongPlanReasonPublishDisabled, "发布已停用", kongPlanPublishMetadata(pub))
	case req.Generation != pub.Generation:
		return nil, kongPlanConflict(KongPlanReasonGenerationMismatch, "发布代次与网关不一致", kongPlanPublishMetadata(pub))
	case req.Seq < pub.LastSeq:
		return nil, kongPlanConflict(KongPlanReasonSeqStale, "发布序号小于网关现存", kongPlanPublishMetadata(pub))
	case req.Seq == pub.LastSeq:
		if req.SHA256 == pub.LastSHA256 && st.dispatch != nil {
			return kongPlanDispatchResult(st.dispatch, true), nil
		}
		return nil, kongPlanConflict(KongPlanReasonSeqConflict, "同一发布序号的内容不同", kongPlanPublishMetadata(pub))
	}

	now := s.now()
	if !req.Snapshot.ExpiresAt.After(now) || req.Snapshot.ExpiresAt.After(now.Add(kongPlanSnapshotMaxTTL)) {
		return nil, infraerrors.BadRequest(KongPlanReasonSnapshotExpiry, "快照有效期必须晚于此刻、且不超过 6 小时").
			WithMetadata(map[string]string{"field": "snapshot.expires_at"})
	}
	ignored, err := s.checkAccounts(ctx, req)
	if err != nil {
		return nil, err
	}
	snapshot := req.Snapshot
	d := &KongPlanDispatch{
		Generation:  req.Generation,
		Seq:         req.Seq,
		PlanVersion: req.PlanVersion,
		CycleID:     req.CycleID,
		SHA256:      req.SHA256,
		AcceptedAt:  now,
		HasFallback: true,
		Fallback:    req.Fallback,
		Snapshot:    &snapshot,
		IgnoredIDs:  ignored,
	}
	d.buildIndex()
	nextPub := pub
	nextPub.LastSeq, nextPub.LastSHA256 = req.Seq, req.SHA256
	next := *st
	next.publish, next.dispatch = nextPub, d
	if err := s.commitLocked(ctx, &next, s.saveRows(kongPlanPublishRow(nextPub), kongPlanDispatchRow(d))); err != nil {
		return nil, err
	}
	return kongPlanDispatchResult(d, false), nil
}

// checkAccounts 返回不存在或已删除的账号编号；非 OAuth 账号带 credits 时拒收。
func (s *KongPlanStore) checkAccounts(ctx context.Context, req *KongPlanDispatchRequest) ([]int64, error) {
	idSet := make(map[int64]bool)
	for _, e := range req.Fallback {
		idSet[e.ID] = true
	}
	for _, e := range req.Snapshot.Accounts {
		idSet[e.ID] = true
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	kinds := map[int64]KongPlanAccountKind{}
	if len(ids) > 0 {
		var err error
		if kinds, err = s.repo.KongPlanAccountKinds(ctx, ids); err != nil {
			return nil, infraerrors.InternalServer("KONG_PLAN_ACCOUNT_LOOKUP_FAILED", "读取账号失败").WithCause(err)
		}
	}
	check := func(entries []KongPlanEntry, field string) error {
		for i, e := range entries {
			kind, ok := kinds[e.ID]
			if e.Credits != nil && ok && (kind.Platform != PlatformOpenAI || kind.Type != AccountTypeOAuth) {
				return infraerrors.BadRequest(KongPlanReasonCreditsNotOAuth, fmt.Sprintf("账号 %d 不是 OpenAI OAuth 账号，不能带 credits", e.ID)).
					WithMetadata(map[string]string{"field": fmt.Sprintf("%s[%d].credits", field, i), "account_id": strconv.FormatInt(e.ID, 10)})
			}
		}
		return nil
	}
	if err := check(req.Fallback, "fallback"); err != nil {
		return nil, err
	}
	if err := check(req.Snapshot.Accounts, "snapshot.accounts"); err != nil {
		return nil, err
	}
	ignored := []int64{}
	for _, id := range ids {
		if _, ok := kinds[id]; !ok {
			ignored = append(ignored, id)
		}
	}
	return ignored, nil
}

// KongPlanDispatchView 是 GET /dispatch 的响应；从未发布时各项为 null。
type KongPlanDispatchView struct {
	Generation        *int64            `json:"generation"`
	Seq               *int64            `json:"seq"`
	PlanVersion       *int64            `json:"plan_version"`
	CycleID           *string           `json:"cycle_id"`
	ContentSHA256     *string           `json:"content_sha256"`
	AcceptedAt        *time.Time        `json:"accepted_at"`
	InUse             string            `json:"in_use"`
	SnapshotExpiresAt *time.Time        `json:"snapshot_expires_at"`
	Fallback          []KongPlanEntry   `json:"fallback"`
	Snapshot          *KongPlanSnapshot `json:"snapshot"`
}

// GetDispatch 返回最近一次接受的发布与网关此刻所用的输入。
func (s *KongPlanStore) GetDispatch(ctx context.Context) (*KongPlanDispatchView, error) {
	st, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	v := &KongPlanDispatchView{InUse: st.inUse(s.now())}
	d := st.dispatch
	if d == nil {
		return v, nil
	}
	v.Generation, v.Seq, v.PlanVersion = &d.Generation, &d.Seq, &d.PlanVersion
	v.CycleID, v.ContentSHA256, v.AcceptedAt = &d.CycleID, &d.SHA256, &d.AcceptedAt
	if d.HasFallback {
		v.Fallback = d.Fallback
		if v.Fallback == nil {
			v.Fallback = []KongPlanEntry{}
		}
	}
	if d.Snapshot != nil {
		v.Snapshot = d.Snapshot
		v.SnapshotExpiresAt = &d.Snapshot.ExpiresAt
	}
	return v, nil
}

// KongPlanPublishView 是发布状态的响应。
type KongPlanPublishView struct {
	Generation      int64                    `json:"generation"`
	Enabled         bool                     `json:"enabled"`
	ChangedAt       *time.Time               `json:"changed_at"`
	Note            string                   `json:"note"`
	LastSeq         int64                    `json:"last_seq"`
	InUse           string                   `json:"in_use"`
	Dispatch        *KongPlanPublishDispatch `json:"dispatch"`
	ControlRevision int64                    `json:"control_revision"`
}

// KongPlanPublishDispatch 是发布状态里对最近一次发布的摘要。
type KongPlanPublishDispatch struct {
	Seq               int64      `json:"seq"`
	PlanVersion       int64      `json:"plan_version"`
	CycleID           string     `json:"cycle_id"`
	AcceptedAt        time.Time  `json:"accepted_at"`
	SnapshotExpiresAt *time.Time `json:"snapshot_expires_at"`
	HasFallback       bool       `json:"has_fallback"`
	HasSnapshot       bool       `json:"has_snapshot"`
}

func (s *KongPlanStore) publishView(st *kongPlanState) *KongPlanPublishView {
	p := st.publish
	v := &KongPlanPublishView{
		Generation:      p.Generation,
		Enabled:         p.Enabled,
		ChangedAt:       p.ChangedAt,
		Note:            p.Note,
		LastSeq:         p.LastSeq,
		InUse:           st.inUse(s.now()),
		ControlRevision: st.control.Revision,
	}
	if d := st.dispatch; d != nil {
		v.Dispatch = &KongPlanPublishDispatch{
			Seq:         d.Seq,
			PlanVersion: d.PlanVersion,
			CycleID:     d.CycleID,
			AcceptedAt:  d.AcceptedAt,
			HasFallback: d.HasFallback,
			HasSnapshot: d.Snapshot != nil,
		}
		if d.Snapshot != nil {
			at := d.Snapshot.ExpiresAt
			v.Dispatch.SnapshotExpiresAt = &at
		}
	}
	return v
}

// GetPublish 返回发布状态，边车每个周期读一次。
func (s *KongPlanStore) GetPublish(ctx context.Context) (*KongPlanPublishView, error) {
	st, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	return s.publishView(st), nil
}

// PutPublish 停用、恢复或清空发布。每次都让代次加一，此前发出、尚未到达的发布因此一律被拒。
func (s *KongPlanStore) PutPublish(ctx context.Context, req *KongPlanPublishRequest) (*KongPlanPublishView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadedLocked(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	pub := st.publish
	pub.Generation++
	pub.Enabled = req.Action == "enable"
	pub.ChangedAt = &now
	pub.Note = req.Note
	next := *st
	next.publish = pub
	rows := []KongPlanRow{kongPlanPublishRow(pub)}
	if req.Clear != "none" && st.dispatch != nil {
		d := *st.dispatch
		d.Snapshot = nil
		if req.Clear == "all" {
			d.HasFallback, d.Fallback = false, nil
		}
		d.buildIndex()
		next.dispatch = &d
		rows = append(rows, kongPlanDispatchRow(&d))
	}
	if err := s.commitLocked(ctx, &next, s.saveRows(rows...)); err != nil {
		return nil, err
	}
	return s.publishView(&next), nil
}

// KongPlanControlView 是人工约束的响应。
type KongPlanControlView struct {
	Idempotent       bool              `json:"idempotent,omitempty"`
	Revision         int64             `json:"revision"`
	CreditsMode      string            `json:"credits_mode"`
	Allow            []int64           `json:"allow"`
	Floor            float64           `json:"floor"`
	PerPoint         *float64          `json:"per_point"`
	OverflowMargin   map[string]int    `json:"overflow_margin"`
	FileHold         *KongPlanFileHold `json:"file_hold"`
	LatchedHolds     []KongPlanHold    `json:"latched_holds"`
	ReleasedTriggers []string          `json:"released_triggers"`
	UpdatedAt        *time.Time        `json:"updated_at"`
}

func kongPlanControlView(c KongPlanControl) *KongPlanControlView {
	v := &KongPlanControlView{
		Revision:         c.Revision,
		CreditsMode:      c.CreditsMode,
		Allow:            c.Allow,
		Floor:            c.Floor,
		PerPoint:         c.PerPoint,
		OverflowMargin:   c.OverflowMargin,
		FileHold:         c.FileHold,
		LatchedHolds:     c.LatchedHolds,
		ReleasedTriggers: make([]string, 0, len(c.Releases)),
		UpdatedAt:        c.UpdatedAt,
	}
	if v.LatchedHolds == nil {
		v.LatchedHolds = []KongPlanHold{}
	}
	for _, r := range c.Releases {
		v.ReleasedTriggers = append(v.ReleasedTriggers, r.Trigger)
	}
	return v
}

// GetControl 返回人工约束。
func (s *KongPlanStore) GetControl(ctx context.Context) (*KongPlanControlView, error) {
	st, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	return kongPlanControlView(st.control), nil
}

// PutControl 按约束修订号整体替换边车同步的部分；锁存暂停不动。
func (s *KongPlanStore) PutControl(ctx context.Context, req *KongPlanControlRequest) (*KongPlanControlView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadedLocked(ctx)
	if err != nil {
		return nil, err
	}
	c := st.control
	md := map[string]string{"revision": strconv.FormatInt(c.Revision, 10)}
	switch {
	case req.Revision < c.Revision:
		return nil, kongPlanConflict(KongPlanReasonRevisionStale, "约束修订号小于网关现存", md)
	case req.Revision == c.Revision:
		if req.SHA256 == c.SyncedSHA256 {
			v := kongPlanControlView(c)
			v.Idempotent = true
			return v, nil
		}
		return nil, kongPlanConflict(KongPlanReasonRevisionConflict, "同一约束修订号的内容不同", md)
	}
	now := s.now()
	c.Revision, c.CreditsMode, c.Allow, c.Floor = req.Revision, req.CreditsMode, req.Allow, req.Floor
	c.PerPoint, c.OverflowMargin, c.FileHold, c.SyncedSHA256 = req.PerPoint, req.OverflowMargin, req.FileHold, req.SHA256
	c.UpdatedAt = &now
	next := *st
	next.control = c
	if err := s.commitLocked(ctx, &next, s.saveRows(kongPlanControlRow(c))); err != nil {
		return nil, err
	}
	return kongPlanControlView(c), nil
}

// KongPlanHoldResult 是设置锁存暂停的结果：编号已解除过时为 released，不生效。
type KongPlanHoldResult struct {
	State      string `json:"state"`
	Idempotent bool   `json:"idempotent,omitempty"`
}

func kongPlanReleased(c KongPlanControl, trigger string) bool {
	for _, r := range c.Releases {
		if r.Trigger == trigger {
			return true
		}
	}
	return false
}

// Hold 设置一个锁存暂停。已解除的编号迟到时不再生效。
func (s *KongPlanStore) Hold(ctx context.Context, req *KongPlanHoldRequest) (*KongPlanHoldResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadedLocked(ctx)
	if err != nil {
		return nil, err
	}
	c := st.control
	if kongPlanReleased(c, req.Trigger) {
		return &KongPlanHoldResult{State: "released"}, nil
	}
	for _, h := range c.LatchedHolds {
		if h.Trigger == req.Trigger {
			return &KongPlanHoldResult{State: "active", Idempotent: true}, nil
		}
	}
	now := s.now()
	holds := make([]KongPlanHold, 0, len(c.LatchedHolds)+1)
	holds = append(holds, c.LatchedHolds...)
	c.LatchedHolds = append(holds, KongPlanHold{Trigger: req.Trigger, Reason: req.Reason, Since: now, Note: req.Note})
	c.UpdatedAt = &now
	next := *st
	next.control = c
	if err := s.commitLocked(ctx, &next, s.saveRows(kongPlanControlRow(c))); err != nil {
		return nil, err
	}
	return &KongPlanHoldResult{State: "active"}, nil
}

// KongPlanReleaseResult 是解除的结果：本次解除的编号与此后仍生效的编号。
type KongPlanReleaseResult struct {
	Released []string `json:"released"`
	Active   []string `json:"active"`
}

// Release 按编号或按原因解除锁存暂停。指定的编号原本不存在也照样记下，之后迟到的同编号设置不再生效。
func (s *KongPlanStore) Release(ctx context.Context, req *KongPlanReleaseRequest) (*KongPlanReleaseResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadedLocked(ctx)
	if err != nil {
		return nil, err
	}
	c := st.control
	targets := req.Triggers
	if req.Reason != "" {
		targets = nil
		for _, h := range c.LatchedHolds {
			if h.Reason == req.Reason {
				targets = append(targets, h.Trigger)
			}
		}
	}
	release := make(map[string]bool, len(targets))
	for _, t := range targets {
		release[t] = true
	}
	now := s.now()
	changed := false
	holds := make([]KongPlanHold, 0, len(c.LatchedHolds))
	for _, h := range c.LatchedHolds {
		if release[h.Trigger] {
			changed = true
			continue
		}
		holds = append(holds, h)
	}
	releases := append([]KongPlanRelease(nil), c.Releases...)
	for _, t := range targets {
		if !kongPlanReleased(c, t) {
			releases = append(releases, KongPlanRelease{Trigger: t, ReleasedAt: now, Note: req.Note})
			changed = true
		}
	}
	result := &KongPlanReleaseResult{Released: append([]string{}, targets...), Active: make([]string, 0, len(holds))}
	for _, h := range holds {
		result.Active = append(result.Active, h.Trigger)
	}
	if !changed {
		return result, nil
	}
	c.LatchedHolds, c.Releases, c.UpdatedAt = holds, releases, &now
	next := *st
	next.control = c
	if err := s.commitLocked(ctx, &next, s.saveRows(kongPlanControlRow(c))); err != nil {
		return nil, err
	}
	return result, nil
}

// PutForecast 整份替换慢速部分；源事实时刻早于现存的拒收。
func (s *KongPlanStore) PutForecast(ctx context.Context, f *KongPlanForecast) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadedLocked(ctx)
	if err != nil {
		return nil, err
	}
	if cur := st.forecast; cur != nil && f.ComputedAt.Before(cur.ComputedAt) {
		return nil, kongPlanConflict(KongPlanReasonForecastStale, "慢速部分的 computed_at 早于网关现存",
			map[string]string{"computed_at": cur.ComputedAt.Format(time.RFC3339Nano)})
	}
	stored := *f
	stored.ReceivedAt = s.now()
	next := *st
	next.forecast = &stored
	row := KongPlanRow{Kind: kongPlanKindForecast, Content: kongPlanMarshal(&stored)}
	if err := s.commitLocked(ctx, &next, s.saveRows(row)); err != nil {
		return nil, err
	}
	return map[string]any{"accepted": true, "computed_at": stored.ComputedAt}, nil
}

// KongPoolCallerView 是调用方声明的响应；过了有效期的按 normal 返回并带 expired。
type KongPoolCallerView struct {
	State            string     `json:"state"`
	DesiredPPPerHour *float64   `json:"desired_pp_per_hour"`
	Until            *time.Time `json:"until"`
	UpdatedAt        *time.Time `json:"updated_at"`
	APIKeyID         *int64     `json:"api_key_id"`
	Note             string     `json:"note"`
	Expired          bool       `json:"expired"`
}

func kongPoolCallerView(c *KongPoolCaller, now time.Time) *KongPoolCallerView {
	if c == nil {
		return &KongPoolCallerView{State: KongPoolCallerNormal}
	}
	until, updated := c.Until, c.UpdatedAt
	v := &KongPoolCallerView{
		State:            c.State,
		DesiredPPPerHour: c.DesiredPPPerHour,
		Until:            &until,
		UpdatedAt:        &updated,
		APIKeyID:         c.APIKeyID,
		Note:             c.Note,
	}
	if !now.Before(c.Until) {
		v.State, v.Expired = KongPoolCallerNormal, true
	}
	return v
}

// GetCaller 返回调用方声明，边车读进需求估计。
func (s *KongPlanStore) GetCaller(ctx context.Context) (*KongPoolCallerView, error) {
	st, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	return kongPoolCallerView(st.caller, s.now()), nil
}

// PutCaller 写入调用方声明。只有一个调用方，后写覆盖。
func (s *KongPlanStore) PutCaller(ctx context.Context, req *KongPoolCallerRequest, apiKeyID *int64) (*KongPoolCallerView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.loadedLocked(ctx)
	if err != nil {
		return nil, err
	}
	// 调用方声明存在 TIMESTAMPTZ 列里（微秒精度），内存里用同样的精度，重启前后读到的一致
	now := s.now().Truncate(time.Microsecond)
	caller := KongPoolCaller{
		State:            req.State,
		DesiredPPPerHour: req.DesiredPPPerHour,
		Until:            now.Add(time.Duration(req.TTLHours) * time.Hour),
		Note:             req.Note,
		APIKeyID:         apiKeyID,
		UpdatedAt:        now,
	}
	next := *st
	next.caller = &caller
	save := func(ctx context.Context) error { return s.repo.SaveKongPoolCaller(ctx, caller) }
	if err := s.commitLocked(ctx, &next, save); err != nil {
		return nil, err
	}
	return kongPoolCallerView(&caller, now), nil
}
