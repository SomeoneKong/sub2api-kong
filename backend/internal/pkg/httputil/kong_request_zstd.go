package httputil

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// 入站 zstd 请求体的解码（设计见 DESIGN-openai-request-body-memory.md §2.3）：复用同步解码器、按帧头预分配输出。读块、
// 内存预算与读取错误的分类都照原代码。快路径一旦出错，或输出超过 64 MiB，就交给原来的 decompressRequestBodyWithBudget
// 重解，错误由原代码决定；只有解码中途请求被取消时直接交出读输出时的错误（与原代码一样带读取进度），重解只会在预留
// 预算时报出不带进度的取消。

// KongRequestZstdFastpathEnabled 报告快路径是否开启；service 包初始化时把它换成读请求体快速路径开关里的 zstd_in。
var KongRequestZstdFastpathEnabled = func() bool { return true }

const (
	// kongZstdPreallocRatio：帧头里的原始大小没有经过验证，只当提示用，预分配不超过压缩体长度的这么多倍。
	kongZstdPreallocRatio = 32
	// kongZstdDecoderMaxWindow 是快路径接受的帧窗口上限（单段帧的窗口就是原始大小）。解码器的历史缓冲按窗口分配、
	// Reset 不会缩小，限住窗口，池里的解码器就不会攥着大块；超限的输入在帧头就报错，交回原代码。
	kongZstdDecoderMaxWindow = 8 << 20
)

var kongZstdDecoderPool sync.Pool

// kongDecompressRequestBody 替换 readRequestBodyWithBudget 里对 decompressRequestBodyWithBudget 的调用，参数与返回值相同。
func kongDecompressRequestBody(ctx context.Context, encoding string, raw []byte) (requestBodyBuffer, error) {
	if encoding == "zstd" && KongRequestZstdFastpathEnabled() {
		if decoded, handled, err := kongDecodeZstd(ctx, raw); handled {
			return decoded, err
		}
	}
	return decompressRequestBodyWithBudget(ctx, encoding, raw)
}

// kongDecodeZstd 用池里的同步解码器解出 raw。handled 为真时返回的就是结果：解码成功，或解码中途请求被取消（err 为读
// 输出时的错误）。handled 为假（出错、窗口超限、输出超过截断线、读不出帧头、空体、预算不足）时已退还自己的预留，由
// 调用方交给原来的 decompressRequestBodyWithBudget。
func kongDecodeZstd(ctx context.Context, raw []byte) (decoded requestBodyBuffer, handled bool, err error) {
	if len(raw) == 0 {
		return requestBodyBuffer{}, false, nil
	}
	var header zstd.Header
	if err := header.Decode(raw); err != nil {
		return requestBodyBuffer{}, false, nil
	}
	capacity := requestBodyReadInitCap
	if header.HasFCS {
		size := min(header.FrameContentSize, uint64(len(raw))*kongZstdPreallocRatio, uint64(maxDecompressedBodySize))
		capacity = max(capacity, int(size)+1) // 多 1 字节，读到 EOF 时不必再分一块
	}

	// 解码器的历史缓冲与原代码一样按窗口上限记账。
	if err := reserveRequestBodyMemory(ctx, maxRequestZstdWindowSize); err != nil {
		return requestBodyBuffer{}, false, nil
	}
	defer releaseRequestBodyMemory(ctx, maxRequestZstdWindowSize)

	decoder, ok := kongZstdDecoderPool.Get().(*zstd.Decoder)
	if !ok {
		var err error
		if decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(kongZstdDecoderMaxWindow)); err != nil {
			return requestBodyBuffer{}, false, nil
		}
	}
	if err := decoder.Reset(bytes.NewReader(raw)); err != nil {
		decoder.Close()
		return requestBodyBuffer{}, false, nil
	}
	// 输出照原代码按块读、逐块记账，只是第一块按帧头预分配：原始大小属实时整份落在这一块里，不再拼接。
	decoded, err = readRequestBodyChunksWithBudget(ctx, io.LimitReader(decoder, maxDecompressedBodySize+1), capacity, -1)
	if err != nil {
		decoder.Close()
		if ctx.Err() != nil {
			return requestBodyBuffer{}, true, err
		}
		return requestBodyBuffer{}, false, nil
	}
	_ = decoder.Reset(nil)
	kongZstdDecoderPool.Put(decoder)
	if len(decoded.data) > maxDecompressedBodySize {
		decoded.release(ctx)
		return requestBodyBuffer{}, false, nil
	}
	return decoded, true, nil
}
