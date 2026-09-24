//go:build unit

package service

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

func kongTestFeaturesEqual(t *testing.T, got *KongRequestFeatures, state, reissued *string) {
	t.Helper()
	if state == nil && reissued == nil {
		if got != nil {
			t.Fatalf("应当一项都没有（nil），实得 %+v", got)
		}
		return
	}
	if got == nil {
		t.Fatal("特征不该为空")
	}
	check := func(name, fp string, n *int, want *string) {
		if want == nil {
			if fp != "" || n != nil {
				t.Errorf("%s 应缺省，实得 fp=%q len=%v", name, fp, n)
			}
			return
		}
		if n == nil || *n != len(*want) || fp != kongStateFingerprint(*want) {
			t.Errorf("%s = fp %q len %v，期望 fp %q len %d", name, fp, n, kongStateFingerprint(*want), len(*want))
		}
	}
	check("state", got.StateFP, got.StateLen, state)
	check("reissued", got.ReissuedFP, got.ReissuedLen, reissued)
}

func kongStrPtr(s string) *string { return &s }

func TestKongStateFingerprint(t *testing.T) {
	fp := kongStateFingerprint("ticket-a")
	if len(fp) != 2*kongStateFingerprintBytes || strings.Trim(fp, "0123456789abcdef") != "" {
		t.Fatalf("指纹应是 %d 位小写 hex：%q", 2*kongStateFingerprintBytes, fp)
	}
	if fp != kongStateFingerprint("ticket-a") || fp == kongStateFingerprint("ticket-b") {
		t.Fatal("同一张票指纹相同、不同的票指纹不同")
	}
	if kongStateFingerprint("") != "" {
		t.Fatal("空串没有指纹")
	}
}

// 缺省与空串是两件事：没带这个字段，与带了个空串。
func TestKongOutboundStateDistinguishesEmptyFromAbsent(t *testing.T) {
	t.Run("HTTP 头", func(t *testing.T) {
		h := http.Header{}
		if got := kongOutboundStateFromHeader(h); got.present {
			t.Error("没带这个头时必须是不存在")
		}
		if got := kongOutboundStateFromHeader(nil); got.present {
			t.Error("nil 头必须是不存在")
		}
		h.Set(openAICodexTurnStateHeader, "")
		if got := kongOutboundStateFromHeader(h); !got.present || got.value != "" {
			t.Errorf("带空串时应是「存在且为空」：%+v", got)
		}
		h.Set(openAICodexTurnStateHeader, "first")
		h.Add(openAICodexTurnStateHeader, "second")
		if got := kongOutboundStateFromHeader(h); got.value != "first" {
			t.Errorf("多个同名头时取第一个：%+v", got)
		}
	})
	t.Run("WS 帧", func(t *testing.T) {
		if got := kongOutboundStateFromWSFrame([]byte(`{"client_metadata":{}}`)); got.present {
			t.Error("键不存在时必须是不存在")
		}
		if got := kongOutboundStateFromWSFrame(nil); got.present {
			t.Error("空帧必须是不存在")
		}
		got := kongOutboundStateFromWSFrame([]byte(`{"client_metadata":{"x-codex-turn-state":""}}`))
		if !got.present || got.value != "" {
			t.Errorf("键存在且为空串时应是「存在且为空」：%+v", got)
		}
	})
	t.Run("WS map", func(t *testing.T) {
		if got := kongOutboundStateFromWSMap(map[string]any{"client_metadata": map[string]any{}}); got.present {
			t.Error("键不存在时必须是不存在")
		}
		if got := kongOutboundStateFromWSMap(map[string]any{}); got.present {
			t.Error("没有 client_metadata 时必须是不存在")
		}
		got := kongOutboundStateFromWSMap(map[string]any{"client_metadata": map[string]any{openAIWSTurnStateMetadataKey: ""}})
		if !got.present || got.value != "" {
			t.Errorf("键存在且为空串时应是「存在且为空」：%+v", got)
		}
		// 值不是字符串时上游读不出票，等同没带。
		if got := kongOutboundStateFromWSMap(map[string]any{"client_metadata": map[string]any{openAIWSTurnStateMetadataKey: 42}}); got.present {
			t.Error("值不是字符串时应是不存在")
		}
	})
	t.Run("空串上送记成长度 0、没有指纹", func(t *testing.T) {
		f := KongNewWSFrameFeatures([]byte(`{"client_metadata":{"x-codex-turn-state":""}}`)).Snapshot()
		if f == nil || f.StateLen == nil || *f.StateLen != 0 || f.StateFP != "" {
			t.Fatalf("实得 %+v", f)
		}
	})
}

