#!/usr/bin/env python3
"""生成并子集化 internal/render/fonts/ 下的六份内嵌字体。

本脚本不是运行期依赖：Go 代码只读仓库里已入库的 .ttf，这里只在**更新字体资源**
时手工执行。产物可重复——同一版 fontTools + 同一输入字体，输出体积一致。

## 字体来源

| 产物 | 来源 |
| --- | --- |
| NotoSans-400/700.ttf | Google Fonts 的 Noto Sans 静态实例（css2 API，UA=Mozilla/4.0 才返回 TrueType） |
| NotoSansMono-400/700.ttf | Google Fonts 的 Noto Sans Mono 静态实例（同上） |
| NotoSansSC-400/700.ttf | 本机可变字体 NotoSansSC-VF.ttf 经 varLib.instancer 钉在 wght=400/700，再按 GB2312 子集化 |

SC 之所以必须 instance：本机 VF 的 wght 默认值是 100（Thin），而
`x/image/font/opentype` 的 FaceOptions 没有 variation axis，直接解析会出细体。

## 用法

    python tools/fonts/gen_fonts.py            # 生成到 internal/render/fonts/
    python tools/fonts/gen_fonts.py --check    # 只校验已入库产物并打印体积

依赖：fontTools >= 4.40（`pip install fonttools`）。
下载物缓存在 .temp/fonts-cache/（已被 .gitignore 忽略），重跑不会重复下载。
"""

from __future__ import annotations

import argparse
import re
import shutil
import sys
import urllib.request
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
OUT_DIR = ROOT / "internal" / "render" / "fonts"
CACHE_DIR = ROOT / ".temp" / "fonts-cache"

# 默认 UA 会被 css2 端点当成现代浏览器，返回 woff2；opentype.Parse 不认 woff2。
UA = "Mozilla/4.0"
CSS_URL = "https://fonts.googleapis.com/css2?family={family}:wght@{weights}"
OFL_URL = "https://raw.githubusercontent.com/notofonts/noto-fonts/main/LICENSE"

# 本机 VF。用 --sc-vf 指定别的来源；缓存键带上它的文件名，换了源不会命中旧实例。
DEFAULT_SC_VF = Path("C:/Windows/Fonts/NotoSansSC-VF.ttf")

WEIGHTS = (400, 700)

# EXTRA 是 markdown 排版会用、但不属于任何"必须覆盖"集合的半角符号（GB2312
# 里的全角版本由 gb2312_chars() 覆盖）。它们**能命中就打包**：三族的静态实例
# 覆盖范围不同（Google Fonts 的 Noto Sans 没有箭头与几何图形，SC 才有），
# 渲染器靠族间回退兜底，所以缺了不算错，但不能静默累积。
EXTRA = (
    "‘’“”„‟–—―…‰′″‹›※·•‥"  # 标点
    "→←↑↓↔⇒⇐⇔"  # 箭头
    "≤≥≠≈≡∞∑√∫±×÷°℃"  # 数学
    "✓✔✗✘■□●○◆◇★☆"  # 符号
)


@dataclass(frozen=True)
class Target:
    """一份产物：文件名、来源族、字重、请求字符集、必须命中的字符集。"""

    name: str
    family: str
    weight: int
    requested: frozenset[int]
    required: frozenset[int]
    subset: bool = False


def ascii_chars() -> set[int]:
    # 0x20–0x7E：空格 + 可见 ASCII。控制字符不参与排版，不打包。
    return set(range(0x20, 0x7F))


def gb2312_chars() -> set[int]:
    """枚举 GB2312 的全部合法双字节码位。

    GB2312 的字符由区字节与位字节各一个组成，但并非所有组合都合法。用
    `bytes.decode("gb2312")` 逐个试解，既不必抄一张 6763 字的字表，也不会漏掉
    非汉字段（A1–A9 区的中文标点、希腊字母、假名）。

    结果共 7445 个码位 = 6763 个汉字 + 682 个符号。
    """
    out: set[int] = set()
    for hi in range(0xA1, 0xFF):
        for lo in range(0xA1, 0xFF):
            try:
                ch = bytes((hi, lo)).decode("gb2312")
            except UnicodeDecodeError:
                continue
            out.add(ord(ch))
    return out


