# 内嵌字体资源

`internal/render/fonts/` 下的六份 `.ttf` 由 `gen_fonts.py` 生成，经 `go:embed`
打进二进制。**脚本不是运行期依赖**：Go 代码只读已入库的文件，这里只在需要
更新字体版本或调整子集范围时手工跑一次。

## 产物

| 文件 | 体积 | 字重 | 来源 |
| --- | --- | --- | --- |
| `NotoSans-400.ttf` | 556 KB | Regular | Google Fonts · Noto Sans 静态实例 |
| `NotoSans-700.ttf` | 558 KB | Bold | 同上 |
| `NotoSansSC-400.ttf` | 2.16 MB | Regular | 本机 `NotoSansSC-VF.ttf` 钉在 `wght=400` 后按 GB2312 子集 |
| `NotoSansSC-700.ttf` | 2.16 MB | Bold | 同上，`wght=700` |
| `NotoSansMono-400.ttf` | 380 KB | Regular | Google Fonts · Noto Sans Mono 静态实例 |
| `NotoSansMono-700.ttf` | 379 KB | Bold | 同上 |
| `OFL.txt` | 4.3 KB | — | SIL Open Font License 1.1 全文（OFL 要求随字体分发） |

合计 **6.12 MB**，落在 6–8 MB 的目标内。实测二进制体积 17.08 MB → 23.52 MB
（+6.44 MB，含 `x/image/font/opentype` 的解析器代码约 0.3 MB）。

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

渲染器按 `Sans → SC → Mono` 回退，所以表格里前两行是可用的；`✔ ✗ ✘` 三个
装饰性符号 Noto 三族都没有，会画成方框（与「生僻字画方框 + warn」同一条策略）。

## 许可

三族均为 SIL Open Font License 1.1。`OFL.txt` 是 Noto 项目的许可全文
（`https://github.com/notofonts/noto-fonts/blob/main/LICENSE`），随字体一起
入库并进二进制所在的分发包。
