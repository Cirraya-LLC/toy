// Package archive 安全解压。铁律 7：路径穿越、绝对路径、逃出目标目录的符号链接一律拒绝。
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrTraversal   = errors.New("archive: 压缩包里有逃出目标目录的路径")
	ErrUnsupported = errors.New("archive: 不认识这个压缩格式")
	ErrTooLarge    = errors.New("archive: 解压后体积超过上限")
)

// MaxTotalBytes 是解压后总字节数上限，防 zip bomb。
const MaxTotalBytes = 2 << 30 // 2 GiB

// maxLinkTargetBytes 是符号链接目标的长度上限。zip 里的链接目标是文件内容，
// 不限长的话一个"符号链接"就能当成任意大小的文件塞进来。
const maxLinkTargetBytes = 4096

// maxEntries 是成员数上限。字节上限拦不住"几百万个空文件"这种耗 inode 的包。
const maxEntries = 100000

type kind int

const (
	kindNone kind = iota
	kindTarGz
	kindTar
	kindZip
	kindTarXz
)

// formatOf 按文件名（不看内容）判断格式。和 IsArchive 共用一套判断，
// 免得"说认识"和"真能解"两处规则分叉。
func formatOf(name string) kind {
	n := strings.ToLower(filepath.Base(name))
	switch {
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return kindTarGz
	case strings.HasSuffix(n, ".tar.xz"):
		return kindTarXz
	case strings.HasSuffix(n, ".tar"):
		return kindTar
	case strings.HasSuffix(n, ".zip"):
		return kindZip
	}
	return kindNone
}

// IsArchive 按文件名判断是不是我们认识的压缩包。
// 认：.tar.gz .tgz .tar.xz(不解，报 ErrUnsupported) .tar .zip
// 不认的（比如裸二进制）返回 false —— 调用方应当直接当成单文件处理。
func IsArchive(name string) bool { return formatOf(name) != kindNone }

// Extract 把 src 解压进 destDir。destDir 必须已存在。
// 必须做到：
//   - 每个成员的最终路径 filepath.Clean 之后仍在 destDir 内，否则 ErrTraversal
//   - 拒绝绝对路径成员
//   - 符号链接：目标解析后逃出 destDir 就拒绝整个包
//   - 跳过设备文件、FIFO、socket 这些非常规成员
//   - 累计字节数超过 MaxTotalBytes 就中止并清理
//
// 返回写出来的相对路径列表。
func Extract(src, destDir string) ([]string, error) {
	return extractTo(src, destDir, MaxTotalBytes)
}

// extractTo 是 Extract 的本体，多一个字节上限参数好让测试用一个小额度验 ErrTooLarge
// —— 不然验一次上限要真的写 2 GiB。
func extractTo(src, destDir string, limit int64) ([]string, error) {
	switch formatOf(src) {
	case kindNone:
		return nil, fmt.Errorf("%w：%s。只认 .tar.gz/.tgz/.tar/.zip，裸二进制请直接当单文件装",
			ErrUnsupported, filepath.Base(src))
	case kindTarXz:
		return nil, fmt.Errorf("%w：%s 要外部 xz 才解得开。请让作者改发 .tar.gz 或 .zip",
			ErrUnsupported, filepath.Base(src))
	}

	dest, err := filepath.Abs(destDir)
	if err != nil {
		return nil, fmt.Errorf("archive: 求不出 %q 的绝对路径：%w", destDir, err)
	}
	dest = filepath.Clean(dest)
	fi, err := os.Stat(dest)
	if err != nil {
		return nil, fmt.Errorf("archive: 目标目录 %s 不可用：%w", dest, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("archive: 目标 %s 不是目录", dest)
	}

	e := &extractor{dest: dest, limit: limit, regular: map[string]bool{}}
	if err := e.run(src); err != nil {
		// 任何一步失败都把这次写出来的东西清干净：半个包比没有包更难排查，
		// 而且 ErrTooLarge 那条路上盘里已经躺了一堆字节。
		e.cleanup()
		return nil, err
	}
	return e.files, nil
}

type extractor struct {
	dest    string          // 目标目录，绝对且已 Clean
	limit   int64           // 解压后总字节数上限
	written int64           // 已写出的字节
	entries int             // 已经处理过的成员数，防"条目数炸弹"
	files   []string        // 返回给调用方的相对路径（只有文件和链接，不含目录）
	created []string        // 清理用：我们真正创建过的每一个条目，按创建顺序
	regular map[string]bool // 本次写出来的普通文件（绝对路径），硬链接只认这些
}

// note 记一个成员。MaxTotalBytes 只管字节，管不住 inode：一个几十 MB 的 zip 可以
// 塞几百万个空条目，解出来就是几百万个文件。
func (e *extractor) note() error {
	e.entries++
	if e.entries > maxEntries {
		return fmt.Errorf("%w：成员数超过 %d 个，包已丢弃", ErrTooLarge, maxEntries)
	}
	return nil
}

func (e *extractor) run(src string) error {
	switch formatOf(src) {
	case kindZip:
		zr, err := zip.OpenReader(src)
		if err != nil {
			return fmt.Errorf("archive: 打不开 zip %s：%w", filepath.Base(src), err)
		}
		defer zr.Close()
		return e.unzip(&zr.Reader)
	case kindTarGz:
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("archive: 打不开 %s：%w", filepath.Base(src), err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("archive: %s 不是合法的 gzip：%w", filepath.Base(src), err)
		}
		defer gz.Close()
		return e.untar(gz)
	default: // kindTar
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("archive: 打不开 %s：%w", filepath.Base(src), err)
		}
		defer f.Close()
		return e.untar(f)
	}
}