def cjk_punct_chars() -> set[int]:
    """GB2312 之外、中文输入法仍会打出的全角标点。"""
    # U+3000–U+303F：CJK 符号与标点（「」『』【】、波浪破折号等）
    # U+FF01–U+FF65：全角 ASCII 与全角括号
    return set(range(0x3000, 0x3040)) | set(range(0xFF01, 0xFF66))


def targets() -> list[Target]:
    ascii_ = ascii_chars()
    latin1 = set(range(0xA0, 0x100))
    gb = gb2312_chars()
    cjk_punct = cjk_punct_chars()
    extra = {ord(c) for c in EXTRA}

    cjk_all = ascii_ | gb | cjk_punct | extra

    # 拉丁与等宽族不裁剪：Google Fonts 的静态实例本来就只有 0.5/0.4 MB，裁掉
    # 希腊、西里尔、越南语与框线字符省不下多少，却会让"多一个字母变方框"的概率
    # 无谓地升高。真正需要裁剪的只有 CJK——它未裁剪时单份 10.6 MB。
    latin_all = ascii_ | latin1 | extra
    out: list[Target] = []
    for weight in WEIGHTS:
        out.append(Target(f"NotoSans-{weight}.ttf", "sans", weight,
                          frozenset(latin_all), frozenset(ascii_ | latin1)))
        # CJK 族自带拉丁：中文段落里的英文单词不该落到别的字体上（会和标点错位）。
        out.append(Target(f"NotoSansSC-{weight}.ttf", "sc", weight,
                          frozenset(cjk_all), frozenset(ascii_ | gb | cjk_punct), subset=True))
        out.append(Target(f"NotoSansMono-{weight}.ttf", "mono", weight,
                          frozenset(latin_all), frozenset(ascii_)))
    return out


def http_get(url: str) -> bytes:
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=120) as resp:
        return resp.read()


def cached(name: str, url: str) -> Path:
    """下载到缓存目录，已存在则直接复用。"""
    CACHE_DIR.mkdir(parents=True, exist_ok=True)
    path = CACHE_DIR / name
    if not (path.exists() and path.stat().st_size > 0):
        print(f"  下载 {url}")
        path.write_bytes(http_get(url))
    return path


def google_font_ttf(css_family: str, slug: str, weight: int) -> Path:
    """从 css2 端点取某一字重的 TrueType 静态实例，返回缓存文件路径。"""
    css_path = cached(
        f"{slug}.css",
        CSS_URL.format(family=css_family, weights=";".join(str(w) for w in WEIGHTS)),
    )
    css = css_path.read_text(encoding="utf-8")

    # 每个 @font-face 块是 `font-weight: NNN` 加一条 `src: url(...ttf)`。
    for block in re.findall(r"@font-face\s*\{(.*?)\}", css, re.S):
        weight_m = re.search(r"font-weight:\s*(\d+)", block)
        url_m = re.search(r"url\((https://[^)]+?\.ttf)\)", block)
        if weight_m and url_m and int(weight_m.group(1)) == weight:
            return cached(f"{slug}-{weight}.ttf", url_m.group(1))
    raise SystemExit(f"css2 响应里找不到 {css_family} wght={weight} 的 TrueType")


def instance_sc(vf: Path, weight: int) -> Path:
    """把可变字体钉在单个字重上，产出静态实例。"""
    from fontTools.ttLib import TTFont
    from fontTools.varLib.instancer import instantiateVariableFont

    out = CACHE_DIR / f"{vf.stem}-{weight}-full.ttf"
    if out.exists() and out.stat().st_size > 0:
        return out

    print(f"  实例化 {vf.name} wght={weight}")
    font = TTFont(str(vf))
    # 不刷新 head.modified：让同一输入产出可重复的字节，而不是每跑一次换一个时间戳。
    font.recalcTimestamp = False
    instantiateVariableFont(font, {"wght": weight}, inplace=True, updateFontNames=True)
    font.save(str(out))
    return out


