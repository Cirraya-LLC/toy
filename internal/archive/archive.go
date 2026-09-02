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
	ErrTraversal   = errors.New("archive: path escapes the destination directory")
	ErrUnsupported = errors.New("archive: unsupported archive format")
	ErrTooLarge    = errors.New("archive: extracted size exceeds the limit")
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
//   - 跳过设备文件、FIFO、socket 这些没有内容的非常规成员；不认识又声明了内容的一律拒绝
//   - 累计字节数超过 MaxTotalBytes 就中止并清理
//
// 解完之后如果整棵树【只有一个顶层条目而且它是目录】，就把那一层剥掉
// （见 stripTopDir）。返回的相对路径列表是剥完之后的。
func Extract(src, destDir string) ([]string, error) {
	return extractTo(src, destDir, MaxTotalBytes)
}

// extractTo 是 Extract 的本体，多一个字节上限参数好让测试用一个小额度验 ErrTooLarge
// —— 不然验一次上限要真的写 2 GiB。
func extractTo(src, destDir string, limit int64) ([]string, error) {
	switch formatOf(src) {
	case kindNone:
		return nil, fmt.Errorf("%w: %s. Only .tar.gz/.tgz/.tar/.zip are supported; install a bare binary as a single file instead",
			ErrUnsupported, filepath.Base(src))
	case kindTarXz:
		return nil, fmt.Errorf("%w: %s needs an external xz to unpack. Ask the author to ship .tar.gz or .zip instead",
			ErrUnsupported, filepath.Base(src))
	}

	dest, err := filepath.Abs(destDir)
	if err != nil {
		return nil, fmt.Errorf("archive: cannot resolve %q to an absolute path: %w", destDir, err)
	}
	dest = filepath.Clean(dest)
	fi, err := os.Stat(dest)
	if err != nil {
		return nil, fmt.Errorf("archive: destination directory %s is unusable: %w", dest, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("archive: destination %s is not a directory", dest)
	}

	e := &extractor{dest: dest, limit: limit, regular: map[string]bool{}}
	if err := e.run(src); err != nil {
		// 任何一步失败都把这次写出来的东西清干净：半个包比没有包更难排查，
		// 而且 ErrTooLarge 那条路上盘里已经躺了一堆字节。
		e.cleanup()
		return nil, err
	}
	if err := e.stripTopDir(); err != nil {
		e.cleanup()
		return nil, err
	}
	return e.files, nil
}

// stripTopDir 剥掉唯一的那个顶层目录，等价于 `tar --strip-components=1`。
//
// cargo-dist 和一半的 goreleaser 配置都把内容套在一层
// `ripgrep-15.2.0-x86_64-unknown-linux-musl/` 里，而那个目录名【同时】钉死了版本
// 和 target triple —— toy.toml 的 [bin] 右边是单个字符串、不按平台变、也不展开
// {version}，根本表达不了它。不剥的话现代 Rust CLI 那一整块（ripgrep / fd / bat /
// delta / eza）一个都收不了。eget / ubi / cargo-binstall 全都剥，用户预期就是这样。
//
// 判据是【恰好一个顶层条目而且它是真目录】：
//   - 两个及以上不剥 —— 剥哪个都得丢掉另一个。
//   - 唯一那个是文件不剥（裸二进制那种包）。
//   - 唯一那个是符号链接也不剥：DirEntry.IsDir() 是 lstat 语义，符号链接为 false，
//     所以不存在"跟着一条链接把内容搬到 destDir 外面去"这回事。
//   - 空目录不剥：剥完 destDir 就空了，那不是"少一层"而是"什么都没有"。
//
// 名字只从 os.ReadDir 拿，它返回的永远是单个路径段（不会是 "." / ".."、也不含
// 分隔符），所以这里的 Join 必定落在 destDir 内 —— 铁律 7 是在 resolve() 里守的，
// 这一步没有引入第二个需要清洗的名字来源。
func (e *extractor) stripTopDir() error {
	ents, err := os.ReadDir(e.dest)
	if err != nil {
		return fmt.Errorf("archive: cannot read %s before stripping the top-level directory: %w", e.dest, err)
	}
	if len(ents) != 1 || !ents[0].IsDir() {
		return nil
	}
	top := ents[0].Name()
	topDir := filepath.Join(e.dest, top)
	inner, err := os.ReadDir(topDir)
	if err != nil {
		return fmt.Errorf("archive: cannot read %s while stripping the top-level directory: %w", topDir, err)
	}
	if len(inner) == 0 {
		return nil
	}
	prefix := top + string(filepath.Separator)
	if !e.stripKeepsLinksInside(prefix) {
		return nil
	}
	// 先把那层目录挪开再往上搬，不然 `pkg/pkg` 这种（里面有个东西和外面那层同名）
	// 会撞在一起：rename 到一个非空目录上是 ENOTEMPTY，到一个空目录上是 EISDIR，
	// 两个都会让一个本来正常的包整个装不上。挪开之后目标名字必定是空的。
	// 这个临时名字和 top 同一个父目录（也就是同一个文件系统），rename 不会跨设备。
	staged := topDir + ".toy-strip"
	if err := os.Rename(topDir, staged); err != nil {
		return fmt.Errorf("archive: cannot move top-level directory %s aside while stripping it: %w", top, err)
	}
	for _, ent := range inner {
		if err := os.Rename(filepath.Join(staged, ent.Name()), filepath.Join(e.dest, ent.Name())); err != nil {
			return fmt.Errorf("archive: stripping top-level directory %s: cannot move %s up: %w", top, ent.Name(), err)
		}
	}
	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("archive: cannot remove top-level directory %s after stripping it: %w", top, err)
	}
	// 返回给调用方的相对路径要跟着剥，否则它和盘上的布局对不上 —— 而 [bin] 右边
	// 和 toy-index 的 probe 认的都是这份列表。
	for i, f := range e.files {
		e.files[i] = strings.TrimPrefix(f, prefix)
	}
	return nil
}