func TestKongFeatureRecorder(t *testing.T) {
	t.Run("一项都没有时快照为 nil", func(t *testing.T) {
		kongTestFeaturesEqual(t, (&KongFeatureRecorder{}).Snapshot(), nil, nil)
		kongTestFeaturesEqual(t, KongNewWSFrameFeatures([]byte(`{"type":"response.create"}`)).Snapshot(), nil, nil)
		if !(*KongRequestFeatures)(nil).IsEmpty() || !(&KongRequestFeatures{}).IsEmpty() {
			t.Fatal("nil 与零值都是空")
		}
	})
	t.Run("nil 记录器上的调用都安全", func(t *testing.T) {
		var r *KongFeatureRecorder
		r.recordOutbound(kongStateObservation{value: "x", present: true})
		r.RecordOutboundIfAbsent("x")
		r.RecordReissued("x")
		if r.Snapshot() != nil {
			t.Fatal("nil 记录器的快照为 nil")
		}
	})
	t.Run("下发的票去空白、空串不记、后一张覆盖前一张", func(t *testing.T) {
		r := &KongFeatureRecorder{}
		r.RecordReissued("   ")
		kongTestFeaturesEqual(t, r.Snapshot(), nil, nil)
		r.RecordReissued(" first ")
		kongTestFeaturesEqual(t, r.Snapshot(), nil, kongStrPtr("first"))
		r.RecordReissued("second")
		kongTestFeaturesEqual(t, r.Snapshot(), nil, kongStrPtr("second"))
	})
	t.Run("连接级的值只在帧里没有时补上", func(t *testing.T) {
		r := KongNewWSFrameFeatures([]byte(`{"type":"response.create"}`))
		r.RecordOutboundIfAbsent("")
		kongTestFeaturesEqual(t, r.Snapshot(), nil, nil)
		r.RecordOutboundIfAbsent("conn")
		kongTestFeaturesEqual(t, r.Snapshot(), kongStrPtr("conn"), nil)
		r.RecordOutboundIfAbsent("other")
		kongTestFeaturesEqual(t, r.Snapshot(), kongStrPtr("conn"), nil)

		frame := KongNewWSFrameFeatures([]byte(`{"client_metadata":{"x-codex-turn-state":"frame"}}`))
		frame.RecordOutboundIfAbsent("conn")
		kongTestFeaturesEqual(t, frame.Snapshot(), kongStrPtr("frame"), nil)

		// 帧里带空串也是"帧里有"：连接级的值不能盖掉它。
		empty := KongNewWSFrameFeatures([]byte(`{"client_metadata":{"x-codex-turn-state":""}}`))
		empty.RecordOutboundIfAbsent("conn")
		if f := empty.Snapshot(); f == nil || f.StateLen == nil || *f.StateLen != 0 {
			t.Fatalf("帧里的空串被连接级的值覆盖了：%+v", f)
		}
	})
	t.Run("map 形态", func(t *testing.T) {
		r := KongNewWSMapFeatures(map[string]any{"client_metadata": map[string]any{openAIWSTurnStateMetadataKey: "m"}})
		kongTestFeaturesEqual(t, r.Snapshot(), kongStrPtr("m"), nil)
	})
	t.Run("快照是副本：之后的事件不改动已经落定的那一行", func(t *testing.T) {
		r := &KongFeatureRecorder{}
		r.RecordReissued("early")
		snap := r.Snapshot()
		r.RecordReissued("late")
		kongTestFeaturesEqual(t, snap, nil, kongStrPtr("early"))
		kongTestFeaturesEqual(t, r.Snapshot(), nil, kongStrPtr("late"))
	})
	t.Run("并发读写", func(t *testing.T) {
		r := &KongFeatureRecorder{}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func() { defer wg.Done(); r.RecordReissued("x") }()
			go func() { defer wg.Done(); _ = r.Snapshot() }()
		}
		wg.Wait()
	})
}

func TestKongFeaturesTravelOnOutboundRequest(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	if KongFeaturesFromRequest(req) != nil || KongFeaturesFromRequest(nil) != nil {
		t.Fatal("没挂记录器时取不到特征")
	}
	r := &KongFeatureRecorder{}
	r.RecordReissued("r")
	if kongWithFeatures(req.Context(), nil) != req.Context() {
		t.Fatal("nil 记录器不改 context")
	}
	req = req.WithContext(kongWithFeatures(req.Context(), r))
	kongTestFeaturesEqual(t, KongFeaturesFromRequest(req), nil, kongStrPtr("r"))
}
