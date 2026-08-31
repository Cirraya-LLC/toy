package archive

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// GNU 稀疏文件（`tar --sparse -cf x.tar tool`）的 typeflag 是 'S'，Go 的 reader
// 认它、把内容展开好了才交给我们，却【保留】这个 typeflag。它一旦落进 untar 的
// default 分支就会被和设备文件一起 continue 掉：文件静默消失，Extract 照样成功，
// 装完是一条不存在的命令。这里断言内容真的落盘。
func TestGNUSparseFileIsExtractedWithItsContent(t *testing.T) {
	outside, dest := canary(t)
	payload := []byte("this is the real payload of the tool binary")

	var m bytes.Buffer
	m.Write(oldGNUSparseHdr("tool", payload))
	rawBody(&m, payload)
	src := rawTar(t, "sparse.tar", m.Bytes())

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("稀疏文件应该被正常解出来，实际 err=%v", err)
	}
	canaryIntact(t, outside)
	if len(files) != 1 || files[0] != "tool" {
		t.Fatalf("文件列表 = %v，想要 [tool]", files)
	}
	got, err := os.ReadFile(filepath.Join(dest, "tool"))
	if err != nil {
		t.Fatalf("tool 没落盘：%v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("内容 = %q，想要 %q", got, payload)
	}
}

// 稀疏文件展开后可以远大于压缩包本身，字节上限必须照常兜住。
func TestGNUSparseFileStillHitsTheByteLimit(t *testing.T) {
	_, dest := canary(t)
	payload := bytes.Repeat([]byte("A"), 4096)

	var m bytes.Buffer
	m.Write(oldGNUSparseHdr("big", payload))
	rawBody(&m, payload)
	src := rawTar(t, "sparsebig.tar", m.Bytes())

	files, err := extractTo(src, dest, 1024)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("4 KiB 的稀疏文件在 1 KiB 上限下应该报 ErrTooLarge，实际 files=%v err=%v", files, err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "big")); !os.IsNotExist(statErr) {
		t.Fatalf("失败之后不该留下半个文件：%v", statErr)
	}
}

// 不认识的 typeflag 如果声明了内容，continue 掉等于让 Go 的 reader 把这些字节从
// gzip 流里解出来丢掉：MaxTotalBytes 数不到，几 KiB 的包能烧掉几百 MiB 的解压，
// 而且包里少了东西却安装成功。必须响亮地失败。
func TestUnknownTypeflagWithContentIsRejected(t *testing.T) {
	_, dest := canary(t)
	body := []byte("payload nobody will ever see")

	var m bytes.Buffer
	m.Write(rawHdr("blob", 'Z', "", int64(len(body)), 0o644))
	rawBody(&m, body)
	src := rawTar(t, "unknown.tar", m.Bytes())

	files, err := Extract(src, dest)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("带内容的未知类型应该被拒，实际 files=%v err=%v", files, err)
	}
}

// 对照组：没有内容的非常规成员（设备文件、FIFO、PAX 全局头）照旧静默跳过，
// 不能因为上面那条把 git archive 生成的包整个拒掉。
func TestContentlessIrregularMembersStillSkipped(t *testing.T) {
	_, dest := canary(t)
	var m bytes.Buffer
	m.Write(rawHdr("dev/null", '3', "", 0, 0o666))  // char
	m.Write(rawHdr("dev/loop0", '4', "", 0, 0o660)) // block
	m.Write(rawHdr("run/pipe", '6', "", 0, 0o600))  // fifo
	m.Write(rawHdr("volume-label", 'V', "", 0, 0o644))
	body := []byte("ok")
	m.Write(rawHdr("real.txt", '0', "", int64(len(body)), 0o644))
	rawBody(&m, body)
	src := rawTar(t, "irregular.tar", m.Bytes())

	files, err := Extract(src, dest)
	if err != nil {
		t.Fatalf("没有内容的非常规成员应该被跳过而不是报错，实际 %v", err)
	}
	if len(files) != 1 || files[0] != "real.txt" {
		t.Fatalf("文件列表 = %v，想要只有 real.txt", files)
	}
}
