interface LatencyRow {
  output_tokens: number
  duration_ms: number | null
  first_token_ms: number | null
}

/**
 * 估算生成速度（token/s）：输出 token ÷（总耗时 − 首字时间）。
 *
 * 首字默认按语义口径计（上游第一个非元数据事件，推理一开始就算），所以这段时长覆盖推理与正文，
 * 与同样包含推理 token 的 output_tokens 对得上。设置里改成「可见输出」口径时，首字之前已生成的
 * 推理 token 也被算进分子，估计值会偏高。没有首字时间（非流式）时排队与预填充剔不出去，不给估计。
 */
export function estimateOutputTokensPerSecond(row: LatencyRow): number | null {
  const { output_tokens: tokens, duration_ms: duration, first_token_ms: firstToken } = row
  if (!(tokens > 0) || duration == null || firstToken == null) return null
  const generationMs = duration - firstToken
  if (generationMs <= 0) return null
  return tokens / (generationMs / 1000)
}

export function formatOutputSpeed(row: LatencyRow): string | null {
  const speed = estimateOutputTokensPerSecond(row)
  if (speed == null) return null
  return `${speed >= 100 ? Math.round(speed) : speed.toFixed(1)} tok/s`
}
