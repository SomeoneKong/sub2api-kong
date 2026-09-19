# sub2api-kong

[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) 的 fork，用于自用部署的定制。
上游以 LGPL-3.0 授权，本 fork 同样以 LGPL-3.0 发布，自 2026-09 起在其基础上修改。

**改了什么看 git**：`git log --grep='^\[kong\]'` 是定制清单，diff 是细节（改动不在文件头
重复标注）。本文只写不随版本变化的东西——约定、规则、流程。

## 定制的边界

只碰两类东西：**codex ticket 相关的功能**，以及**fork 自身必须适配的部分**（版本检查、
发布标识）。上游其余部分一律不动——定制面越窄，越能持续跟上上游的 bug 修复。

有两处是**结构性的、不随上游版本变化的约束**，值得写在这里而不是只留在 commit 里：

- **版本检查必须指向本 fork。** 否则上游每发一个新版就提示一次更新，点下去会把定制覆盖成
  官方版。这是 `githubRepo` 那处改动存在的全部理由。
- **容器部署下不要用面板上的就地升级/回滚。** 它替换的是容器可写层里的二进制，重启即回到
  镜像版本，"升级成功"是假的。代码层面**故意不加门控**——上游对这几个函数有测试，禁用等于
  连带改上游测试文件，白白扩大 rebase 冲突面；而改了 `githubRepo` 之后最坏结果只是"重启后
  白升一次"，不再有覆盖定制的风险。换镜像才是正道。顺带一个反直觉点：上游的 `BuildType`
  （`source`/`release`）看着像天然开关，但它只被透传给前端展示、**没有任何门控**。

## 维护约定

| 约定 | 原因 |
|---|---|
| 定制提交以 `[kong]` 开头 | rebase 到新基线时用 `--grep='^\[kong\]'` 筛出待重放的提交；也是定制清单的来源 |
| 定制测试用 `*_kong_test.go` | 独立文件名，不与上游测试文件冲突 |
| **不改 Go module path** | 仍是 `github.com/Wei-Shaw/sub2api`。改它要动几百处 import、制造巨大 rebase 冲突面；本 fork 只构建镜像、不作为库被引用 |
| **不改 `backend/cmd/server/VERSION`** | 版本优先级是 `--build-arg VERSION` > git tag > 该文件，走 build-arg 即可 |
| CI / Security Scan 只在 `main` 与 PR 上触发 | 上游的 `on: push` 无分支过滤。本 fork 会持续 push `base/*` tag 归档上游基线，不限制就每个 tag 都跑一遍全量 CI——跑的还是纯上游代码 |
| `upstream` remote 禁止 push | `git remote set-url --push upstream DISABLED` |
| `base/*` tag 必须 push 到 origin | push 后该 commit object 即归本仓库。上游会 force push、删 tag、撤 release，不这么做就得靠运气 |

## 版本号与发布

发布 tag 形如 `v<上游基线版本>-kong.<n>`。序号在同一上游基线内递增，跟进新基线时归 1
（`v0.2.5-kong.7` → `v0.2.6-kong.1`）。

`-kong.<n>` 后缀参与版本比较（见 `update_service.go` 的 `parseVersion`），所以序号必须是
数字；精确的上游基线 SHA 记在 tag message、release body 与镜像 OCI label 里。

镜像发布到 GHCR：`ghcr.io/someonekong/sub2api-kong`。推 `v*` tag 即触发
`.github/workflows/release.yml` 构建发布，镜像名由 `github.repository` 推导，无需配置。

发布行为由**仓库变量 `SIMPLE_RELEASE=true`** 控制：只出 x86_64 的 GHCR 镜像，跳过多架构
qemu 与其它产物。部署目标只有 amd64，这样省掉大半构建时间。用仓库变量而不是改 workflow，
是为了不增加 `release.yml` 的 rebase 冲突面——上游本就留了 `vars.SIMPLE_RELEASE` 这个入口。
未配 Docker Hub / Telegram secrets 时，相应步骤自带守卫会跳过。

版本号无需在开发时维护：打 tag 即决定。`update-version` job 会把 tag 版本写进工作区的
`VERSION` 文件（`go:embed` 进二进制），手工构建则走 `--build-arg VERSION`（ldflags 优先级
最高）。**裸 `go build` 不传参时自报仓库里 `VERSION` 的值，即上游版本号，属预期**——该文件
刻意不跟随本 fork 的版本。

## 与上游同步

当前基线：`git tag -l 'base/*' --sort=-creatordate | head -1`（按时间排，不能按字典序——
`base/v0.2.10` 会排在 `base/v0.2.7` 前面）。跟进新基线：

```bash
git fetch upstream --tags
git tag -a base/<版本>-<sha8> <sha> -m "上游基线：<sha>"
git push origin base/<版本>-<sha8>
git rebase <新 base>          # 重放 [kong] 提交
```

冲突只会出现在定制触及的文件上。要知道是哪些：

```bash
git diff --name-only $(git tag -l 'base/*' --sort=-creatordate | head -1)..HEAD
```
