# 指纹归因的校准资料

`unified_bank.json` 来自 [ModelTrace](https://xqy2006.github.io/ModelTrace/) 的统一模型库，
用于 Codex 票据的档位归因（设计见仓库根 `DESIGN-codex-ticket.md` §4.1）。

**这份资料不是本项目产出的**，算法也照官方口径移植、不自创变体——判据只需要 astra / 非 astra
的二分，但偏差不会报错，只会表现成「一直拿不到合格票」。移植正确性由
`internal/service/kong_fingerprint_test.go` 的 golden file 回归测试锁住。

资料内容：13 个候选模型的中心向量、两个分支各自的共享环境干扰方向、以及按有效份数分档的
softmax 校准温度。维度在加载时校验（`kongParseFingerprintBank`）——维度对不上时算法会算出一个
「看起来正常」的分数而不是报错，那是移植偏差最危险的形态。

**换资料不必重新构建镜像**：设置环境变量 `KONG_FINGERPRINT_BANK` 指向外部文件即可覆盖内置的
这份。每次归因都会把当时所用资料的 `schema` / `built_at` / 内容摘要记进探测记录——档 "1" 不是
一份永不变的资料，换了资料同一序列会算出不同概率，没有版本标识就既不能复核也不能重算。