def subset_font(src: Path, dst: Path, unicodes: frozenset[int]) -> None:
    from fontTools import subset

    options = subset.Options()
    # 只保留横排排版会用到的特性。`*` 会把 vert/palt 这些竖排特性一起带上，
    # 对横排出图毫无用处，平白多几百 KB。
    options.layout_features = ["kern", "liga", "clig", "calt", "ccmp", "locl", "mark", "mkmk"]
    options.recalc_timestamp = False
    options.name_IDs = ["*"]
    options.name_legacy = True
    options.notdef_outline = True
    options.recommended_glyphs = True

    font = subset.load_font(str(src), options)
    subsetter = subset.Subsetter(options=options)
    subsetter.populate(unicodes=set(unicodes))
    subsetter.subset(font)
    subset.save_font(font, str(dst), options)


def verify(path: Path, t: Target) -> None:
    """产物必须是静态实例、字重正确、必须覆盖的字符一个不少。"""
    from fontTools.ttLib import TTFont

    font = TTFont(str(path))
    try:
        if "fvar" in font:
            raise SystemExit(f"{path.name} 仍是可变字体，opentype 只会取默认实例")
        actual = font["OS/2"].usWeightClass
        if actual != t.weight:
            raise SystemExit(f"{path.name} usWeightClass={actual}，期望 {t.weight}")

        cmap = font.getBestCmap()
        missing_required = sorted(c for c in t.required if c not in cmap)
        if missing_required:
            shown = "".join(chr(c) for c in missing_required[:40])
            raise SystemExit(
                f"{path.name} 缺少 {len(missing_required)} 个必须覆盖的码位：{shown}"
            )
        missing_optional = sorted(c for c in t.requested - t.required if c not in cmap)
        if missing_optional:
            print(f"  {path.name} 未覆盖 {len(missing_optional)} 个补充符号"
                  f"（由其他族回退）：{''.join(chr(c) for c in missing_optional[:20])}")
    finally:
        font.close()


def build(sc_vf: Path, out_dir: Path) -> int:
    if not sc_vf.exists():
        raise SystemExit(f"找不到可变字体 {sc_vf}；用 --sc-vf 指定")

    out_dir.mkdir(parents=True, exist_ok=True)
    sources = {
        "sans": lambda w: google_font_ttf("Noto+Sans", "notosans", w),
        "mono": lambda w: google_font_ttf("Noto+Sans+Mono", "notosansmono", w),
        "sc": lambda w: instance_sc(sc_vf, w),
    }

    total = 0
    for t in targets():
        print(f"[{t.name}] 来源 {t.family} {t.weight}")
        src = sources[t.family](t.weight)
        dst = out_dir / t.name
        if t.subset:
            print(f"  子集 {len(t.requested)} 个码位")
            subset_font(src, dst, t.requested)
        else:
            shutil.copyfile(src, dst)
        verify(dst, t)
        size = dst.stat().st_size
        total += size
        print(f"  → {size / 1048576:.2f} MB")

    ofl = out_dir / "OFL.txt"
    if not ofl.exists():
        try:
            ofl.write_bytes(http_get(OFL_URL))
            print("  → OFL.txt（OFL 1.1，随字体分发）")
        except OSError as exc:  # 离线时不该让已经生成好的字体白跑一趟
            print(f"  ! OFL.txt 下载失败（{exc}）；它已入库，可手动放置后再跑")

    print(f"六份字体合计 {total / 1048576:.2f} MB（目标 6–8 MB）")
    return 0


def check(out_dir: Path) -> int:
    total = 0
    for t in targets():
        path = out_dir / t.name
        if not path.exists():
            print(f"缺失 {path}")
            return 1
        verify(path, t)
        size = path.stat().st_size
        total += size
        print(f"{t.name:<24} {size / 1048576:6.2f} MB")
    print(f"{'合计':<22} {total / 1048576:6.2f} MB")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(
        description="生成内嵌字体资源", formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument("--sc-vf", type=Path, default=DEFAULT_SC_VF, help="Noto Sans SC 可变字体路径")
    ap.add_argument("--out", type=Path, default=OUT_DIR, help="产物目录")
    ap.add_argument("--check", action="store_true", help="只校验已入库产物，不重新生成")
    args = ap.parse_args()
    return check(args.out) if args.check else build(args.sc_vf, args.out)


if __name__ == "__main__":
    sys.exit(main())
