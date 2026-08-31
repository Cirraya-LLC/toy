// Package archive 安全解压。铁律 7：路径穿越、绝对路径、逃出目标目录的符号链接一律拒绝。
package archive

import "errors"

var (
	ErrTraversal   = errors.New("archive: 压缩包里有逃出目标目录的路径")
	ErrUnsupported = errors.New("archive: 不认识这个压缩格式")
	ErrTooLarge    = errors.New("archive: 解压后体积超过上限")
)

// MaxTotalBytes 是解压后总字节数上限，防 zip bomb。
const MaxTotalBytes = 2 << 30 // 2 GiB

// IsArchive 按文件名判断是不是我们认识的压缩包。
// 认：.tar.gz .tgz .tar.xz(不解，报 ErrUnsupported) .tar .zip
// 不认的（比如裸二进制）返回 false —— 调用方应当直接当成单文件处理。
func IsArchive(name string) bool { panic("todo") }

// Extract 把 src 解压进 destDir。destDir 必须已存在。
// 必须做到：
//   - 每个成员的最终路径 filepath.Clean 之后仍在 destDir 内，否则 ErrTraversal
//   - 拒绝绝对路径成员
//   - 符号链接：目标解析后逃出 destDir 就拒绝整个包
//   - 跳过设备文件、FIFO、socket 这些非常规成员
//   - 累计字节数超过 MaxTotalBytes 就中止并清理
//
// 返回写出来的相对路径列表。
func Extract(src, destDir string) ([]string, error) { panic("todo") }
