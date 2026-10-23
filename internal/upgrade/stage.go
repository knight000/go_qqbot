package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 落位用到的两个后缀（ADR-0006 决策 5/6）：
//
//	<exe>.new  下载目标，与 exe 同目录 ⇒ 同卷，rename 才是原子替换而不是复制
//	<exe>.old  旧映像，回滚与"下次启动再删"都用它
const (
	StagedSuffix = ".new"
	OldSuffix    = ".old"
)

// Staged 是一个已经写完、校验过、可执行的 `<exe>.new`。
type Staged struct {
	Path string
	Size int64
	SHA  string
}

// StagedPath 返回某份二进制对应的落位目标。
func StagedPath(exe string) string { return exe + StagedSuffix }

// OldPath 返回某份二进制的旧映像路径。
func OldPath(exe string) string { return exe + OldSuffix }

// download 把产物流式写到 dst，边写边算 sha256，并限制总字节数。
//
// 三件事必须同时成立，所以只能自己写而不能"先 io.ReadAll 再校验"：
//   - 不把 40 MB 放进内存；
//   - 边下边算，下完就知道对不对；
//   - 有上限，被投毒或配错的元数据不能把磁盘写满。
//
// 失败时一定删掉 dst：磁盘上留半个二进制，下一次启动的 .old 清理与回滚
// 都会把它当"上一版"处理。
func (c *client) download(ctx context.Context, url, dst, wantSHA string, maxBytes int64) (Staged, error) {
	if maxBytes <= 0 {
		maxBytes = MaxAssetBytes
	}
	body, err := c.get(ctx, c.body, url, "application/octet-stream")
	if err != nil {
		return Staged{}, err
	}
	defer body.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Staged{}, fmt.Errorf("创建落位目录: %w", err)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return Staged{}, fmt.Errorf("创建 %s: %w", dst, err)
	}

	h := sha256.New()
	// LimitReader 多给 1 字节：读满 maxBytes+1 就说明超限，
	// 而"恰好等于 maxBytes"必须成功。
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, maxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return Staged{}, fmt.Errorf("下载 %s: %w", url, copyErr)
	}
	if n > maxBytes {
		_ = os.Remove(dst)
		return Staged{}, fmt.Errorf("产物超过 %d MiB 上限，已放弃", maxBytes>>20)
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return Staged{}, fmt.Errorf("关闭 %s: %w", dst, closeErr)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, strings.TrimSpace(wantSHA)) {
		_ = os.Remove(dst)
		return Staged{}, fmt.Errorf("校验和不匹配，已删除下载的产物（期望 %s，实际 %s）",
			short(wantSHA), short(got))
	}
	return Staged{Path: dst, Size: n, SHA: got}, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Stage 把已经写到 <exe>.new 的内容变成"可以成为 exe"的文件：
// 权限 0755 → fsync 文件 → 校验内容真的是可执行文件 → fsync 目录项。
//
// 这是本票对「exec 失败」那条验收的落点。原验收想要的是"exec 失败时进程内
// 同步回滚"，但按决策 8 的顺序（先落位、后关停、再换盘），换盘发生在网关
// 已断、库已关之后——那时进程已经不可能"继续服务"。所以把可行性的判定
// 提前到这里：同卷（.new 与 exe 同目录，天然成立，仍然校验一次以防将来
// 有人把它改成跨目录临时文件）、目录可写（刚写完就是证据）、目标可改名
// （写入成功即成立）、内容非空且可执行（读文件头判断）。任何一条不成立
// 都在进程内失败——旧二进制与 state.json 都不动，服务继续跑。
//
// 失败时删掉 .new：留着它只会让下一次 /upgrade 或启动期回滚判断困惑。
func Stage(exe string) (Staged, error) { return stageFile(exe, StagedPath(exe)) }

