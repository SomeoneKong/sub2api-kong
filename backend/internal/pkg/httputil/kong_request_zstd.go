package httputil

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// 入站 zstd 请求体的快路径（设计见 DESIGN-openai-request-body-memory.md §2.3）：读块不拼接、复用同步解码器、按帧头
// 预分配。结果与原来的读法一致：快路径一旦出错，或输出达到 64 MiB 的截断线，就把各块拼起来交给原来的
// decompressRequestBody 重解，错误与截断都由原代码决定——同步与异步解码只在这两种情况下可能交出不同的结果。

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

// kongReadZstdRequestBody 接管 Content-Encoding 为 zstd 的请求体，handled 为假时由 ReadRequestBodyWithPrealloc
// 照原样处理。capHint 是原函数按 Content-Length 算出的初始容量。
func kongReadZstdRequestBody(req *http.Request, capHint int) (body []byte, handled bool, err error) {
	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	if enc != "zstd" || !KongRequestZstdFastpathEnabled() {
		return nil, false, nil
	}
	chunks, total, err := kongReadRequestBodyChunkList(req.Body, capHint, req.ContentLength)
	if err != nil {
		return nil, true, err
	}
	decoded, ok := kongDecodeZstdChunks(chunks, total)
	if !ok {
		var decodeErr error
		decoded, decodeErr = decompressRequestBody(enc, kongJoinChunks(chunks, total))
		if decodeErr != nil {
			return nil, true, fmt.Errorf("decode Content-Encoding %q: %w", enc, decodeErr)
		}
	}
	// 与 ReadRequestBodyWithPrealloc 解压成功之后的处理相同。
	req.Header.Del("Content-Encoding")
	req.Header.Del("Content-Length")
	req.ContentLength = int64(len(decoded))
	return decoded, true, nil
}

// kongReadRequestBodyChunkList 与 readRequestBodyChunks 的读法逐行相同（同样的块容量、同样原样返回读取错误），
// 只是不把各块拼成一份。
func kongReadRequestBodyChunkList(reader io.Reader, initialCapacity int, contentLength int64) ([][]byte, int, error) {
	capacity := initialCapacity
	var chunks [][]byte
	total := 0
	for {
		chunkCapacity := capacity
		if remaining := contentLength - int64(total); remaining >= 0 && remaining < int64(chunkCapacity) {
			chunkCapacity = int(remaining) + 1
		}
		chunk := make([]byte, chunkCapacity)
		n := 0
		var err error
		for n < len(chunk) && err == nil {
			var read int
			read, err = reader.Read(chunk[n:])
			n += read
		}
		if err != nil && err != io.EOF {
			return nil, 0, err
		}
		if n > 0 {
			chunks = append(chunks, chunk[:n])
			total += n
		}
		if err != nil {
			return chunks, total, nil
		}
		if capacity < requestBodyReadMaxInitCap {
			capacity *= 2
			if capacity > requestBodyReadMaxInitCap {
				capacity = requestBodyReadMaxInitCap
			}
		}
	}
}

// kongJoinChunks 拼出 readRequestBodyChunks 会返回的那份字节。
func kongJoinChunks(chunks [][]byte, total int) []byte {
	switch len(chunks) {
	case 0:
		return []byte{}
	case 1:
		return chunks[0]
	}
	body := make([]byte, total)
	offset := 0
	for _, part := range chunks {
		offset += copy(body[offset:], part)
	}
	return body
}

// kongDecodeZstdChunks 用池里的同步解码器解出各块。ok 为假（出错、窗口超限、输出达到截断线、读不出帧头、空体）时
// 由调用方交给原来的 decompressRequestBody。
func kongDecodeZstdChunks(chunks [][]byte, total int) ([]byte, bool) {
	if total == 0 {
		return nil, false
	}
	var header zstd.Header
	if err := header.Decode(chunks[0]); err != nil {
		return nil, false
	}
	capacity := requestBodyReadInitCap
	if header.HasFCS {
		size := min(header.FrameContentSize, uint64(total)*kongZstdPreallocRatio, uint64(maxDecompressedBodySize))
		if int(size)+1 > capacity {
			capacity = int(size) + 1
		}
	}

	decoder, ok := kongZstdDecoderPool.Get().(*zstd.Decoder)
	if !ok {
		var err error
		if decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(kongZstdDecoderMaxWindow)); err != nil {
			return nil, false
		}
	}
	readers := make([]io.Reader, len(chunks))
	for i, chunk := range chunks {
		readers[i] = bytes.NewReader(chunk)
	}
	if err := decoder.Reset(io.MultiReader(readers...)); err != nil {
		decoder.Close()
		return nil, false
	}

	// 读取循环与 io.ReadAll 相同，只是起始容量不同；外面同样套 64 MiB 的 LimitReader。
	out := make([]byte, 0, capacity)
	limited := io.LimitReader(decoder, maxDecompressedBodySize)
	for {
		n, err := limited.Read(out[len(out):cap(out)])
		out = out[:len(out)+n]
		if err != nil {
			if err != io.EOF {
				decoder.Close()
				return nil, false
			}
			break
		}
		if len(out) == cap(out) {
			out = append(out, 0)[:len(out)]
		}
	}

	_ = decoder.Reset(nil)
	kongZstdDecoderPool.Put(decoder)
	if len(out) >= maxDecompressedBodySize {
		return nil, false
	}
	return out, true
}
