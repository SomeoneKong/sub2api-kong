//go:build unit && race

package handler

// kongRaceEnabled 为真时按分配计量的用例跳过：race 下 sync.Pool 会随机丢弃放回的对象，池化复用的计量不代表实际。
const kongRaceEnabled = true
