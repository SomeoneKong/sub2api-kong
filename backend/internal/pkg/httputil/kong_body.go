package httputil

import "encoding/binary"

// kongContainsJSONControlByte 报告 b 里有没有 NormalizeLenientJSONRequestBody 会改写的控制字节
// （0x00–0x1F 与 0x7F）。一个都没有时，状态机必然原样返回，可以跳过。
//
// 每次看 8 个字节：hasLess 判断有没有小于 0x20 的字节，hasZero 配合异或判断有没有 0x7F。这两个写法作为
// "有没有"的布尔判断都是精确的；不小于 0x80 的字节被 ^x 的最高位屏蔽，不会误报。
func kongContainsJSONControlByte(b []byte) bool {
	const (
		ones  = 0x0101010101010101
		highs = 0x8080808080808080
	)
	i := 0
	for ; i+8 <= len(b); i += 8 {
		x := binary.LittleEndian.Uint64(b[i:])
		hasLess := (x - ones*0x20) & ^x & highs
		del := x ^ (ones * 0x7f)
		hasDel := (del - ones) & ^del & highs
		if hasLess|hasDel != 0 {
			return true
		}
	}
	for ; i < len(b); i++ {
		if isJSONControlByte(b[i]) {
			return true
		}
	}
	return false
}
