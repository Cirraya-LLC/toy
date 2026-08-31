package store

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 这里是一个极小的 TOML 读写，只覆盖本包磁盘格式用到的四种形态：
// 顶层标量、字符串数组、[[表数组]]、[表.子表]。
//
// ponytail: 手写而不是用 BurntSushi/toml（规格 §10 允许用它），是因为此刻
// go.mod 里还没有那条 require，而 `go build ./...` 必须能过。等依赖进来了，
// 把 encode*/decode* 四个函数换成 toml.Marshal/Unmarshal 即可 ——
// 写出去的文件本来就是合法 TOML，格式兼容，不用迁移。

type tomlValue struct {
	str    string
	list   []string
	isList bool
}

type tomlTable map[string]tomlValue

func (t tomlTable) str(key string) string { return t[key].str }

func (t tomlTable) strs(key string) []string {
	v := t[key]
	if !v.isList {
		return nil
	}
	return v.list
}

// tomlSub 是一个 [a.b] 子表；path 是拆开并去过引号的段。
type tomlSub struct {
	path []string
	tbl  tomlTable
}

type tomlDoc struct {
	root tomlTable
	subs []tomlSub
	arrs map[string][]tomlTable // [[index]] 这种，key 是用 . 连起来的表名
}

// decodeTOML 按行解析。数组允许跨行（有人手改 config.toml 时会这么写）。
func decodeTOML(data []byte) (*tomlDoc, error) {
	doc := &tomlDoc{root: tomlTable{}, arrs: map[string][]tomlTable{}}
	cur := doc.root

	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripTOMLComment(lines[i]))
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "[["):
			body, ok := strings.CutSuffix(line, "]]")
			if !ok {
				return nil, fmt.Errorf("第 %d 行的表数组头没有闭合的 ]]：%s", i+1, line)
			}
			path, err := splitTOMLKeyPath(strings.TrimPrefix(body, "[["))
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", i+1, err)
			}
			key := strings.Join(path, ".")
			cur = tomlTable{}
			doc.arrs[key] = append(doc.arrs[key], cur)

		case strings.HasPrefix(line, "["):
			body, ok := strings.CutSuffix(line, "]")
			if !ok {
				return nil, fmt.Errorf("第 %d 行的表头没有闭合的 ]：%s", i+1, line)
			}
			path, err := splitTOMLKeyPath(strings.TrimPrefix(body, "["))
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", i+1, err)
			}
			cur = tomlTable{}
			doc.subs = append(doc.subs, tomlSub{path: path, tbl: cur})

		default:
			key, rest, ok := strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf("第 %d 行既不是表头也不是 key = value：%s", i+1, line)
			}
			// 数组可能跨行，把后面的行接上直到方括号配平。
			for strings.HasPrefix(strings.TrimSpace(rest), "[") && !tomlBracketsBalanced(rest) && i+1 < len(lines) {
				i++
				rest += " " + strings.TrimSpace(stripTOMLComment(lines[i]))
			}
			k, err := unquoteTOMLKey(strings.TrimSpace(key))
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", i+1, err)
			}
			v, err := parseTOMLValue(strings.TrimSpace(rest))
			if err != nil {
				return nil, fmt.Errorf("第 %d 行的 %s：%w", i+1, k, err)
			}
			cur[k] = v
		}
	}
	return doc, nil
}

// subsUnder 返回所有 [prefix.X] 形态的子表，key 是 X。
func (d *tomlDoc) subsUnder(prefix string) map[string]tomlTable {
	out := map[string]tomlTable{}
	for _, s := range d.subs {
		if len(s.path) == 2 && s.path[0] == prefix {
			out[s.path[1]] = s.tbl
		}
	}
	return out
}

// stripTOMLComment 去掉 # 之后的内容，但不碰引号里的 #。
func stripTOMLComment(line string) string {
	inStr, esc := false, false
	for i, r := range line {
		switch {
		case esc:
			esc = false
		case inStr && r == '\\':
			esc = true
		case r == '"':
			inStr = !inStr
		case r == '#' && !inStr:
			return line[:i]
		}
	}
	return line
}

func tomlBracketsBalanced(s string) bool {
	depth, inStr, esc := 0, false, false
	for _, r := range s {
		switch {
		case esc:
			esc = false
		case inStr && r == '\\':
			esc = true
		case r == '"':
			inStr = !inStr
		case inStr:
		case r == '[':
			depth++
		case r == ']':
			depth--
		}
	}
	return depth == 0
}

// splitTOMLKeyPath 拆 a.b."c d" 这样的表名，每段去引号。
func splitTOMLKeyPath(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("表名是空的")
	}
	var parts []string
	var cur strings.Builder
	inStr, esc := false, false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case inStr && r == '\\':
			cur.WriteRune(r)
			esc = true
		case r == '"':
			inStr = !inStr
			cur.WriteRune(r)
		case r == '.' && !inStr:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	parts = append(parts, cur.String())

	for i, p := range parts {
		k, err := unquoteTOMLKey(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		if k == "" {
			return nil, fmt.Errorf("表名 %q 里有空段", s)
		}
		parts[i] = k
	}
	return parts, nil
}

func unquoteTOMLKey(s string) (string, error) {
	if strings.HasPrefix(s, `"`) {
		v, rest, err := scanTOMLString(s)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(rest) != "" {
			return "", fmt.Errorf("键 %q 后面有多余的内容", s)
		}
		return v, nil
	}
	return s, nil
}

