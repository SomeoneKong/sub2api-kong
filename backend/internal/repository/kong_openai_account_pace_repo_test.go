//go:build unit

package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newKongPaceStoreTest(t *testing.T) (service.KongPaceStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewKongPaceStore(rdb), mr
}

func TestKongPaceStore_SaveLoadDelete(t *testing.T) {
	ctx := context.Background()
	store, mr := newKongPaceStoreTest(t)

	require.NoError(t, store.SaveAccountStates(ctx, map[int64][]byte{1: []byte(`{"a":1}`), 7: []byte(`{"a":7}`)}, nil))
	require.NoError(t, mr.Set(kongPaceAccountKeyPrefix+"junk", "x"))
	got, err := store.LoadAccountStates(ctx)
	require.NoError(t, err)
	require.Equal(t, map[int64][]byte{1: []byte(`{"a":1}`), 7: []byte(`{"a":7}`)}, got)
	require.Zero(t, mr.TTL(kongPaceAccountKeyPrefix+"1"), "account state must not expire")

	require.NoError(t, store.SaveAccountStates(ctx, map[int64][]byte{1: []byte(`{"a":2}`)}, []int64{7}))
	got, err = store.LoadAccountStates(ctx)
	require.NoError(t, err)
	require.Equal(t, map[int64][]byte{1: []byte(`{"a":2}`)}, got)
}

func TestKongPaceStore_PublishReplacesState(t *testing.T) {
	ctx := context.Background()
	store, mr := newKongPaceStoreTest(t)

	require.NoError(t, store.PublishState(ctx, map[int64][]byte{1: []byte("one"), 2: []byte("two")}, []byte("m1"), 10*time.Minute))
	require.NoError(t, store.PublishState(ctx, map[int64][]byte{2: []byte("two'")}, []byte("m2"), 10*time.Minute))

	fields, err := mr.HKeys(kongPaceStateKey)
	require.NoError(t, err)
	require.Equal(t, []string{"2"}, fields, "accounts gone from the latest publish must disappear")
	require.Equal(t, "two'", mr.HGet(kongPaceStateKey, "2"))
	meta, err := mr.Get(kongPaceMetaKey)
	require.NoError(t, err)
	require.Equal(t, "m2", meta)
	require.Equal(t, 10*time.Minute, mr.TTL(kongPaceStateKey))
	require.Equal(t, 10*time.Minute, mr.TTL(kongPaceMetaKey))

	require.NoError(t, store.PublishState(ctx, nil, []byte("m3"), 10*time.Minute))
	require.False(t, mr.Exists(kongPaceStateKey))
}

func TestKongPaceRepository_ListParsesReadings(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("FROM accounts a")).
		WithArgs(service.PlatformOpenAI, service.AccountTypeOAuth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan", "concurrency", "exp", "used", "reset", "window", "updated", "last_used"}).
			AddRow(int64(3), "pro", 8, "2026-10-01T00:00:00Z", "43.5", "2026-09-26T08:00:00.123Z", "10080", "2026-09-23T16:59:00+08:00", time.Date(2026, 9, 23, 8, 58, 0, 0, time.UTC)).
			AddRow(int64(4), "", 5, "", "NaN", "bad", "", "", nil))

	rows, err := NewKongPaceRepository(db).ListOpenAIOAuthAccounts(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 2)

	r := rows[0]
	require.Equal(t, int64(3), r.ID)
	require.Equal(t, "pro", r.Plan)
	require.Equal(t, 8, r.Concurrency)
	require.Equal(t, 43.5, *r.UsedPercent)
	require.Equal(t, 10080, *r.WindowMinutes)
	require.True(t, r.ResetAt.Equal(time.Date(2026, 9, 26, 8, 0, 0, 123e6, time.UTC)))
	require.True(t, r.UsageUpdatedAt.Equal(time.Date(2026, 9, 23, 8, 59, 0, 0, time.UTC)))
	require.True(t, r.SubscriptionExpiresAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)))
	require.True(t, r.LastUsedAt.Equal(time.Date(2026, 9, 23, 8, 58, 0, 0, time.UTC)))

	empty := rows[1]
	require.Nil(t, empty.UsedPercent)
	require.Nil(t, empty.ResetAt)
	require.Nil(t, empty.WindowMinutes)
	require.Nil(t, empty.UsageUpdatedAt)
	require.Nil(t, empty.SubscriptionExpiresAt)
	require.Nil(t, empty.LastUsedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKongPaceRepository_InsertDecisionsNullsAbsentFields(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	at := time.Date(2026, 9, 23, 17, 0, 0, 0, time.UTC)
	full := service.KongPaceDecisionRecord{
		CreatedAt: at, RequestID: "req", ClientRequestID: "cli", UserID: 5, GroupID: 2, Model: "gpt", SessionHash: "h",
		Reason: "new_session", Applied: true, ConfigVersion: "abc", Config: []byte(`{"k":1}`), SnapshotAgeMs: 1200,
		Rounds: []byte(`[]`), Outcome: "acquired", AccountID: 3, AttemptIndex: 0,
	}
	bare := service.KongPaceDecisionRecord{
		CreatedAt: at, Reason: "no_session", NotAppliedReason: "snapshot_missing", SnapshotAgeMs: -1,
		Outcome: "no_available", AttemptIndex: -1,
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO kong_openai_pace_decisions")).
		WithArgs(
			at, "req", "cli", int64(5), int64(2), "gpt", "h", "new_session", true, nil, "abc", `{"k":1}`, int64(1200), `[]`, "acquired", int64(3), 0,
			at, nil, nil, nil, nil, nil, nil, "no_session", false, "snapshot_missing", nil, nil, nil, nil, "no_available", nil, nil,
		).
		WillReturnResult(sqlmock.NewResult(0, 2))

	require.NoError(t, NewKongPaceRepository(db).InsertDecisions(context.Background(), []service.KongPaceDecisionRecord{full, bare}))
	require.NoError(t, mock.ExpectationsWereMet())
}
