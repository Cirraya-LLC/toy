package archive

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// skipBomb 造一个 .tar.gz：里面只有一个"我们不认识的类型"的成员，
// 头里声明自己有 size 字节。untar 的 default 分支 continue 之后，
// Go 的 tar.Reader 会把这 size 个字节从 gzip 流里【解压出来丢掉】。
// 一个字节都不落盘 —— 所以 MaxTotalBytes 从头到尾看不到它。
func skipBomb(t *testing.T, size int64, typ byte) string {
	t.Helper()
	var raw bytes.Buffer
	raw.Write(rawHdr("blob", typ, "", size, 0o644))
	zeros := make([]byte, 1<<20)
	for w := int64(0); w < size; w += int64(len(zeros)) {
		n := int64(len(zeros))
		if size-w < n {
			n = size - w
		}
		raw.Write(zeros[:n])
	}
	if pad := (512 - raw.Len()%512) % 512; pad > 0 {
		raw.Write(make([]byte, pad))
	}
	raw.Write(make([]byte, 1024))

	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(raw.Bytes())
	zw.Close()

	p := filepath.Join(t.TempDir(), "bomb.tar.gz")
	if err := os.WriteFile(p, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("类型 %q 声明 %d 字节，压缩包只有 %d 字节（压缩比 %.0f:1）",
		typ, size, gz.Len(), float64(size)/float64(gz.Len()))
	return p
}

// 512 MiB 的"跳过"数据，看解压要花多久、盘上留下什么。
func TestAuditSkippedMemberBypassesByteLimit(t *testing.T) {
	_, dest := canary(t)
	const size = 512 << 20
	src := skipBomb(t, size, 'Z') // 'Z' 不是任何已知类型 -> untar 走 default: continue

	fi, _ := os.Stat(src)
	start := time.Now()
	files, err := extractTo(src, dest, 1024) // 上限只给 1 KiB
	el := time.Since(start)
	t.Logf("下载体积 %d 字节 -> 解压耗时 %v，files=%v err=%v", fi.Size(), el, files, err)

	if err != nil {
		t.Logf("被拒了，很好")
		return
	}
	t.Errorf("上限只有 1 KiB，却把 %d MiB 从 gzip 流里解了出来，耗时 %v，"+
		"而且返回成功 —— MaxTotalBytes 只数落盘的字节，数不到这里", size>>20, el)
}

// 对照组：同样的字节数，但成员是普通文件 —— 这条路是有上限兜着的。
func TestAuditRegularMemberIsBounded(t *testing.T) {
	_, dest := canary(t)
	src := skipBomb(t, 512<<20, '0')
	start := time.Now()
	_, err := extractTo(src, dest, 1024)
	t.Logf("普通文件：耗时 %v err=%v", time.Since(start), err)
	if err == nil {
		t.Fatalf("普通文件超上限应该被拒")
	}
}

// oldGNUSparseHdr 手搓一个 old-GNU sparse 头（typeflag 'S'）。
// Go 的 reader 认它、会把稀疏段展开成完整内容（reader.go handleSparseFile），
// 而且【保留】Typeflag == TypeGNUSparse 交给调用方。
func oldGNUSparseHdr(name string, payload []byte) []byte {
	b := rawHdr(name, 'S', "", int64(len(payload)), 0o755)
	copy(b[257:265], "ustar  \x00") // GNU magic
	// 386 起是 4 个稀疏项，每项 offset[12] + numbytes[12]
	copy(b[386:398], fmt.Sprintf("%011o\x00", 0))
	copy(b[398:410], fmt.Sprintf("%011o\x00", len(payload)))
	b[482] = 0                                               // isextended = 0
	copy(b[483:495], fmt.Sprintf("%011o\x00", len(payload))) // realsize
	// 重算 checksum
	for i := 148; i < 156; i++ {
		b[i] = ' '
	}
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

// GNU sparse（typeflag 'S'）是一个【有内容的普通文件】，只是内容按稀疏段编码。
// untar 的 default 分支把它和设备文件、FIFO、socket 一起 continue 掉了 ——
// 于是这个文件静默消失，Extract 却返回成功、也没有任何警告。
// 复现方式：`tar --sparse -cf x.tar tool`（GNU tar 的 -S/--sparse）。
func TestAuditGNUSparseSilentlyDropsAFile(t *testing.T) {
	_, dest := canary(t)
	payload := []byte("this is the real payload of the tool binary")
	var m bytes.Buffer
	m.Write(oldGNUSparseHdr("tool", payload))
	rawBody(&m, payload)
	src := rawTar(t, "sparse.tar", m.Bytes())

	files, err := Extract(src, dest)
	t.Logf("files=%v err=%v", files, err)
	if err != nil {
		t.Skipf("这个手搓头 Go 没认（%v），换别的方式复现", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "tool")); statErr != nil {
		t.Errorf("Extract 返回成功、files=%v，但 tool 根本没落盘（%v）——"+
			"装完是一条不存在的命令，而且没有任何地方说过", files, statErr)
	}
}
