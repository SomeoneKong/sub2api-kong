#!/usr/bin/env python3
"""给 kong-race 的 go test -race 输出分诊：只有与本 fork 相关的问题才算失败。

上游测试套在 -race 下一直不干净（测试桩不加锁、并行用例各自 gin.SetMode、改包级变量时上一个用例的后台
协程还在跑），每次都会报十几个竞态、几十个连带失败的用例，并且每次不一样。本脚本把它们归为上游噪声，只在
以下情况返回 1：

1. 有包没跑完或日志不完整：编译失败、测试 panic、fatal error（如并发写 map）、超时、被杀、测试程序没打出
   结束的 FAIL 就退出（如中途 os.Exit）、竞态检测器自身退出、竞态报告没有收尾、预期的包没有结果——这时本
   fork 的用例可能根本没跑到；
2. 本 fork 的用例（定义在 kong_*_test.go 里）失败，且原因不只是"执行期间检测到竞态"；
3. 竞态报告的调用栈（两次访问与各自协程的创建处）里有本 fork 的代码：kong_*.go 的帧，或落在本 fork 相对上游
   基线新增的行上的帧（上游名字的文件里的接入点）。唯一例外是两次访问都在 gin 的全局模式读写上（并行用例各自
   gin.SetMode），那是测试写法造成的。

用法：race_triage.py <go test 输出> <backend 目录> [上游基线 ref] [预期包清单文件]。不给基线时第 3 条只看
kong_*.go；预期包清单每行一个导入路径，不给时只要求至少有一个包的结果。结论写到标准输出；设置了
GITHUB_STEP_SUMMARY 时另写一份 Markdown 摘要。
"""
import collections
import os
import re
import subprocess
import sys

ACCESS = re.compile(r"^(Read|Write|Previous read|Previous write|Atomic \w+|Previous atomic \w+) at ")
CREATED = re.compile(r"^Goroutine \d+ \(\w+\) created at:")
# 竞态报告里的帧位置行带 +0x 偏移；日志里的调用栈（zap console 等）没有，不会被当成竞态帧
RACE_FRAME = re.compile(r"^\s+(\S+\.go):(\d+) \+0x[0-9a-f]+$")
KONG_FILE = re.compile(r"(^|/)kong_[^/]*\.go$")
GIN_MODE = re.compile(r"/github\.com/gin-gonic/gin@[^/]+/(mode|debug)\.go$")
PKG_RESULT = re.compile(r"^(ok|FAIL|\?)\s+(\S+)(\s|$)")
TEST_FAIL = re.compile(r"^\s*--- FAIL: (\S+)")
CRASH = re.compile(r"^(panic: |fatal error: |race: |signal: )|\[(build|setup) failed\]")
HUNK = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")
RACE_ONLY = "race detected during execution of test"


def kong_tests(backend):
    """backend 目录下 kong_*_test.go 里定义的顶层测试名。"""
    names = set()
    for root, dirs, files in os.walk(backend):
        dirs[:] = [d for d in dirs if d not in ("node_modules", ".git")]
        for f in files:
            if KONG_FILE.search(f) and f.endswith("_test.go"):
                with open(os.path.join(root, f), encoding="utf-8", errors="replace") as fh:
                    names.update(re.findall(r"^func (Test\w+)\(", fh.read(), re.M))
    return names


def fork_lines(backend, base):
    """本 fork 相对上游基线在 .go 文件里新增或改过的行 {(仓库内路径, 行号)}，行号按当前版本。"""
    out = subprocess.run(["git", "diff", "-U0", "--no-color", base, "HEAD", "--", "*.go"], cwd=backend,
                         capture_output=True, text=True, encoding="utf-8", errors="replace", check=True).stdout
    lines, cur = set(), None
    for line in out.splitlines():
        if line.startswith("+++ "):
            cur = line[6:] if line.startswith("+++ b/") else None
            continue
        m = HUNK.match(line)
        if m and cur:
            start, n = int(m.group(1)), int(m.group(2) or 1)
            lines.update((cur, k) for k in range(start, start + n))
    return lines


def repo_path(path):
    """runner 上的绝对路径 -> 仓库内路径（backend/...）；不在本仓库里的为 None。"""
    return "backend/" + path.split("/backend/", 1)[1] if "/backend/" in path else None


def short(path):
    for key in ("/backend/", "/pkg/mod/", "/src/"):
        if key in path:
            return path.split(key, 1)[1]
    return path


def parse_race(block):
    """一个竞态报告 -> (两次访问与协程创建处的全部帧 [(文件, 行)], 两次访问各自最内层的帧)。"""
    frames, sites, in_stack, want_site = [], [], False, False
    for line in block:
        if ACCESS.match(line) or CREATED.match(line):
            in_stack, want_site = True, bool(ACCESS.match(line))
            continue
        m = RACE_FRAME.match(line)
        if m and in_stack:
            frames.append((m.group(1), int(m.group(2))))
            if want_site:
                sites.append((m.group(1), int(m.group(2))))
                want_site = False
    return frames, sites


def fail_reason(lines, i):
    """失败行之后缩进的说明行（t.Errorf、testify 的输出、竞态提示），到下一个失败行或不缩进的行为止。"""
    detail = []
    for line in lines[i + 1:]:
        if TEST_FAIL.match(line) or not line[:1].isspace():
            break
        if line.strip():
            detail.append(line.strip())
    return detail


