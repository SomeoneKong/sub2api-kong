-- 账号指纹测试改用文本指纹（fork 专有）：探测记录加回答原文与各题得分。
-- 设计见仓库根的 DESIGN-openai-fingerprint-test.md。只加可空列，旧版本的代码不读不写它们。

ALTER TABLE kong_fingerprint_probes
    -- 这一份的完整回答正文，是文本指纹归因的完整输入：换库或换判定规则后凭它重算
    ADD COLUMN IF NOT EXISTS answer_text TEXT,
    -- 题名 → 模型 → 得分，说明是哪几道题把结论推向哪边
    ADD COLUMN IF NOT EXISTS section_scores JSONB;