// stripKeepsLinksInside 报告剥掉那一层之后，包里每一条【相对】符号链接是不是仍然
// 落在 destDir 内。
//
// 这是剥目录唯一自己引入的逃逸面，必须自己堵上：剥掉一层等于把每条链接的解析起点
// 上移一层。包内自己指自己的链接（macOS 那种 `Versions/Current -> A`）不受影响，
// 但一条先爬到包根、再顺着顶层目录名走回来的链接会：
//
//	sub/deep/back -> ../../sub/real.txt    剥之前落在 dest/sub/real.txt ✓
//	   deep/back  -> ../../sub/real.txt    剥之后落在 dest 的【父目录】 ✗
//
// 这种包就不剥，原样交给调用方：[bin] 会因为路径对不上而响亮地失败，
// 那比在包里留一条能读到 destDir 外面的链接好太多了（铁律 7）。
// 读不出来的（Lstat / Readlink 出错）一律当成不安全 —— 剥目录是便利，不是必需。
func (e *extractor) stripKeepsLinksInside(prefix string) bool {
	for _, rel := range e.files {
		full := filepath.Join(e.dest, rel)
		fi, err := os.Lstat(full)
		if err != nil {
			return false
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue // 普通文件和硬链接认的是 inode，挪一层不改变任何东西
		}
		target, err := os.Readlink(full)
		if err != nil {
			return false
		}
		moved := filepath.Join(e.dest, strings.TrimPrefix(rel, prefix))
		if e.checkLinkTarget(moved, target) != nil {
			return false
		}
	}
	return true
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
		return fmt.Errorf("%w: more than %d entries; archive discarded", ErrTooLarge, maxEntries)
	}
	return nil
}

