// Package ref 解析用户在命令行上写的包引用。
// 规格 §2 的四条路都从这里进来。
package ref

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type Kind int

const (
	KindIndex Kind = iota // "Derrity/tinyserve"       —— 要查索引
	KindHost              // "github:Derrity/tinyserve" —— 直接读那个托管方
	KindURL               // "https://…/toy.toml"       —— 完全裸奔
)

var ErrBadRef = errors.New("ref: cannot parse this package reference")

type Ref struct {
	Kind    Kind
	Host    string // KindHost 才有，如 "github"
	Owner   string
	Repo    string
	URL     string // KindURL 才有：toy.toml 的直接地址
	Version string // 用户写了 @v1.2.3 才有，空表示取最新
	Raw     string // 原始输入，报错时用
}

// owner / repo / version 都会被拼进 URL 和 ~/.toy 下的目录路径（规格 §6），
// 所以长度也要有个上限，免得一个 ref 就造出个没法处理的路径。
const maxSegment = 100

// URL 形态只认这三种 scheme（规格 §5：索引来源是 file:// 或 https://；
// §2：裸 URL 是直接指向 toy.toml 的地址）。
var allowedSchemes = map[string]bool{"http": true, "https": true, "file": true}

// Parse 认这四种形态：
//
//	Derrity/tinyserve            KindIndex
//	Derrity/tinyserve@v1.2.3     KindIndex + 指定版本
//	github:Derrity/tinyserve     KindHost
//	https://…/toy.toml           KindURL
//
// owner/repo 只允许 [A-Za-z0-9._-]，不允许空段，不允许多于一个斜杠。
func Parse(s string) (Ref, error) {
	raw := s
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("%w: empty package reference; write it as owner/repo or github:owner/repo", ErrBadRef)
	}
	// URL 形态先认出来：它里面的斜杠和冒号跟下面两种形态的规则完全不是一回事。
	if strings.Contains(s, "://") {
		return parseURL(s, raw)
	}

	// @version 只对 owner/repo 形态有意义；URL 自己就指到了具体文件。
	body, version := s, ""
	if b, v, ok := strings.Cut(s, "@"); ok {
		body, version = b, v
		if err := validVersion(v); err != nil {
			return Ref{}, fmt.Errorf("%w: %q: %v", ErrBadRef, s, err)
		}
	}

	r := Ref{Kind: KindIndex, Version: version, Raw: raw}

	// host 前缀。注意 "C:foo" 这种 Windows 盘符不能被当成 host ——
	// 所以 host 名至少两个字符、必须字母开头。
	if h, rest, ok := strings.Cut(body, ":"); ok {
		if !validHostName(h) {
			return Ref{}, fmt.Errorf("%w: %q: %q is not a valid host prefix (a host name starts with a letter, "+
				"is at least two characters long, and contains only letters, digits, . and -, as in github:owner/repo)", ErrBadRef, s, h)
		}
		r.Kind = KindHost
		r.Host = strings.ToLower(h)
		body = rest
	}

	owner, repo, ok := strings.Cut(body, "/")
	if !ok {
		return Ref{}, fmt.Errorf("%w: %q: missing the slash; write it as owner/repo (for example Derrity/tinyserve)", ErrBadRef, s)
	}
	if strings.Contains(repo, "/") {
		return Ref{}, fmt.Errorf("%w: %q: only one slash is allowed — owner/repo is the whole reference (subdirectories are not supported)", ErrBadRef, s)
	}
	if err := validSegment("owner", owner); err != nil {
		return Ref{}, fmt.Errorf("%w: %q: %v", ErrBadRef, s, err)
	}
	if err := validSegment("repo", repo); err != nil {
		return Ref{}, fmt.Errorf("%w: %q: %v", ErrBadRef, s, err)
	}
	r.Owner, r.Repo = owner, repo
	return r, nil
}

