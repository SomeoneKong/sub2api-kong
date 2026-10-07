# 账号指纹测试的文本指纹库

`gpt_text_bank.json` 用于账号指纹测试的模型归因（设计见仓库根 `DESIGN-openai-fingerprint-test.md`）。
它由工作区的 `fp-lab/library.py build` 生成，原样复制进来，不手改。

当前版本：fp-lab 的 `library/gpt-text-v3.json`，`built_at` 2026-10-07T06:11:40Z。8 个 GPT 模型，
每个模型 40 份合并挑战，取自 10-07 的两个时段，推理强度 low，instructions 为网关的默认 instructions。

库里有：
- 模型列表，也就是指纹测试可选的目标；
- 合并挑战的题目全文与各题的名字；
- 建库时的推理强度与 instructions 摘要；
- 每个模型在每道题上的用词与词对计数；
- 两层判定的参数（阈值、最多份数、第一层温度、第二层的 w / m / cap）。

加载时逐项校验（`kongParseFingerprintBank`）：计数缺一个模型、题目与计数对不上号时，评分照样会算出
看起来正常的分数，而不是报错。instructions 摘要必须等于网关当前的默认 instructions：两者不同，
模型的写法就和库里的样本不可比。

**换库不必重新构建镜像**：设置环境变量 `KONG_FINGERPRINT_BANK` 指向外部文件即可覆盖内置的这份。
每一份探测记录都带着当时所用库的内容摘要与判定规则版本，换库后历史结论仍能复核，也能凭存下的回答原文重算。

## 换内置库的步骤

1. 在 fp-lab 补采数据并建库，先通过 `fp-lab/STRATEGY.md` 里的验收（交叉验证、跨批次留出、序贯模拟）。
2. 把新库文件原样复制为 `backend/internal/service/kong_data/gpt_text_bank.json`，再更新本说明里的版本。
3. 重算 golden：在工作区的 `fp-lab/` 下运行 `python export_golden.py --library <新库>`，它会重写
   `testdata/kong_fingerprint_text_golden.json`（含库文件的摘要、各份回答的分段与得分、判定序列）。
4. 在 `backend/` 下跑 `go test -tags=unit -run '^TestKong' ./internal/service/`：golden 测试先核对摘要与内置库一致，
   再逐项比对 Go 与 Python 的分段、得分与判定。
5. 换题就是换库：题目全文在库里，代码不存题目。网关默认 instructions 变了也要重建库。

外部覆盖只用来换同一方法下的新样本：题目、推理强度、阈值、最多份数与难分的那一对都是方法的一部分，管理端弹窗
与文档按它们写成了固定说明。要改其中任何一项，是改方法，要随代码一起发布并同步弹窗文案与设计文档。