func (e *extractor) run(src string) error {
	switch formatOf(src) {
	case kindZip:
		zr, err := zip.OpenReader(src)
		if err != nil {
			return fmt.Errorf("archive: cannot open zip %s: %w", filepath.Base(src), err)
		}
		defer zr.Close()
		return e.unzip(&zr.Reader)
	case kindTarGz:
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("archive: cannot open %s: %w", filepath.Base(src), err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("archive: %s is not valid gzip: %w", filepath.Base(src), err)
		}
		defer gz.Close()
		return e.untar(gz)
	default: // kindTar
		f, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("archive: cannot open %s: %w", filepath.Base(src), err)
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
		return "", "", fmt.Errorf("%w: an entry has no name", ErrTraversal)
	}
	if strings.HasPrefix(n, "/") || filepath.IsAbs(filepath.FromSlash(n)) {
		return "", "", fmt.Errorf("%w: entry %q is an absolute path", ErrTraversal, name)
	}
	rel = filepath.Clean(filepath.FromSlash(n))
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%w: entry %q points outside the destination directory", ErrTraversal, name)
	}
	full = filepath.Join(e.dest, rel)
	if !within(e.dest, full) {
		return "", "", fmt.Errorf("%w: entry %q resolves to %s, which is outside %s", ErrTraversal, name, full, e.dest)
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
		return fmt.Errorf("%w: symlink %s has an empty target", ErrTraversal, linkFull)
	}
	if strings.HasPrefix(t, "/") || filepath.IsAbs(filepath.FromSlash(t)) {
		return fmt.Errorf("%w: symlink %s points at absolute path %q", ErrTraversal, filepath.Base(linkFull), target)
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
				return fmt.Errorf("%w: symlink %s has target %q containing a backtracking '..', so where it resolves cannot be trusted",
					ErrTraversal, filepath.Base(linkFull), target)
			}
		default:
			seenName = true
		}
	}
	full := filepath.Clean(filepath.Join(filepath.Dir(linkFull), filepath.FromSlash(t)))
	if !within(e.dest, full) {
		return fmt.Errorf("%w: symlink %s points at %q, which resolves to %s, outside %s",
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
		return fmt.Errorf("archive: cannot resolve %s relative to %s: %w", dir, e.dest, err)
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
				return fmt.Errorf("archive: cannot create directory %s: %w", cur, err)
			}
			e.created = append(e.created, cur)
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s in the path is a symlink; refusing to write through it", ErrTraversal, cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("archive: %s already exists and is not a directory; paths inside the archive conflict", cur)
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
		return fmt.Errorf("archive: cannot write %s: %w", rel, err)
	}
	e.track(full, rel, false) // 先记账：写到一半失败也要清得掉

	// 多读一个字节就知道超没超，不用信包里自报的大小。
	remain := e.limit - e.written
	n, copyErr := io.Copy(f, io.LimitReader(r, remain+1))
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("archive: cannot write %s: %w", rel, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("archive: cannot write %s: %w", rel, closeErr)
	}
	if n > remain {
		return fmt.Errorf("%w: extracting %s passed the %d byte limit; archive discarded", ErrTooLarge, rel, e.limit)
	}
	e.written += n

	// OpenFile 的权限会被 umask 削掉，两档权限得自己钉一次。
	if err := os.Chmod(full, mode); err != nil {
		return fmt.Errorf("archive: cannot set permissions on %s: %w", rel, err)
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
		return fmt.Errorf("archive: cannot create symlink %s: %w", rel, err)
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
		return fmt.Errorf("bad target for hard link %s: %w", rel, err)
	}
	if !e.regular[targetFull] {
		return fmt.Errorf("%w: hard link %s points at %q, which is not a regular file this archive wrote earlier",
			ErrTraversal, rel, target)
	}
	if err := e.mkdirAll(filepath.Dir(full)); err != nil {
		return err
	}
	if err := e.removeExisting(full); err != nil {
		return err
	}
	if err := os.Link(targetFull, full); err != nil {
		return fmt.Errorf("archive: cannot create hard link %s: %w", rel, err)
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
		return fmt.Errorf("archive: cannot remove the existing %s: %w", full, err)
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
			return fmt.Errorf("archive: cannot read tar: %w", err)
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
		case tar.TypeReg, tar.TypeGNUSparse:
			// GNU 稀疏文件（`tar --sparse`）是【有内容的普通文件】，只是内容按稀疏段
			// 编码。Go 的 reader 已经把 tr 包成 sparseFileReader，读出来就是展开后的
			// 完整内容，但它【保留】Typeflag == 'S' 交给我们（PAX 格式的稀疏文件才会
			// 归一成 TypeReg）。落进 default 就是静默少一个文件、Extract 还返回成功。
			// 展开后可以非常大，writeFile 的字节上限正好兜住。
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
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// 设备文件、FIFO：本来就没有内容，跳过不报错。
			// 二进制包里出现这些只可能是打包工具带进来的噪音，不值得让整次安装失败。
			continue
		default:
			// 其余类型（PAX 全局头、GNU 卷标之类）本身不带内容，同样跳过。
			// 但只要它声明了内容就不能默默扔掉：Go 的 reader 会把这些字节从流里解出来
			// 丢掉，MaxTotalBytes 数不到（几 KiB 的包能烧掉几百 MiB 的解压），而
			// "少一个文件却安装成功"比装不上更难查。
			if hdr.Size > 0 {
				return fmt.Errorf("%w: entry %s has unknown type %q yet declares %d bytes of content; refusing to silently drop it",
					ErrUnsupported, rel, hdr.Typeflag, hdr.Size)
			}
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
				return fmt.Errorf("archive: cannot read zip entry %s: %w", rel, err)
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
		return "", fmt.Errorf("archive: cannot read symlink %s: %w", f.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxLinkTargetBytes+1))
	if err != nil {
		return "", fmt.Errorf("archive: cannot read symlink %s: %w", f.Name, err)
	}
	if len(b) > maxLinkTargetBytes {
		return "", fmt.Errorf("%w: %s claims to be a symlink but its target exceeds %d bytes", ErrTraversal, f.Name, maxLinkTargetBytes)
	}
	return string(b), nil
}
