//go:build unit

package service

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 共用夹具 testdata/kong_plan/balance_timelines.json：边车的余额账本跑同一份。夹具里的账号套餐系数都是 1，
// 点与本账号百分点相同。

type kongPlanTimelineFile struct {
	Cases []struct {
		Name     string   `json:"name"`
		PerPoint *float64 `json:"per_point"`
		Events   []struct {
			Type    string                     `json:"type"`
			At      string                     `json:"at"`
			Seq     int64                      `json:"seq"`
			Balance *float64                   `json:"balance"`
			Expect  map[string]json.RawMessage `json:"expect"`
		} `json:"events"`
	} `json:"cases"`
}

type kongPlanTimelineRise struct {
	At     string  `json:"at"`
	Points float64 `json:"points"`
}

func TestKongPlanBalanceTimelines(t *testing.T) {
	raw, err := os.ReadFile("testdata/kong_plan/balance_timelines.json")
	require.NoError(t, err)
	var file kongPlanTimelineFile
	require.NoError(t, json.Unmarshal(raw, &file))
	require.NotEmpty(t, file.Cases)

	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			st := &kongPaceAccountState{}
			entries := map[int64]*KongPlanTierEntry{}
			var now time.Time
			for i, ev := range tc.Events {
				if ev.At != "" {
					now, err = time.Parse(time.RFC3339, ev.At)
					require.NoError(t, err)
				}
				switch ev.Type {
				case "obs":
					hasOpen := false
					for _, e := range entries {
						hasOpen = hasOpen || !e.Settled
					}
					kongPlanApplyObservation(st, kongPlanObservation{At: now, Balance: ev.Balance}, tc.PerPoint, 1, hasOpen)
				case "enter":
					if entries[ev.Seq] == nil {
						entries[ev.Seq] = &KongPlanTierEntry{Seq: ev.Seq, EnteredAt: now}
					}
				case "leave":
					if e := entries[ev.Seq]; e != nil && e.LeftAt == nil {
						at := now
						e.LeftAt = &at
					}
				case "tick":
				case "restart":
					b, err := json.Marshal(st)
					require.NoError(t, err)
					st = &kongPaceAccountState{}
					require.NoError(t, json.Unmarshal(b, st))
					eb, err := json.Marshal(entries)
					require.NoError(t, err)
					entries = map[int64]*KongPlanTierEntry{}
					require.NoError(t, json.Unmarshal(eb, &entries))
				default:
					t.Fatalf("未知事件 %q", ev.Type)
				}
				if ev.Type != "restart" {
					for _, e := range entries {
						if kongPlanSettleDue(*e, st, now) {
							e.Settled = true
						}
					}
				}
				if ev.Expect != nil {
					kongPlanCheckTimeline(t, i, ev.Expect, st, entries, now)
				}
			}
		})
	}
}

func kongPlanCheckTimeline(t *testing.T, step int, expect map[string]json.RawMessage, st *kongPaceAccountState, entries map[int64]*KongPlanTierEntry, now time.Time) {
	t.Helper()
	list := make([]KongPlanTierEntry, 0, len(entries))
	var open []int64
	for _, e := range entries {
		list = append(list, *e)
		if !e.Settled {
			open = append(open, e.Seq)
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i] < open[j] })
	for key, want := range expect {
		switch key {
		case "credits_rises":
			var w []kongPlanTimelineRise
			require.NoError(t, json.Unmarshal(want, &w))
			got := []kongPlanTimelineRise{}
			for _, r := range st.Rises {
				if r.Credits {
					got = append(got, kongPlanTimelineRise{At: r.At.UTC().Format(time.RFC3339), Points: r.Points})
				}
			}
			if w == nil {
				w = []kongPlanTimelineRise{}
			}
			require.Equal(t, w, got, "第 %d 步 credits_rises", step+1)
		case "unknown_until":
			var w *string
			require.NoError(t, json.Unmarshal(want, &w))
			if w == nil {
				require.True(t, st.UnknownUntil.IsZero(), "第 %d 步 unknown_until 应为空，实际 %v", step+1, st.UnknownUntil)
			} else {
				require.Equal(t, *w, st.UnknownUntil.UTC().Format(time.RFC3339), "第 %d 步 unknown_until", step+1)
			}
		case "open":
			var w []int64
			require.NoError(t, json.Unmarshal(want, &w))
			if len(w) == 0 {
				require.Empty(t, open, "第 %d 步 open", step+1)
			} else {
				require.Equal(t, w, open, "第 %d 步 open", step+1)
			}
		case "balance":
			var w *struct {
				At    string  `json:"at"`
				Value float64 `json:"value"`
			}
			require.NoError(t, json.Unmarshal(want, &w))
			if w == nil {
				require.Nil(t, st.Balance, "第 %d 步 balance", step+1)
			} else {
				require.NotNil(t, st.Balance, "第 %d 步 balance", step+1)
				require.Equal(t, w.At, st.Balance.At.UTC().Format(time.RFC3339), "第 %d 步 balance.at", step+1)
				require.Equal(t, w.Value, st.Balance.Value, "第 %d 步 balance.value", step+1)
			}
		case "at_line":
			var w bool
			require.NoError(t, json.Unmarshal(want, &w))
			require.Equal(t, w, kongPlanAtLine(list, st, now), "第 %d 步 at_line", step+1)
		default:
			t.Fatalf("未知期望项 %q", key)
		}
	}
}
