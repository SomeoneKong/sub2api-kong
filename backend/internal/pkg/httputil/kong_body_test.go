//go:build unit

package httputil

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func kongNaiveContainsJSONControlByte(b []byte) bool {
	for _, c := range b {
		if isJSONControlByte(c) {
			return true
		}
	}
	return false
}

func TestKongContainsJSONControlByteMatchesNaive(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for n := 0; n < 20000; n++ {
		size := rng.IntN(40)
		b := make([]byte, size)
		for i := range b {
			switch rng.IntN(5) {
			case 0:
				b[i] = byte(rng.IntN(256))
			case 1:
				b[i] = []byte{0x00, 0x1f, 0x20, 0x7e, 0x7f, 0x80, 0x9f, 0xff}[rng.IntN(8)]
			default:
				b[i] = byte(0x20 + rng.IntN(0x5f))
			}
		}
		require.Equal(t, kongNaiveContainsJSONControlByte(b), kongContainsJSONControlByte(b), "%x", b)
	}
	for c := 0; c < 256; c++ {
		for pos := 0; pos < 17; pos++ {
			b := []byte("abcdefghijklmnopq")
			b[pos] = byte(c)
			require.Equal(t, kongNaiveContainsJSONControlByte(b), kongContainsJSONControlByte(b), "byte %#x at %d", c, pos)
		}
	}
}

// 预判只替代状态机：去 BOM 与长度检查照旧在前面。
func TestKongNormalizeLenientJSONKeepsBOMAndLimit(t *testing.T) {
	bom := []byte("\xef\xbb\xbf")
	compact := []byte(`{"a":"b"}`)

	got, err := NormalizeLenientJSONRequestBody(append(append([]byte(nil), bom...), compact...), 0)
	require.NoError(t, err)
	require.Equal(t, compact, got)

	got, err = NormalizeLenientJSONRequestBody(bom, 0)
	require.NoError(t, err)
	require.Empty(t, got)

	_, err = NormalizeLenientJSONRequestBody(compact, int64(len(compact)-1))
	var maxErr *http.MaxBytesError
	require.True(t, errors.As(err, &maxErr))
	require.Equal(t, int64(len(compact)-1), maxErr.Limit)

	// 去 BOM 前超限、去 BOM 后恰好不超限：照旧放行。
	got, err = NormalizeLenientJSONRequestBody(append(append([]byte(nil), bom...), compact...), int64(len(compact)))
	require.NoError(t, err)
	require.Equal(t, compact, got)

	got, err = NormalizeLenientJSONRequestBody([]byte("{\"a\":\"x\x01y\"}"), 0)
	require.NoError(t, err)
	require.Equal(t, "{\"a\":\"x"+`\`+"u0001y\"}", string(got))
}
