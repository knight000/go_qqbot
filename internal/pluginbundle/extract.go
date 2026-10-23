package pluginbundle

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Option 是 Extract 的可选项。零值即默认行为。
type Option func(*extractOptions)

type extractOptions struct {
	perm fs.FileMode
}

// WithPerm 覆盖释放出来的文件的权限位（默认 0o755）。
//
// 默认值不是随便挑的：插件是子进程的可执行文件，没有 +x 位在 Unix 上
// 就是"探测失败、插件静默缺失"。
func WithPerm(mode fs.FileMode) Option {
	return func(o *extractOptions) { o.perm = mode }
}

// Extract 把内嵌副本里**缺失**的文件释放到 dst；已存在的文件一律跳过，
// 绝不覆盖（ADR-0005 决策 11：内嵌副本只作释放源）。
//
// 这是 `qqbot init --plugins` 的实现原语（CLI 面归 #42）：核心启动期不调用
// 它。逐个文件判断"存在即跳过"而不是"目录非空就整轮放弃"，所以误删一个
// 插件后重跑一次只会补回那一个，用户自己换过的版本不受影响。
//
// dst 不存在时按需创建。本平台没有出厂副本时返回 ErrUnavailable。
func Extract(dst string, opts ...Option) error {
	if embeddedFS == nil {
		return ErrUnavailable
	}
	return extractFS(embeddedFS, dst, opts...)
}

// extractFS 是 Extract 的可注入版本：测试用 fstest.MapFS 观察释放语义，
// 不必依赖某个平台真的编过产物。
func extractFS(src fs.FS, dst string, opts ...Option) error {
	o := extractOptions{perm: 0o755}
	for _, opt := range opts {
		opt(&o)
	}

	names, err := namesOf(src)
	if err != nil {
		return fmt.Errorf("读取内嵌插件副本: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("创建插件目录 %s: %w", dst, err)
	}
	for _, name := range names {
		if err := extractOne(src, dst, name, o.perm); err != nil {
			return err
		}
	}
	return nil
}

// extractOne 释放单个插件。O_EXCL 同时承担两件事：判定"已存在"和
// 防止并发释放覆盖别人刚写好的文件。
func extractOne(src fs.FS, dst, name string, perm fs.FileMode) error {
	target := filepath.Join(dst, name)
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("释放插件 %s 到 %s: %w", name, target, err)
	}

	data, err := fs.ReadFile(src, name)
	if err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// 半截文件比没有文件更糟：它会被当成一个插件去探测，然后每次启动
		// 报一条看不懂的解析错误。清掉，下次再试。
		_ = os.Remove(target)
		return fmt.Errorf("释放插件 %s 到 %s: %w", name, target, err)
	}
	return nil
}