func parseTOMLValue(s string) (tomlValue, error) {
	if strings.HasPrefix(s, "[") {
		body, ok := strings.CutSuffix(s, "]")
		if !ok {
			return tomlValue{}, fmt.Errorf("数组没有闭合的 ]")
		}
		items, err := splitTOMLArray(strings.TrimPrefix(body, "["))
		if err != nil {
			return tomlValue{}, err
		}
		return tomlValue{list: items, isList: true}, nil
	}
	if strings.HasPrefix(s, `"`) {
		v, rest, err := scanTOMLString(s)
		if err != nil {
			return tomlValue{}, err
		}
		if strings.TrimSpace(rest) != "" {
			return tomlValue{}, fmt.Errorf("字符串后面有多余的内容：%s", rest)
		}
		return tomlValue{str: v}, nil
	}
	// 裸标量（数字、布尔）。本包的结构体全是字符串，原样留着就够。
	return tomlValue{str: s}, nil
}

func splitTOMLArray(s string) ([]string, error) {
	out := []string{}
	rest := s
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return out, nil
		}
		if strings.HasPrefix(rest, ",") {
			rest = rest[1:]
			continue
		}
		if strings.HasPrefix(rest, `"`) {
			v, r, err := scanTOMLString(rest)
			if err != nil {
				return nil, err
			}
			out, rest = append(out, v), r
			continue
		}
		// 裸元素，读到逗号为止。
		item, r, _ := strings.Cut(rest, ",")
		out = append(out, strings.TrimSpace(item))
		rest = r
	}
}

// scanTOMLString 读一个基本字符串，返回值和剩下的部分。s[0] 必须是 "。
func scanTOMLString(s string) (string, string, error) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", fmt.Errorf("这里应该是一个用双引号括起来的字符串：%s", s)
	}
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		switch c {
		case '"':
			return b.String(), s[i+1:], nil
		case '\\':
			i++
			if i >= len(s) {
				return "", "", fmt.Errorf("字符串里的反斜杠后面没有内容")
			}
			switch s[i] {
			case 'b':
				b.WriteByte('\b')
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'f':
				b.WriteByte('\f')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'u', 'U':
				n := 4
				if s[i] == 'U' {
					n = 8
				}
				if i+n >= len(s) {
					return "", "", fmt.Errorf("\\%c 后面的十六进制位数不够", s[i])
				}
				cp, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
				if err != nil {
					return "", "", fmt.Errorf("\\%c 后面不是合法的十六进制：%w", s[i], err)
				}
				b.WriteRune(rune(cp))
				i += n
			default:
				return "", "", fmt.Errorf("不认识的转义 \\%c", s[i])
			}
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", "", fmt.Errorf("字符串没有闭合的双引号")
}

// ---- 编码 ----

func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f || r == utf8.RuneError {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// tomlKey 裸键能用就用裸键，否则加引号。
func tomlKey(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		ok := r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return tomlQuote(s)
		}
	}
	return s
}

func tomlKV(b *strings.Builder, k, v string) {
	b.WriteString(tomlKey(k))
	b.WriteString(" = ")
	b.WriteString(tomlQuote(v))
	b.WriteByte('\n')
}

func tomlArrKV(b *strings.Builder, k string, vs []string) {
	b.WriteString(tomlKey(k))
	b.WriteString(" = [")
	for i, v := range vs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlQuote(v))
	}
	b.WriteString("]\n")
}

func encodeConfig(c Config) []byte {
	var b strings.Builder
	b.WriteString("# toy 的配置：索引列表、镜像列表、托管方覆盖。规格 §6。\n\n")
	tomlArrKV(&b, "mirrors", c.Mirrors)

	for _, ix := range c.Indexes {
		b.WriteString("\n[[index]]\n")
		tomlKV(&b, "name", ix.Name)
		tomlKV(&b, "url", ix.URL)
	}

	names := make([]string, 0, len(c.Hosts))
	for n := range c.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := c.Hosts[n]
		fmt.Fprintf(&b, "\n[hosts.%s]\n", tomlKey(n))
		tomlKV(&b, "raw", h.Raw)
		tomlKV(&b, "clone", h.Clone)
		tomlKV(&b, "release", h.Release)
		tomlKV(&b, "web", h.Web)
	}
	return []byte(b.String())
}

func encodeManifest(m Manifest) []byte {
	var b strings.Builder
	b.WriteString("# toy 安装清单，自动生成。删了就没法 uninstall / update。\n\n")
	tomlKV(&b, "id", m.ID)
	tomlKV(&b, "name", m.Name)
	tomlKV(&b, "version", m.Version)
	tomlKV(&b, "tag", m.Tag)
	tomlKV(&b, "host", m.Host)
	tomlKV(&b, "owner", m.Owner)
	tomlKV(&b, "repo", m.Repo)
	tomlKV(&b, "trust", m.Trust)
	tomlKV(&b, "signer", m.Signer)
	tomlKV(&b, "sha256", m.SHA256)
	tomlKV(&b, "source", m.Source)
	tomlKV(&b, "pkg_dir", m.PkgDir)
	tomlArrKV(&b, "links", m.Links)
	tomlKV(&b, "installed_at", m.InstalledAt)
	return []byte(b.String())
}