def triage(lines, kong_names, expected=None):
    """返回 (竞态 [(帧, 访问点)], 失败用例 [(名字, 是否本 fork, 是否只因竞态)], 没跑完的说明)。
    没有说明的失败行，后面跟着它的子用例失败时是连带失败，交给子用例判断；否则（t.Fail 之类）不算"只因竞态"。"""
    races, raw_fails, incomplete, results = [], [], [], set()
    block = None
    seg = {"fails": 0, "races": 0, "crash": False, "ended": False}
    for i, line in enumerate(lines):
        if line.startswith("WARNING: DATA RACE"):
            block = []
            continue
        if block is not None:
            if line.startswith("=================="):
                races.append(parse_race(block))
                seg["races"] += 1
                seg["ended"] = False
                block = None
            else:
                block.append(line)
            continue
        m = TEST_FAIL.match(line)
        if m:
            raw_fails.append((m.group(1), fail_reason(lines, i)))
            seg["fails"] += 1
            seg["ended"] = False
            continue
        if line == "FAIL":
            seg["ended"] = True   # 测试程序正常结束时打的最后一行
            continue
        if CRASH.search(line):
            incomplete.append(line.strip()[:200])
            seg["crash"] = True
        m = PKG_RESULT.match(line)
        if m:
            results.add(m.group(2))
            if m.group(1) == "FAIL" and not seg["crash"]:
                if not (seg["fails"] or seg["races"]):
                    incomplete.append(f"包 {m.group(2)} 失败，但没有失败用例")
                elif not seg["ended"]:
                    incomplete.append(f"包 {m.group(2)} 的测试程序没有正常结束（中途退出）")
            seg = {"fails": 0, "races": 0, "crash": False, "ended": False}
    if block is not None:
        incomplete.append("日志末尾的竞态报告没有收尾（输出被截断）")
    if expected is not None:
        incomplete += [f"包 {p} 没有结果" for p in sorted(set(expected) - results)]
    elif not results:
        incomplete.append("没有任何包的结果（go test 没跑起来）")

    names = [n for n, _ in raw_fails]
    fails = []
    for name, detail in raw_fails:
        if not detail and any(other.startswith(name + "/") for other in names):
            continue
        top = name.split("/", 1)[0]
        fails.append((name, top in kong_names, bool(detail) and all(RACE_ONLY in d for d in detail)))
    return races, fails, incomplete


def fork_frame(frames, changed):
    """帧里第一个属于本 fork 的：kong_*.go，或落在本 fork 新增行上。没有为 None。"""
    for f, n in frames:
        if KONG_FILE.search(f) or (repo_path(f), n) in changed:
            return f"{short(f)}:{n}"
    return None


def main():
    log_path, backend = sys.argv[1], sys.argv[2]
    base = sys.argv[3] if len(sys.argv) > 3 else ""
    expected = None
    if len(sys.argv) > 4:
        with open(sys.argv[4], encoding="utf-8") as fh:
            expected = [l.strip() for l in fh if l.strip()]
    with open(log_path, encoding="utf-8", errors="replace") as fh:
        lines = fh.read().splitlines()
    kong_names = kong_tests(backend)
    changed = fork_lines(backend, base) if base else set()
    races, fails, incomplete = triage(lines, kong_names, expected)

    fork_races, gin_noise, upstream = [], 0, collections.Counter()
    for frames, sites in races:
        site_key = " / ".join(sorted({f"{short(f)}:{n}" for f, n in sites}))
        hit = fork_frame(frames, changed)
        if sites and all(GIN_MODE.search(f) for f, _ in sites):
            gin_noise += 1
        elif hit:
            fork_races.append(f"{site_key}（本 fork 的帧 {hit}）")
        else:
            upstream[site_key] += 1
    kong_fails = [name for name, is_kong, race_only in fails if is_kong and not race_only]
    upstream_fails = sorted({name for name, is_kong, _ in fails if not is_kong})
    tests_failed = {name.split("/", 1)[0] for name, _, _ in fails}

    bad = bool(incomplete or kong_fails or fork_races)
    out = [f"## Kong race 分诊：{'有本 fork 相关的问题' if bad else '只有上游噪声'}", ""]
    out.append(f"- 本 fork 的用例 {len(kong_names)} 个；失败用例 {len(tests_failed)} 个，竞态报告 {len(races)} 个")
    out.append(f"- 上游基线 `{base}`，本 fork 新增的行 {len(changed)} 行" if base else
               "- 没给上游基线：只按 kong_*.go 认本 fork 的代码")
    if expected is not None:
        out.append(f"- 预期的包 {len(expected)} 个")
    if incomplete:
        out += ["", "### 没跑完的包或不完整的日志（本 fork 的用例可能没跑到）", ""] + [f"- `{c}`" for c in incomplete]
    if kong_fails:
        out += ["", "### 本 fork 的用例失败（不只是检测到竞态）", ""] + [f"- `{n}`" for n in kong_fails]
    if fork_races:
        out += ["", "### 调用栈里有本 fork 代码的竞态", ""] + [f"- {s}" for s in fork_races]
    out += ["", "### 上游噪声（不影响结论）", "",
            f"- 竞态 {sum(upstream.values())} 个，另有并行用例 gin.SetMode {gin_noise} 个；失败用例 {len(upstream_fails)} 个"]
    for site, n in upstream.most_common():
        out.append(f"  - {n} × {site}")
    if upstream_fails:
        out.append("- 失败用例：" + "、".join(f"`{n}`" for n in upstream_fails))
    text = "\n".join(out) + "\n"
    print(text)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as fh:
            fh.write(text)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
