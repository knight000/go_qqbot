# 内嵌字体资源

`internal/render/fonts/` 下的六份 `.ttf` 由 `gen_fonts.py` 生成，经 `go:embed`
打进二进制。**脚本不是运行期依赖**：Go 代码只读已入库的文件，这里只在需要
更新字体版本或调整子集范围时手工跑一次。

## 产物

| 文件 | 体积 | 字重 | 来源 |
| --- | --- | --- | --- |
| `NotoSans-400.ttf` | 543 KiB | Regular | Google Fonts · Noto Sans 静态实例 |
| `NotoSans-700.ttf` | 545 KiB | Bold | 同上 |
| `NotoSansSC-400.ttf` | 2.16 MiB | Regular | 本机 `NotoSansSC-VF.ttf` 钉在 `wght=400` 后按 GB2312 子集 |
| `NotoSansSC-700.ttf` | 2.16 MiB | Bold | 同上，`wght=700` |
| `NotoSansMono-400.ttf` | 380 KiB | Regular | Google Fonts · Noto Sans Mono 静态实例 |
| `NotoSansMono-700.ttf` | 379 KiB | Bold | 同上 |
| `OFL.txt` | 4.3 KiB | — | SIL Open Font License 1.1 全文（OFL 要求随字体分发） |

合计 **6,415,488 字节 = 6.12 MiB**（十进制 6.42 MB），落在 6–8 MiB 的目标内。
体积口径统一用 **KiB/MiB = 1024**：

```bash
# 字体负载（六个 ttf，不含 OFL.txt）
du -cb internal/render/fonts/*.ttf | tail -1     # 6415488
```

### 二进制体积怎么量

**口径必须带 `-trimpath -ldflags "-s -w"`**，否则量到的是带调试符号的默认构建，
两者差约 5.7 MiB，混着比会得出"字体让二进制涨了 6.4 MB 还是 7 MB"这种对不上的数。

```bash
# “字体落地之前”的基线：0520d98（字体提交）的父提交，此时 internal/render 还不存在
git worktree add --detach /tmp/rv-base 3710a56
cd /tmp/rv-base && go build -trimpath -ldflags "-s -w" -o /tmp/bot-base.exe ./cmd/bot

# 当前分支
go build -trimpath -ldflags "-s -w" -o /tmp/bot.exe ./cmd/bot
ls -l /tmp/bot-base.exe /tmp/bot.exe
```

windows/amd64 实测（2026-10-18）：

| 构建口径 | `3710a56`（字体落地前） | 当前分支 | 增量 |
| --- | --- | --- | --- |
| `-trimpath -ldflags "-s -w"` | 12,346,368 B（11.77 MiB） | 19,216,896 B（18.33 MiB） | +6.55 MiB |
| `go build`（默认，未剥离） | 17,905,664 B（17.08 MiB） | 24,931,840 B（23.78 MiB） | +6.70 MiB |

增量比字体负载多出的约 0.4 MiB 是 `x/image/font/opentype` 的解析器代码与
`internal/render` 本身。旧版文档里的 17.08 → 23.52 MB 是**默认构建**量出来的
（未剥离，且当时 `internal/render` 还只有骨架），同一口径现在读数是 23.78 MiB，
所以正文改用上表的可复现数字。

## 三条前提

1. **必须是静态实例。** `x/image/font/opentype` 的 `FaceOptions` 只有
   `Size`/`DPI`/`Hinting`，没有 variation axis；可变字体只能取默认实例，而本机
   `NotoSansSC-VF.ttf` 的 `wght` 默认值是 **100（Thin）**，直接解析会渲染成发丝字。
   脚本用 `fontTools.varLib.instancer` 离线钉字重，并在 `verify()` 里断言产物
   没有 `fvar` 表、`OS/2.usWeightClass` 与文件名一致。
2. **只有 CJK 需要子集化。** 未裁剪的 SC 单份 10.6 MB；拉丁与等宽族的静态实例
   本来就只有 0.5/0.4 MB，裁掉希腊、西里尔、越南语与框线字符省不下多少，却会让
   "多一个字母变方框" 的概率无谓升高——所以这两族**原样入库**。
3. **取字体源的 UA 必须是 `Mozilla/4.0`。** `fonts.googleapis.com/css2` 对现代
   UA 返回 woff2，`opentype.Parse` 不认。脚本用这个 UA 认领 `format('truetype')`
   的那条 `src`。

## GB2312 字符集是怎么来的

不抄字表，直接枚举 `gb2312` 编码的合法双字节组合：

```python
for hi in range(0xA1, 0xFF):
    for lo in range(0xA1, 0xFF):
        try:
            ch = bytes((hi, lo)).decode("gb2312")
        except UnicodeDecodeError:
            continue
```

结果是 **7445 个码位 = 6763 个汉字 + 682 个符号**（A1–A9 区的中文标点、希腊
字母、假名等）。合法组合占不满 94×94 的方阵，所以必须靠解码试错而不是穷举。

在此之上补两类：

- **ASCII 0x20–0x7E**：中文段落里的英文单词与数字由 SC 族自带，否则会和标点错位。
- **`cjk_punct_chars()`**：U+3000–U+303F 与 U+FF01–U+FF65。中文输入法打出的
  「」【】、全角括号不在 GB2312 里，但排版里天天出现。

## 重跑

```bash
pip install fonttools                      # 只在生成时需要
python tools/fonts/gen_fonts.py            # 生成到 internal/render/fonts/
python tools/fonts/gen_fonts.py --check    # 只校验已入库产物并打印体积
```

- 下载物（Google Fonts 的 css 与 ttf、instancer 的中间产物）缓存在
  `.temp/fonts-cache/`，已被 `.gitignore` 忽略；重跑不会重复下载。
- `--sc-vf` 可指定另一个 Noto Sans SC 可变字体（默认 `C:/Windows/Fonts/NotoSansSC-VF.ttf`）。
- 可重复性：`recalc_timestamp` 关掉了，同一版 fontTools + 同一输入字体产出相同
  体积；不比字节（Google Fonts 的 URL 会随版本变化），比"量级"。
- `verify()` 会拦三种错误：产物仍是可变字体、字重与文件名不符、必须覆盖的码位缺失。

## 子集范围之外的字

`EXTRA` 里的补充符号是"能命中就打包"，三族的静态实例覆盖范围不同：

| 符号 | 拉丁 | SC | 等宽 |
| --- | --- | --- | --- |
| `→ ← ↑ ↓ ↑ ∑ √ ∞ ≈ ≤ ≥` | ✗ | ✓ | 部分 |
| `★ ☆ ■ □ ● ○` | ✗ | ✓ | 部分 |
| `✓` | ✗ | ✓ | ✗ |
| `✔ ✗ ✘` | ✗ | ✗ | ✗ |

渲染器**逐字符**选字形，不是按码位选一族：先试首选族（CJK → SC，其余 → Sans，
行内代码与代码块 → Mono），再沿 `Sans → SC → Mono` 的回退链接着试（代码路径以
`Mono` 打头），取第一个**真有该字形**的 face，推进量也取自那个 face。表格前两行
的符号不在拉丁族的 cmap 里，正是靠这条链才画得出字形而不是方框；代码块里的
`→ ●` 同理。`✔ ✗ ✘` 三个装饰性符号 Noto 三族都没有，会画成方框（与「生僻字
画方框 + warn」同一条策略）。

## 许可

三族均为 SIL Open Font License 1.1。`OFL.txt` 是 Noto 项目的许可全文
（`https://github.com/notofonts/noto-fonts/blob/main/LICENSE`），随字体一起
入库并进二进制所在的分发包。
