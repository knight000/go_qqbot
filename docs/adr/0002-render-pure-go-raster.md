# 渲染走纯 Go 光栅，内嵌静态字体子集

插件需要把 markdown 出成图片：旧版 QQ 客户端完全不渲染 markdown，而部分内容又需要统一的观感。实现是 `goldmark` 解析 GFM → 自研布局器 → `x/image/font/opentype` 光栅 → PNG；字体用 Noto Sans / Noto Sans SC / Noto Sans Mono 的**静态实例子集**经 `go:embed` 进二进制。HTML 路线被排除：整页渲染只能靠无头浏览器，而 `deploy/qqbot.service:19` 的 `MemoryMax=256M` 装不下一个 Chromium 实例。

## Considered Options

- **无头浏览器（chromedp / go-rod + Chromium）**：唯一能真正"照抄 GitHub"的路径，但要外挂一个进程。256 MB 的 cgroup 里它单实例常驻就在 200 MB 量级，必然 OOM；同时破坏「单二进制、`GOOS=linux` 一条命令交叉编译」的部署形态——当初选 `modernc.org/sqlite` 正是为了躲开这类外挂。
- **CGO 渲染库（wkhtmltoimage / libvips）**：需要 cgo，交叉编译直接报废，理由同上。
- **可变字体（variable font）**：`opentype.FaceOptions` 只有 `Size` / `DPI` / `Hinting` 三个字段，**没有 variation axis**，`wght=700` 设不进去，标题只能拿到默认字重。因此每个字重必须是独立的静态实例。
- **自写 GFM 解析器**：能省一个依赖，但 CommonMark 的 emphasis 规则（`***a**b*` 之类）会长期产生"看起来对、个别输入错"的 bug，而目标恰恰是语义准确。`goldmark` 的 `go.mod` 只有 `module` 与 `go` 两行、零 require，属于叶子依赖，代价可接受——同日把 `ARCHITECTURE.md` §3.7 的「依赖不超过 5 个」改成按**传递链宽度**判断的软约束，就是为了让这次选型有据可依，而不是悄悄破例。

## Consequences

- 二进制从 11–16 MB 涨到 **19 MB 量级**（实测 19.03–19.22 MB，其中内嵌字体 6.2 MB）。
  当时按 22–24 MB 估，实际更小：`-s -w` 加 GB2312 子集化压得比预估狠。
- 内存必须自己管：1080×4000 的 RGBA 光栅是 17.3 MB/张，所以渲染并发被限死在 2；超限内容直接返回错误，不做自动分页。不设限的话 16 个 worker 峰值 277 MB，会被内核杀。实测（仅渲染器的独立进程、1080×3928）：单次 123–154 ms；16 并发峰值 113–115 MB，与 2 并发（108–112 MB）同量级，吞吐在 15 rps 处封顶——信号量确实生效，内存不随并发线性增长。
- 观感是「GitHub 的视觉语言」而不是像素级一致：字体栈被固定成 Noto Sans——它正是 GitHub 在没有 Segoe/PingFang 的系统上的实际落点，但断行规则、margin collapsing、表格列宽算法都得自己实现，浏览器行为只能近似。
- 含中文的代码块永远无法严格对齐：`Noto Sans Mono` 的半角宽度与 `Noto Sans SC` 的全角宽度不同族，`1 汉字 ≠ 2 半角`。这是选型时明知的妥协，换 Sarasa Mono SC 能解决，但字体体积翻倍。