func parseURL(s, raw string) (Ref, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: %q: not a valid URL: %v", ErrBadRef, s, err)
	}
	if !allowedSchemes[u.Scheme] {
		return Ref{}, fmt.Errorf("%w: %q: only http://, https:// and file:// addresses are accepted, not %q", ErrBadRef, s, u.Scheme)
	}
	if u.Scheme == "file" {
		if u.Path == "" {
			return Ref{}, fmt.Errorf("%w: %q: a local file goes as file:///absolute/path/toy.toml (three slashes)", ErrBadRef, s)
		}
		// file:// 后面跟主机名是个陷阱：读的人只会用 u.Path，所以
		// file://evil.com/etc/passwd 读的是【本机】的 /etc/passwd，而这一行
		// 在日志、安装清单和信任等级里长得像一个远程地址（规格 §4 要求如实告知）。
		// RFC 8089 只认空主机名和 localhost，别的一律拒绝而不是悄悄丢掉。
		if h := u.Hostname(); h != "" && !strings.EqualFold(h, "localhost") {
			return Ref{}, fmt.Errorf("%w: %q: file:// must not carry the host name %q — it is ignored, and what "+
				"actually gets read is the local file %q. Write local files as file:///absolute/path/toy.toml (three slashes)", ErrBadRef, s, h, u.Path)
		}
	} else if u.Hostname() == "" {
		// 用 Hostname() 不是 Host()："https://:8080/x" 的 Host 是 ":8080"，非空但没有主机名。
		return Ref{}, fmt.Errorf("%w: %q: missing the host name; write it as https://host/path/toy.toml", ErrBadRef, s)
	}
	// 主机名大小写不敏感，统一小写：URL 会变成下载缓存和 TOFU 记录的 key（规格 §4、§6），
	// 只差大小写的两个地址必须是同一个 key，否则 Example.COM 能绕过 example.com 钉住的 hash。
	u.Host = strings.ToLower(u.Host)
	// 存 u.String() 而不是原样：scheme 和主机名已被规范化，且保证还能再被 Parse。
	return Ref{Kind: KindURL, URL: u.String(), Raw: raw}, nil
}

// validSegment 是 owner/repo 的字符白名单。这些值会被拼进下载 URL 和
// ~/.toy/pkg/<host>/<owner>/<name>/ 这样的文件系统路径，所以宁可严格：
// ".." 和空段是路径穿越，斜杠和反斜杠会凭空多出一层目录。
func validSegment(what, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s is empty", what)
	case s == "." || s == "..":
		return fmt.Errorf("%s cannot be %q", what, s)
	case len(s) > maxSegment:
		return fmt.Errorf("%s is too long (%d characters max)", what, maxSegment)
	}
	for _, c := range s {
		if !isSegmentChar(c) {
			return fmt.Errorf("%s %q contains the character %q, which is not allowed (letters, digits and . _ - only)", what, s, string(c))
		}
	}
	return nil
}

// validVersion：版本号同样会变成 pkg 下的一级目录，规则跟 owner/repo 一样，
// 只是多允许一个 semver 的构建元数据分隔符 +。
func validVersion(s string) error {
	switch {
	case s == "":
		return errors.New("write a version after the @, as in @v1.2.3")
	case s == "." || s == "..":
		return fmt.Errorf("a version cannot be %q", s)
	case len(s) > maxSegment:
		return fmt.Errorf("version is too long (%d characters max)", maxSegment)
	}
	for _, c := range s {
		if !isSegmentChar(c) && c != '+' {
			return fmt.Errorf("version %q contains the character %q, which is not allowed (letters, digits and . _ - + only)", s, string(c))
		}
	}
	return nil
}

func isSegmentChar(c rune) bool {
	return c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-'
}

// validHostName 认的是 host 表里的名字（github / gitlab / codeberg / …），
// 不是域名。至少两个字符这条是为了让 "C:foo" 这种 Windows 盘符落不进来。
func validHostName(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case (c >= '0' && c <= '9' || c == '.' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// ID 返回规范化的 "owner/repo"，索引里用它当 key。
func (r Ref) ID() string {
	if r.Owner == "" || r.Repo == "" {
		return ""
	}
	return r.Owner + "/" + r.Repo
}

// String 返回可以再被 Parse 的规范形式。
func (r Ref) String() string {
	if r.Kind == KindURL {
		return r.URL
	}
	s := r.ID()
	if r.Kind == KindHost && r.Host != "" {
		s = r.Host + ":" + s
	}
	if r.Version != "" {
		s += "@" + r.Version
	}
	return s
}
