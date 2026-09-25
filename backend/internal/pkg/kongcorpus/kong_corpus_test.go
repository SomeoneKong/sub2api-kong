//go:build unit

package kongcorpus

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRequestShapes(t *testing.T) {
	for _, shape := range []Shape{ShapeSol, ShapeLite} {
		o := Options{Shape: shape, Seed: 7, TargetBytes: 1100 << 10}
		body := Request(o)
		require.True(t, json.Valid(body))
		require.GreaterOrEqual(t, len(body), o.TargetBytes)
		require.True(t, bytes.Equal(body, Request(o)), "同一组 Options 必须生成同样的字节")

		var keys []string
		gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
			keys = append(keys, key.String())
			return true
		})
		if shape == ShapeSol {
			require.Equal(t, []string{"model", "instructions", "input", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "store", "stream", "include", "prompt_cache_key", "text", "client_metadata"}, keys)
		} else {
			require.Equal(t, []string{"model", "input", "tool_choice", "parallel_tool_calls", "reasoning", "store", "stream", "include", "prompt_cache_key", "text", "client_metadata"}, keys)
			require.Equal(t, "additional_tools", gjson.GetBytes(body, "input.0.type").String())
			require.Equal(t, "all_turns", gjson.GetBytes(body, "reasoning.context").String())
		}
	}
}

func TestQuoteMatchesJSON(t *testing.T) {
	for _, s := range []string{"plain", "quote\" back\\slash", "ctrl\x01\x1b\n\t\r\b\f", "中文 “引号” naïve", "</script>&"} {
		var decoded string
		require.NoError(t, json.Unmarshal([]byte(Quote(s)), &decoded))
		require.Equal(t, s, decoded)
	}
	require.Equal(t, `"</script>&"`, Quote("</script>&"), "serde_json 不转义 HTML 字符")
}

func TestSSEResponseEndsWithCompleted(t *testing.T) {
	sse := SSEResponse(1, "gpt-6-sol", 20)
	require.Contains(t, string(sse), "event: response.completed\n")
	require.True(t, bytes.HasSuffix(sse, []byte("\n\n")))
}
