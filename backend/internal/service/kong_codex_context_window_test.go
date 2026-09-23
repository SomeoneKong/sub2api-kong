//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKongParseCodexContextWindowRatio(t *testing.T) {
	for _, tc := range []struct {
		value   string
		present bool
		want    float64
		wantErr bool
	}{
		{present: false, want: 1},
		{value: "0", present: true, want: 0},
		{value: " 0.7 ", present: true, want: 0.7},
		{value: "1", present: true, want: 1},
		{value: "", present: true, wantErr: true},
		{value: "abc", present: true, wantErr: true},
		{value: "NaN", present: true, wantErr: true},
		{value: "-0.1", present: true, wantErr: true},
		{value: "1.5", present: true, wantErr: true},
	} {
		got, err := kongParseCodexContextWindowRatio(tc.value, tc.present)
		if tc.wantErr {
			require.Error(t, err, "value=%q", tc.value)
			continue
		}
		require.NoError(t, err, "value=%q", tc.value)
		require.Equal(t, tc.want, got, "value=%q", tc.value)
	}
}

const kongContextWindowManifest = `{"etag_hint":"kept","models":[
	{"slug":"gpt-6-sol","context_window":272000,"max_context_window":872000,"effective_context_window_percent":95},
	{"slug":"gpt-5.5","context_window":272000,"max_context_window":272000},
	{"slug":"above-max","context_window":900000,"max_context_window":872000},
	{"slug":"no-max","context_window":128000},
	{"slug":"null-max","context_window":128000,"max_context_window":null},
	{"slug":"no-window","max_context_window":872000}
]}`

func kongContextWindows(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var envelope struct {
		Models []map[string]any `json:"models"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	out := make(map[string]any, len(envelope.Models))
	for _, model := range envelope.Models {
		slug, ok := model["slug"].(string)
		require.True(t, ok, "slug 应为字符串：%v", model)
		out[slug] = model["context_window"]
	}
	return out
}

func TestKongRaiseCodexContextWindows(t *testing.T) {
	for _, tc := range []struct {
		ratio   float64
		wantSol float64
	}{
		{ratio: 1, wantSol: 872000},
		// 872000 × 0.7 在浮点里是 610399.99…，按四舍五入而非截断。
		{ratio: 0.7, wantSol: 610400},
	} {
		body, changed, err := kongRaiseCodexContextWindows([]byte(kongContextWindowManifest), tc.ratio)
		require.NoError(t, err)
		require.True(t, changed)
		windows := kongContextWindows(t, body)
		require.Equal(t, tc.wantSol, windows["gpt-6-sol"])
		require.Equal(t, float64(272000), windows["gpt-5.5"])
		require.Equal(t, float64(900000), windows["above-max"], "只往上调")
		require.Equal(t, float64(128000), windows["no-max"])
		require.Equal(t, float64(128000), windows["null-max"])
		require.Nil(t, windows["no-window"], "缺 context_window 时不补")

		var envelope struct {
			ETagHint string           `json:"etag_hint"`
			Models   []map[string]any `json:"models"`
		}
		require.NoError(t, json.Unmarshal(body, &envelope))
		require.Equal(t, "kept", envelope.ETagHint)
		sol := envelope.Models[0]
		require.Equal(t, float64(872000), sol["max_context_window"])
		require.Equal(t, float64(95), sol["effective_context_window_percent"])
	}
}

func TestKongRaiseCodexContextWindowsLeavesUnchangedBodyIntact(t *testing.T) {
	original := []byte(`{"models":[{"slug":"gpt-5.5","context_window":272000,"max_context_window":272000}]}`)
	body, changed, err := kongRaiseCodexContextWindows(original, 1)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, original, body)

	body, changed, err = kongRaiseCodexContextWindows([]byte(`{"object":"list"}`), 1)
	require.NoError(t, err)
	require.False(t, changed)
	require.JSONEq(t, `{"object":"list"}`, string(body))
}

func TestKongFinalizeCodexModelsManifest(t *testing.T) {
	original := []byte(kongContextWindowManifest)
	originalETag := codexModelsManifestBodyETag(original)
	newManifest := func() *OpenAIModelsResponse {
		return &OpenAIModelsResponse{Body: append([]byte(nil), original...), ETag: originalETag}
	}

	// 客户端带着折算前的 ETag 来，必须拿到折算后的正文而不是 304。
	manifest := newManifest()
	kongFinalizeCodexModelsManifest(manifest, originalETag, 1)
	require.False(t, manifest.NotModified)
	require.Equal(t, float64(872000), kongContextWindows(t, manifest.Body)["gpt-6-sol"])
	require.Equal(t, codexModelsManifestBodyETag(manifest.Body), manifest.ETag)
	require.NotEqual(t, originalETag, manifest.ETag)
	finalETag := manifest.ETag

	manifest = newManifest()
	kongFinalizeCodexModelsManifest(manifest, "W/"+finalETag, 1)
	require.True(t, manifest.NotModified)
	require.Nil(t, manifest.Body)
	require.Equal(t, finalETag, manifest.ETag)

	// 关闭时正文与 ETag 原样，但 If-None-Match 仍要判定。
	manifest = newManifest()
	kongFinalizeCodexModelsManifest(manifest, "", 0)
	require.Equal(t, original, manifest.Body)
	require.Equal(t, originalETag, manifest.ETag)
	manifest = newManifest()
	kongFinalizeCodexModelsManifest(manifest, originalETag, 0)
	require.True(t, manifest.NotModified)

	// 上游已判定 304 的不再处理。
	notModified := &OpenAIModelsResponse{ETag: originalETag, NotModified: true}
	kongFinalizeCodexModelsManifest(notModified, "", 1)
	require.True(t, notModified.NotModified)
	require.Nil(t, notModified.Body)
}
