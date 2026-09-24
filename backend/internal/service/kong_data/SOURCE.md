# 指纹归因的校准资料

`unified_bank.json` 来自 [ModelTrace](https://github.com/xqy2006/ModelTrace) 的统一模型库
（`data/unified_bank.json`），用于账号指纹测试的模型归因（设计见仓库根 `DESIGN-openai-fingerprint-test.md`）。

当前版本：上游 commit `55a2e4a`，`built_at` 2026-09-23T06:17:08，16 个候选模型（gpt 8 个、claude 8 个，
含 `gpt-6-sol` / `gpt-6-luna`）。

**这份资料不是本项目产出的**，算法也照官方口径移植、不自创变体——偏差不会报错，只会表现成
测不出结论或测错模型。移植正确性由
`internal/service/kong_fingerprint_test.go` 的 golden file 回归测试锁住。

资料内容：候选模型的中心向量、两个分支各自的共享环境干扰方向、以及按有效份数分档的
softmax 校准温度。维度在加载时校验（`kongParseFingerprintBank`）——维度对不上时算法会算出一个
「看起来正常」的分数而不是报错，那是移植偏差最危险的形态。

**换资料不必重新构建镜像**：设置环境变量 `KONG_FINGERPRINT_BANK` 指向外部文件即可覆盖内置的
这份。每次归因都会把当时所用资料的 `schema` / `built_at` / 内容摘要记进探测记录——档 "1" 不是
一份永不变的资料，换了资料同一序列会算出不同概率，没有版本标识就既不能复核也不能重算。

## 换内置资料的步骤

1. 确认上游从上一版到新版之间 `fingerprint.py`、`challenge_suite.py` 没有改动（`git diff --stat
   <旧 commit> <新 commit> -- fingerprint.py challenge_suite.py`）。改了就是算法变更，要先移植算法，
   不能只换资料。
2. 用 `git show <新 commit>:data/unified_bank.json` 取原始字节覆盖本文件，并更新上面的版本说明。
3. 重算 golden：在上游仓库里对 `testdata/kong_fingerprint_golden.jsonl` 的每一行调用
   `analyze_outputs([{"text": text, "expected_count": expected_count}], bank)`，改写 `parsed`
   （`diagnostics[0].parsed_numbers`）、`best` / `best_p`（`prediction` / `probability`）与 `top`
   （前三名），概率保留 4 位小数，其余字段不动。**先用旧资料跑一遍，确认能逐条复现现有 golden**，
   再换新资料生成——否则说不清差异来自资料还是来自重算方式。
4. 跑 `go test -tags=unit -run '^TestKong' ./internal/service/`。资料里 `family` 为 `gpt` 的候选就是
   指纹测试可选的目标模型，增删候选会直接改变管理端的可选项。新候选可能从已有模型那里分走概率，
   上线前按 ModelTrace 参考数据做交叉验证（口径同上游 `bank_builder.calibration_records`）。