// within 判断 p 是不是 destDir 本身或它里面的东西。
// 前缀比较必须带上分隔符：不带的话 dest=/x/dest 会把 /x/destevil 当成自己人。
func within(dest, p string) bool {
	return p == dest || strings.HasPrefix(p, dest+string(filepath.Separator))
}

// resolve 把成员名解析成 destDir 下的绝对路径，同时返回相对路径。
// 判定是纯词法的（Clean + 带分隔符的前缀比较），不看盘上有什么：
//   - 只查 strings.Contains("..") 拦不住 "a/../../b"，也会误杀 "..foo"
//   - 前缀比较必须带分隔符，否则 dest=/tmp/x 会把 /tmp/xevil 当成自己人
//
// 反斜杠一律当成分隔符：zip 里的 "..\evil" 在 Unix 上是个合法文件名，
// 但作者绝不会真想要这么个名字，而攻击者会。
func (e *extractor) resolve(name string) (full, rel string, err error) {
	n := strings.ReplaceAll(name, `\`, "/")
	if strings.TrimSpace(n) == "" {
		return "", "", fmt.Errorf("%w：有个成员没有名字", ErrTraversal)
	}
	if strings.HasPrefix(n, "/") || filepath.IsAbs(filepath.FromSlash(n)) {
		return "", "", fmt.Errorf("%w：成员 %q 是绝对路径", ErrTraversal, name)
	}
	rel = filepath.Clean(filepath.FromSlash(n))
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w：成员 %q 指到目标目录外面去了", ErrTraversal, name)
	}
	full = filepath.Join(e.dest, rel)
	if !within(e.dest, full) {
		return "", "", fmt.Errorf("%w：成员 %q 解析出来是 %s，不在 %s 里", ErrTraversal, name, full, e.dest)
	}
	return full, rel, nil
}

// checkLinkTarget 校验链接目标解析之后仍落在 destDir 内。
// 相对目标按链接自己所在的目录解析（符号链接就是这么被内核解析的）。
//
// ⚠ 这一层【不足以】保证安全，别把它当成全部：词法判定按词法父目录算，内核按真实
// 父目录算，两边深度错开一层就能爬出去。真正兜底的是 mkdirAll 里"路径中间不许有
// 符号链接"那一条（详见那里的反例），以及下面这条"目标里不许有回头的 .."。
func (e *extractor) checkLinkTarget(linkFull, target string) error {
	t := strings.ReplaceAll(target, `\`, "/")
	if strings.TrimSpace(t) == "" {
		return fmt.Errorf("%w：%s 的链接目标是空的", ErrTraversal, linkFull)
	}
	if strings.HasPrefix(t, "/") || filepath.IsAbs(filepath.FromSlash(t)) {
		return fmt.Errorf("%w：符号链接 %s 指向绝对路径 %q", ErrTraversal, filepath.Base(linkFull), target)
	}
	// 目标里不许出现"回头的 .."：开头连着的 .. 是从链接自己的父目录往上爬，而那条
	// 父链已经被 mkdirAll 保证全是真目录，所以词法和内核算出来一样；一旦先迈过一段
	// 普通名字再 ..，那一段可能是个指得比自己浅的符号链接，Clean 会把这个 .. 消掉，
	// 于是词法看着还在界内，内核却已经爬到 destDir 外面了（x -> "." 再 "x/.." 就是）。
	// 正经打包工具不会写 a/../b 这种冗余路径，拦掉零误伤。
	seenName := false
	for _, seg := range strings.Split(t, "/") {
		switch seg {
		case "", ".":
		case "..":
			if seenName {
				return fmt.Errorf("%w：符号链接 %s 的目标 %q 里有回头的 ..，解析结果不可信",
					ErrTraversal, filepath.Base(linkFull), target)
			}
		default:
			seenName = true
		}
	}
	full := filepath.Clean(filepath.Join(filepath.Dir(linkFull), filepath.FromSlash(t)))
	if !within(e.dest, full) {
		return fmt.Errorf("%w：符号链接 %s 指向 %q，解析后是 %s，不在 %s 里",
			ErrTraversal, filepath.Base(linkFull), target, full, e.dest)
	}
	return nil
}

// safeMode 只保留两档权限：原本可执行的给 0755，其余 0644。
// 绝不照抄包里的 mode —— setuid/setgid/sticky 一个都不能带出来。
func safeMode(m os.FileMode) os.FileMode {
	if m.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func (e *extractor) track(full, rel string, isDir bool) {
	e.created = append(e.created, full)
	if !isDir {
		e.files = append(e.files, rel)
	}
}

func (e *extractor) cleanup() {
	for i := len(e.created) - 1; i >= 0; i-- {
		os.Remove(e.created[i]) // 倒着删，目录里的东西先没，目录才空得掉
	}
}

// mkdirAll 从 destDir 往下逐段建目录，把真正新建的每一层记下来，好在失败时精确回滚
// —— 不能拿 WalkDir 扫一遍就删，destDir 里可能本来就有别人的东西。
//
// 每一段都要 Lstat 一次并拒绝符号链接，这是"不跟随出界链接"的真正落点：
// resolve() 的词法判定按【词法】父目录解析链接目标，内核按【真实】父目录解析，
// 两者深度一旦对不上就能爬出去。反例（三条成员各自都能过词法校验）：
//
//	x        -> "."   词法落点 dest/x，within ✓
//	x/up     -> ".."  词法 Join(dest/x, "..") = dest ✓，真实落点却是 dest/up -> dest 的父目录
//	x/up/pwned        词法 dest/x/up/pwned ✓，真实写到 destDir 外面
//
// 所以"被放行的链接必定指向 destDir 内部"这条归纳是不成立的，必须在穿过路径的
// 时候再拦一道。顺带也挡住了 destDir 里本来就躺着的符号链接。
//
// 注意不能在"这一段已经存在且是目录"时就 return —— Lstat 一个深层路径会把中间
// 的符号链接悄悄解析掉，那样最上面几段永远检查不到。
func (e *extractor) mkdirAll(dir string) error {
	if !within(e.dest, dir) {
		return nil // 往外走的路径 resolve 早就拦下了
	}
	rel, err := filepath.Rel(e.dest, dir)
	if err != nil {
		return fmt.Errorf("archive: 求不出 %s 相对 %s 的路径：%w", dir, e.dest, err)
	}
	if rel == "." {
		return nil
	}
	cur := e.dest
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if err := os.Mkdir(cur, 0o755); err != nil {
				return fmt.Errorf("archive: 建目录 %s 失败：%w", cur, err)
			}
			e.created = append(e.created, cur)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w：路径中的 %s 是符号链接，不能穿过它往里写", ErrTraversal, cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("archive: %s 已存在而且不是目录，包里的路径互相打架了", cur)
		}
	}
	return nil
}

func (e *extractor) writeFile(full, rel string, r io.Reader, mode os.FileMode) error {
	if err := e.mkdirAll(filepath.Dir(full)); err != nil {
		return err
	}
	if err := e.removeExisting(full); err != nil {
		return err
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("archive: 写 %s 失败：%w", rel, err)
	}
	e.track(full, rel, false) // 先记账：写到一半失败也要清得掉

	// 多读一个字节就知道超没超，不用信包里自报的大小。
	remain := e.limit - e.written
	n, copyErr := io.Copy(f, io.LimitReader(r, remain+1))
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("archive: 写 %s 失败：%w", rel, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("archive: 写 %s 失败：%w", rel, closeErr)
	}
	if n > remain {
		return fmt.Errorf("%w：解压到 %s 时已经超过 %d 字节，包已丢弃", ErrTooLarge, rel, e.limit)
	}
	e.written += n

	// OpenFile 的权限会被 umask 削掉，两档权限得自己钉一次。
	if err := os.Chmod(full, mode); err != nil {
		return fmt.Errorf("archive: 设置 %s 权限失败：%w", rel, err)
	}
	e.regular[full] = true
	return nil
}

func (e *extractor) writeSymlink(full, rel, target string) error {
	if err := e.checkLinkTarget(full, target); err != nil {
		return err
	}
	if err := e.mkdirAll(filepath.Dir(full)); err != nil {
		return err
	}
	if err := e.removeExisting(full); err != nil {
		return err
	}
	if err := os.Symlink(target, full); err != nil {
		return fmt.Errorf("archive: 建符号链接 %s 失败：%w", rel, err)
	}
	e.track(full, rel, false)
	return nil
}

// writeHardlink 的目标是包内路径，一样要落在 destDir 里。
//
// 而且必须是【本次解压自己写出来的普通文件】：os.Link 会跟随目标路径中间的符号
// 链接，所以光做词法校验的话，destDir 里一个指向外面的旧链接就能让包把系统上任意
// 一个文件硬链进来（拿到的是同一个 inode，不是副本）。tar 里的硬链接本来就只会
// 指向前面出现过的成员，这条限制不会误伤正经包，同时也让"目标在包里出现得更晚"
// 这种手搓包响亮地失败而不是留下半个包。
func (e *extractor) writeHardlink(full, rel, target string) error {
	targetFull, _, err := e.resolve(target)
	if err != nil {
		return fmt.Errorf("硬链接 %s 的目标有问题：%w", rel, err)
	}
	if !e.regular[targetFull] {
		return fmt.Errorf("%w：硬链接 %s 指向 %q，那不是这个包前面写出来的普通文件",
			ErrTraversal, rel, target)
	}
	if err := e.mkdirAll(filepath.Dir(full)); err != nil {
		return err
	}
	if err := e.removeExisting(full); err != nil {
		return err
	}
	if err := os.Link(targetFull, full); err != nil {
		return fmt.Errorf("archive: 建硬链接 %s 失败：%w", rel, err)
	}
	e.track(full, rel, false)
	e.regular[full] = true // 硬链接落地之后自己也是一个普通文件，可以再被链
	return nil
}

// removeExisting 处理包里同名条目出现两次的情况：直接盖会跟着旧的符号链接走。
func (e *extractor) removeExisting(full string) error {
	fi, err := os.Lstat(full)
	if err != nil || fi.IsDir() {
		return nil
	}
	if err := os.Remove(full); err != nil {
		return fmt.Errorf("archive: 清掉旧的 %s 失败：%w", full, err)
	}
	return nil
}

func (e *extractor) untar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: 读 tar 失败：%w", err)
		}
		if err := e.note(); err != nil {
			return err
		}
		full, rel, err := e.resolve(hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := e.mkdirAll(full); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := e.writeFile(full, rel, tr, safeMode(hdr.FileInfo().Mode())); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := e.writeSymlink(full, rel, hdr.Linkname); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := e.writeHardlink(full, rel, hdr.Linkname); err != nil {
				return err
			}
		default:
			// 设备文件、FIFO、socket、各种扩展头：跳过，不报错。
			// 二进制包里出现这些只可能是打包工具带进来的噪音，不值得让整次安装失败。
			continue
		}
	}
}

func (e *extractor) unzip(zr *zip.Reader) error {
	for _, f := range zr.File {
		if err := e.note(); err != nil {
			return err
		}
		full, rel, err := e.resolve(f.Name)
		if err != nil {
			return err
		}
		mode := f.Mode()
		switch {
		case mode&os.ModeSymlink != 0: // 必须排在 ModeType 前面，符号链接也在 ModeType 里
			target, err := readZipLinkTarget(f)
			if err != nil {
				return err
			}
			if err := e.writeSymlink(full, rel, target); err != nil {
				return err
			}
		case f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/"):
			if err := e.mkdirAll(full); err != nil {
				return err
			}
		case mode&os.ModeType != 0:
			continue // 设备文件 / FIFO / socket：跳过
		default:
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("archive: 读 zip 成员 %s 失败：%w", rel, err)
			}
			err = e.writeFile(full, rel, rc, safeMode(mode))
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func readZipLinkTarget(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("archive: 读符号链接 %s 失败：%w", f.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxLinkTargetBytes+1))
	if err != nil {
		return "", fmt.Errorf("archive: 读符号链接 %s 失败：%w", f.Name, err)
	}
	if len(b) > maxLinkTargetBytes {
		return "", fmt.Errorf("%w：%s 号称是符号链接，目标却超过 %d 字节", ErrTraversal, f.Name, maxLinkTargetBytes)
	}
	return string(b), nil
}