// stageFile 校验 staged 这份文件能不能成为 exe：非空、同卷、可执行、可落盘。
//
// 抽出来是因为插件用另一套文件名（与插件同目录、点开头）：落位的**规则**
// 只有一套，文件名只是调用方的事。
func stageFile(exe, staged string) (Staged, error) {
	st, err := os.Stat(staged)
	if err != nil {
		return Staged{}, fmt.Errorf("找不到已下载的产物 %s: %w", staged, err)
	}
	if st.Size() == 0 {
		_ = os.Remove(staged)
		return Staged{}, errors.New("下载的产物是空文件，放弃落位")
	}
	if ok, err := sameVolume(exe, staged); err != nil {
		_ = os.Remove(staged)
		return Staged{}, fmt.Errorf("判断同卷失败: %w", err)
	} else if !ok {
		_ = os.Remove(staged)
		return Staged{}, fmt.Errorf("%s 与 %s 不在同一个卷上，rename 会退化成复制+删除，放弃落位", staged, exe)
	}
	if err := checkExecutableContent(staged); err != nil {
		_ = os.Remove(staged)
		return Staged{}, err
	}
	// 先 chmod 再 fsync：fsync 落的是内容与元数据，权限位要在它之前定下来。
	if err := os.Chmod(staged, 0o755); err != nil {
		_ = os.Remove(staged)
		return Staged{}, fmt.Errorf("设置执行权限: %w", err)
	}
	if err := syncFile(staged); err != nil {
		_ = os.Remove(staged)
		return Staged{}, err
	}
	// Unix 要 fsync 目录项，否则掉电后 rename 可能没落盘（决策 5）。
	// Windows 没有目录 fsync，靠 MoveFileEx 的语义。
	if err := syncDir(filepath.Dir(staged)); err != nil {
		_ = os.Remove(staged)
		return Staged{}, err
	}
	return Staged{Path: staged, Size: st.Size()}, nil
}

// syncFile 把文件内容刷到盘上。写完整份 40 MB 之后必须显式 fsync：
// Close 只保证写进页缓存，掉电后可能是一份被截断的二进制。
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("打开 %s 以落盘: %w", path, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	return nil
}

// checkExecutableContent 用文件头判断"这真的是可执行文件吗"。
//
// 只认三个家族：Windows PE（MZ）、Linux ELF（\x7fELF）、Mach-O（四个魔数）。
// 不认的（脚本、HTML 错误页、被截断的下载）在落位前就拒绝——把一份 200 OK
// 的 JSON 错误页当二进制换上去，症状是"重启后进程起不来"，而那条路已经
// 没法在进程内自救了。
func checkExecutableContent(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("读取 %s: %w", path, err)
	}
	defer f.Close()

	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return fmt.Errorf("%s 不足 4 字节，不是可执行文件: %w", path, err)
	}
	ok := false
	switch {
	case hdr[0] == 'M' && hdr[1] == 'Z': // Windows PE
		ok = true
	case hdr[0] == 0x7f && hdr[1] == 'E' && hdr[2] == 'L' && hdr[3] == 'F': // Linux
		ok = true
	case hdr[0] == 0xcf && hdr[1] == 0xfa && hdr[2] == 0xed && hdr[3] == 0xfe, // Mach-O 64 LE
		hdr[0] == 0xfe && hdr[1] == 0xed && hdr[2] == 0xfa && hdr[3] == 0xcf, // Mach-O 64 BE
		hdr[0] == 0xca && hdr[1] == 0xfe && hdr[2] == 0xba && hdr[3] == 0xbe: // universal
		ok = true
	}
	if !ok {
		return fmt.Errorf("下载的内容不是可执行文件（文件头 %x），放弃落位", hdr[:])
	}
	return nil
}

// checkOldIsRegular 确认 .old 是一份普通文件。
//
// 不是洁癖：Windows 的 rename 可以拿一个目录顶掉同名文件，一次"回滚"就会
// 把 exe 变成目录——目录里没有可执行的二进制，那是最坏的结局。Unix 上
// rename(dir, file) 会直接失败，但同样不该把它当成可用的回滚来源。
func checkOldIsRegular(old string) error {
	st, err := os.Stat(old)
	if err != nil {
		return fmt.Errorf("没有可回滚的旧二进制 %s: %w", old, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s 不是普通文件（%s），拒绝用它回滚", old, st.Mode().Type())
	}
	return nil
}

// CleanOld 删掉上一次升级留下的 <exe>.old。
//
// 只在启动早期调用，失败只记录不阻止启动：Windows 上杀软或旧进程可能
// 短暂持有它（决策 5 的代价），一份 40 MB 的残留不值得让机器人下线。
func CleanOld(exe string) error {
	err := os.Remove(OldPath(exe))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RemoveStaged 删掉残留的 <exe>.new（升级失败、或上一次 apply 中断）。
func RemoveStaged(exe string) error {
	err := os.Remove(StagedPath(exe))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
