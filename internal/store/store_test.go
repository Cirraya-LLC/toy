package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
)

// newStore 开一个临时 store，并把 PATH 清空 —— 遮蔽检查读的是真实 PATH，
// 不隔离的话测试结果取决于跑测试的机器上装了什么。
func newStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("PATH", "")
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// mkexec 造一个可执行文件，返回它的路径。
func mkexec(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenCreatesLayout(t *testing.T) {
	tests := []struct {
		name    string
		root    func(t *testing.T) string // 返回传给 Open 的 root
		toyHome string
		wantEnv bool // 期望落在 TOY_HOME 上
	}{
		{name: "显式 root", root: func(t *testing.T) string { return t.TempDir() }},
		{name: "root 为空时用 TOY_HOME", root: func(t *testing.T) string { return "" }, toyHome: "x", wantEnv: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := tc.root(t)
			if tc.toyHome != "" {
				root = ""
				t.Setenv("TOY_HOME", filepath.Join(base, tc.toyHome))
			}
			s, err := Open(root)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if tc.wantEnv && s.Root != filepath.Join(base, tc.toyHome) {
				t.Fatalf("Root = %q，想要 TOY_HOME 指的那个", s.Root)
			}
			for _, d := range []string{
				s.BinDir(), s.CacheDir(), s.KeysDir(), s.KnownDir(), s.IndexDir(),
				filepath.Join(s.Root, dirPkg), filepath.Join(s.Root, dirInstalled),
			} {
				fi, err := os.Stat(d)
				if err != nil {
					t.Errorf("目录没建出来 %s: %v", d, err)
					continue
				}
				if !fi.IsDir() {
					t.Errorf("%s 不是目录", d)
				}
			}
		})
	}
}

// 私钥放 keys/，别人不该读得到。
func TestOpenKeysDirIsPrivate(t *testing.T) {
	s := newStore(t)
	fi, err := os.Stat(s.KeysDir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("keys/ 权限 = %o，想要 700（里面是私钥）", perm)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("第二次 Open 应该也成功：%v", err)
	}
}

func TestOpenRelativeRootBecomesAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	s, err := Open("toyhome")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(s.Root) {
		t.Fatalf("Root = %q，应该是绝对路径", s.Root)
	}
}

func TestPkgDirIsAlwaysInsideRoot(t *testing.T) {
	s := newStore(t)
	pkgRoot := filepath.Join(s.Root, dirPkg)
	tests := []struct {
		name                      string
		host, owner, pkg, version string
	}{
		{"正常", "github", "Derrity", "tinyserve", "1.2.3"},
		{"路径穿越", "..", "..", "..", ".."},
		{"带斜杠", "gh/x", "a/../..", "n", "v/../.."},
		{"空段", "", "", "", ""},
		{"绝对路径", "/etc", "/etc", "passwd", "/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.PkgDir(tc.host, tc.owner, tc.pkg, tc.version)
			rel, err := filepath.Rel(pkgRoot, got)
			if err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("PkgDir = %q 跑到 %q 外面去了", got, pkgRoot)
			}
			// 四段结构不能被压扁
			if n := len(strings.Split(rel, string(os.PathSeparator))); n != 4 {
				t.Fatalf("PkgDir 相对路径 %q 有 %d 段，想要 4 段", rel, n)
			}
		})
	}
}

func TestConfigRoundTrip(t *testing.T) {
	s := newStore(t)

	// 全新的 store 读出来就是内置默认。
	got, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.Indexes) != 1 || got.Indexes[0].URL == "" {
		t.Fatalf("全新 store 应该有一个内置索引，拿到 %+v", got.Indexes)
	}

	want := Config{
		Indexes: []IndexRef{
			{Name: "default", URL: "https://example.com/index.json"},
			{Name: "本地", URL: "file:///tmp/idx.json"},
		},
		Mirrors: []string{"https://m1.example.com", "https://m2.example.com/带#号和\"引号\""},
		Hosts: map[string]host.Host{
			"mygit": {
				Name:    "mygit",
				Raw:     "https://git.example.com/{owner}/{repo}/raw/tag/{ref}/{path}",
				Clone:   "https://git.example.com/{owner}/{repo}",
				Release: "https://git.example.com/{owner}/{repo}/releases/download/{tag}/{asset}",
				Web:     "https://git.example.com/{owner}/{repo}",
			},
		},
	}
	if err := s.SaveConfig(want); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err = s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(got.Indexes) != 2 || got.Indexes[0] != want.Indexes[0] || got.Indexes[1] != want.Indexes[1] {
		t.Errorf("Indexes = %+v，想要 %+v", got.Indexes, want.Indexes)
	}
	if strings.Join(got.Mirrors, "|") != strings.Join(want.Mirrors, "|") {
		t.Errorf("Mirrors = %q，想要 %q", got.Mirrors, want.Mirrors)
	}
	if got.Hosts["mygit"] != want.Hosts["mygit"] {
		t.Errorf("Hosts[mygit] = %+v，想要 %+v", got.Hosts["mygit"], want.Hosts["mygit"])
	}
}

// 用户 `toy index rm` 删光索引之后，不能又被默认值填回来。
func TestLoadConfigKeepsAnEmptyIndexList(t *testing.T) {
	s := newStore(t)
	if err := s.SaveConfig(Config{Indexes: nil, Mirrors: []string{}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Indexes) != 0 {
		t.Fatalf("Indexes = %+v，用户删光了就该是空的", got.Indexes)
	}
}

func TestLoadConfigRejectsGarbage(t *testing.T) {
	s := newStore(t)
	if err := os.WriteFile(s.configPath(), []byte("[hosts.github\nraw = "), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadConfig(); err == nil {
		t.Fatal("坏掉的 config.toml 应该报错，而不是静默变成空配置")
	}
}

func sampleManifest(id, name string) Manifest {
	return Manifest{
		ID: id, Name: name, Version: "1.2.3", Tag: "v1.2.3",
		Host: "github", Owner: "Derrity", Repo: name,
		Trust: TrustHost, Signer: "", SHA256: "abc123",
		Source: "https://github.com/Derrity/" + name + "/releases/download/v1.2.3/x.tar.gz",
		PkgDir: "/root/.toy/pkg/github/Derrity/" + name + "/1.2.3",
		Links:  []string{"/root/.toy/bin/" + name},
		// 中文备注和引号都要能原样存回来
		InstalledAt: "2026-08-31T12:00:00Z",
	}
}

func TestManifestRoundTrip(t *testing.T) {
	s := newStore(t)
	want := sampleManifest("github:Derrity/tinyserve", "tinyserve")
	want.Signer = `a3f9"中文"\反斜杠`
	want.Links = append(want.Links, "/root/.toy/bin/ts")

	if err := s.SaveManifest(want); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	got, err := s.LoadManifest(want.ID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.ID != want.ID || got.Name != want.Name || got.Version != want.Version ||
		got.Tag != want.Tag || got.Host != want.Host || got.Owner != want.Owner ||
		got.Repo != want.Repo || got.Trust != want.Trust || got.Signer != want.Signer ||
		got.SHA256 != want.SHA256 || got.Source != want.Source || got.PkgDir != want.PkgDir ||
		got.InstalledAt != want.InstalledAt {
		t.Fatalf("清单没原样存回来：\n got = %+v\nwant = %+v", got, want)
	}
	if strings.Join(got.Links, "|") != strings.Join(want.Links, "|") {
		t.Fatalf("Links = %q，想要 %q", got.Links, want.Links)
	}
}

func TestLoadManifestNotInstalled(t *testing.T) {
	s := newStore(t)
	_, err := s.LoadManifest("github:nobody/nothing")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v，想要 ErrNotInstalled", err)
	}
}

func TestManifestPathStaysInInstalledDir(t *testing.T) {
	s := newStore(t)
	installed := filepath.Join(s.Root, dirInstalled)
	ids := []string{
		"github:Derrity/tinyserve",
		"../../../etc/passwd",
		"..",
		"https://evil.example.com/a/b/toy.toml",
		strings.Repeat("长", 200),
	}
	seen := map[string]string{}
	for _, id := range ids {
		p, err := s.manifestPath(id)
		if err != nil {
			t.Fatalf("manifestPath(%q): %v", id, err)
		}
		if filepath.Dir(p) != installed {
			t.Errorf("manifestPath(%q) = %q，跑出 %q 了", id, p, installed)
		}
		if prev, dup := seen[p]; dup {
			t.Errorf("%q 和 %q 撞到同一个文件 %q", id, prev, p)
		}
		seen[p] = id
	}
}

// 转义是多对一的，靠 hash 后缀区分。
func TestManifestPathDistinguishesIDsThatEscapeAlike(t *testing.T) {
	s := newStore(t)
	a, _ := s.manifestPath("github:a/b")
	b, _ := s.manifestPath("github/a:b")
	if a == b {
		t.Fatalf("两个不同的 id 落到同一个清单文件 %q —— hash 后缀没起作用", a)
	}
	if !strings.HasPrefix(filepath.Base(a), "github_a_b-") {
		t.Fatalf("文件名 %q 不像转义后的样子", filepath.Base(a))
	}
}

func TestManifestPathRejectsEmptyID(t *testing.T) {
	s := newStore(t)
	if _, err := s.manifestPath("   "); err == nil {
		t.Fatal("空 id 应该报错")
	}
	if err := s.SaveManifest(Manifest{}); err == nil {
		t.Fatal("SaveManifest 空 id 应该报错")
	}
}

func TestListAndRemoveManifests(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"github:z/zed", "github:a/alpha", "gitlab:m/mid"} {
		name := strings.SplitN(id, "/", 2)[1]
		if err := s.SaveManifest(sampleManifest(id, name)); err != nil {
			t.Fatal(err)
		}
	}
	// 非 .toml 的东西不能被当成清单
	if err := os.WriteFile(filepath.Join(s.Root, dirInstalled, "README"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListManifests()
	if err != nil {
		t.Fatalf("ListManifests: %v", err)
	}
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	want := []string{"github:a/alpha", "github:z/zed", "gitlab:m/mid"} // 按 id 排序
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ListManifests = %v，想要 %v（按 id 排序）", ids, want)
	}

	if err := s.RemoveManifest("github:z/zed"); err != nil {
		t.Fatalf("RemoveManifest: %v", err)
	}
	if err := s.RemoveManifest("github:z/zed"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("重复 Remove err = %v，想要 ErrNotInstalled", err)
	}
	if got, _ := s.ListManifests(); len(got) != 2 {
		t.Fatalf("删完还剩 %d 条，想要 2 条", len(got))
	}
}

func TestListManifestsEmptyStore(t *testing.T) {
	s := newStore(t)
	got, err := s.ListManifests()
	if err != nil || len(got) != 0 {
		t.Fatalf("空 store：got = %v, err = %v", got, err)
	}
}

// 坏掉的清单要吵，不能被跳过 —— 跳过等于那个包再也卸不掉。
func TestListManifestsRefusesToSkipACorruptOne(t *testing.T) {
	s := newStore(t)
	if err := s.SaveManifest(sampleManifest("github:a/b", "b")); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(s.Root, dirInstalled, "broken.toml")
	if err := os.WriteFile(bad, []byte(`name = "没有右引号`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListManifests(); err == nil {
		t.Fatal("坏清单应该让 ListManifests 报错")
	}
}

func TestFindByName(t *testing.T) {
	s := newStore(t)
	ts := sampleManifest("github:Derrity/tinyserve", "tinyserve")
	ts.Links = []string{"/x/bin/tinyserve", "/x/bin/ts"}
	other := sampleManifest("gitlab:someone/tinyserve", "tinyserve") // 同名不同 host
	solo := sampleManifest("github:me/solo", "solo")
	for _, m := range []Manifest{ts, other, solo} {
		if err := s.SaveManifest(m); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name     string
		query    string
		wantID   string
		wantErr  error
		wantList []string // 歧义时错误里必须提到的候选
	}{
		{name: "按包名唯一命中", query: "solo", wantID: "github:me/solo"},
		{name: "按仓库名唯一命中", query: "solo", wantID: "github:me/solo"},
		{name: "写全 id 直接命中", query: "github:Derrity/tinyserve", wantID: "github:Derrity/tinyserve"},
		{name: "写全另一个 id", query: "gitlab:someone/tinyserve", wantID: "gitlab:someone/tinyserve"},
		{name: "按软链名命中", query: "ts", wantID: "github:Derrity/tinyserve"},
		{name: "没装过", query: "nope", wantErr: ErrNotInstalled},
		{name: "空名字", query: "  ", wantErr: nil},
		{
			name:     "歧义要列候选",
			query:    "tinyserve",
			wantList: []string{"github:Derrity/tinyserve", "gitlab:someone/tinyserve"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.FindByName(tc.query)
			switch {
			case tc.wantList != nil:
				if err == nil {
					t.Fatalf("想要歧义报错，拿到 %+v", got)
				}
				for _, id := range tc.wantList {
					if !strings.Contains(err.Error(), id) {
						t.Errorf("错误里没提到候选 %q：%v", id, err)
					}
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v，想要 %v", err, tc.wantErr)
				}
			case tc.wantID != "":
				if err != nil {
					t.Fatalf("FindByName(%q): %v", tc.query, err)
				}
				if got.ID != tc.wantID {
					t.Fatalf("ID = %q，想要 %q", got.ID, tc.wantID)
				}
			default:
				if err == nil {
					t.Fatal("想要报错")
				}
			}
		})
	}
}

func TestLinkHappyPath(t *testing.T) {
	s := newStore(t)
	target := mkexec(t, t.TempDir(), "tinyserve")

	got, err := s.Link("tinyserve", target, false)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if got != filepath.Join(s.BinDir(), "tinyserve") {
		t.Fatalf("链接路径 = %q", got)
	}
	dest, err := os.Readlink(got)
	if err != nil {
		t.Fatalf("不是软链：%v", err)
	}
	if dest != target {
		t.Fatalf("指向 %q，想要 %q", dest, target)
	}
}

// 铁律 5：PATH 上已有同名程序就不能装，除非 --force。
func TestLinkRefusesToShadowPATH(t *testing.T) {
	s := newStore(t)
	sysDir := t.TempDir()
	sudo := mkexec(t, sysDir, "sudo")
	t.Setenv("PATH", sysDir)
	target := mkexec(t, t.TempDir(), "payload")

	_, err := s.Link("sudo", target, false)
	if !errors.Is(err, ErrShadow) {
		t.Fatalf("err = %v，想要 ErrShadow", err)
	}
	// 错误文本必须点名被遮蔽的那个程序的完整路径，否则用户没法判断该不该 --force
	if !strings.Contains(err.Error(), sudo) {
		t.Fatalf("错误里没说被遮蔽的是哪个程序（想要 %q）：%v", sudo, err)
	}
	if _, statErr := os.Lstat(filepath.Join(s.BinDir(), "sudo")); statErr == nil {
		t.Fatal("被拒之后不该留下任何链接")
	}

	// --force 才放行
	got, err := s.Link("sudo", target, true)
	if err != nil {
		t.Fatalf("force 应该放行：%v", err)
	}
	if dest, _ := os.Readlink(got); dest != target {
		t.Fatalf("force 之后指向 %q，想要 %q", dest, target)
	}
}

// BinDir 自己在 PATH 上时，LookPath 找到的是我们自己的链接 —— 那不叫遮蔽。
func TestLinkOurOwnBinDirIsNotShadow(t *testing.T) {
	s := newStore(t)
	t.Setenv("PATH", s.BinDir())
	target := mkexec(t, t.TempDir(), "tinyserve")

	if _, err := s.Link("tinyserve", target, false); err != nil {
		t.Fatalf("第一次 Link: %v", err)
	}
	// 现在 bin/tinyserve 既在 PATH 上又在 BinDir 里：应该报「已存在」而不是「遮蔽」
	_, err := s.Link("tinyserve", target, false)
	if !errors.Is(err, ErrLinkExists) {
		t.Fatalf("err = %v，想要 ErrLinkExists（自己的链接不算遮蔽）", err)
	}
}

// PATH 项写的是 BinDir 的软链或相对路径时，也不该被当成遮蔽。
func TestLinkPATHEntryViaSymlinkIsNotShadow(t *testing.T) {
	s := newStore(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(s.BinDir(), alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", alias)
	target := mkexec(t, t.TempDir(), "tinyserve")

	if _, err := s.Link("tinyserve", target, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Link("tinyserve", target, false); !errors.Is(err, ErrLinkExists) {
		t.Fatalf("err = %v，想要 ErrLinkExists —— 经软链看到的还是自己的 BinDir", err)
	}
}

func TestLinkExistsAndForceReplaces(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	old := mkexec(t, dir, "v1")
	fresh := mkexec(t, dir, "v2")

	if _, err := s.Link("tool", old, false); err != nil {
		t.Fatal(err)
	}
	_, err := s.Link("tool", fresh, false)
	if !errors.Is(err, ErrLinkExists) {
		t.Fatalf("err = %v，想要 ErrLinkExists", err)
	}
	if dest, _ := os.Readlink(filepath.Join(s.BinDir(), "tool")); dest != old {
		t.Fatal("被拒之后旧链接必须原样留着")
	}

	link, err := s.Link("tool", fresh, true)
	if err != nil {
		t.Fatalf("force 替换: %v", err)
	}
	if dest, _ := os.Readlink(link); dest != fresh {
		t.Fatalf("指向 %q，想要新的 %q", dest, fresh)
	}
	// 原子替换：不能留下临时文件
	entries, _ := os.ReadDir(s.BinDir())
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("bin/ 里有 %v，想要只剩 tool（临时名没清干净）", names)
	}
}

// force 替换一个普通文件（不是软链）也要能成 —— rename 会直接顶掉。
func TestLinkForceReplacesARegularFile(t *testing.T) {
	s := newStore(t)
	stale := filepath.Join(s.BinDir(), "tool")
	if err := os.WriteFile(stale, []byte("旧的"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := mkexec(t, t.TempDir(), "v1")
	if _, err := s.Link("tool", target, true); err != nil {
		t.Fatalf("force: %v", err)
	}
	if dest, _ := os.Readlink(stale); dest != target {
		t.Fatalf("指向 %q，想要 %q", dest, target)
	}
}

func TestLinkRejectsBadInput(t *testing.T) {
	s := newStore(t)
	good := mkexec(t, t.TempDir(), "ok")

	tests := []struct {
		name, link, target string
	}{
		{"空名字", "", good},
		{"带斜杠", "../evil", good},
		{"带斜杠2", "a/b", good},
		{"点", ".", good},
		{"点点", "..", good},
		{"目标不存在", "tool", filepath.Join(t.TempDir(), "没有这个文件")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Link(tc.link, tc.target, false); err == nil {
				t.Fatalf("Link(%q, %q) 应该报错", tc.link, tc.target)
			}
			// force 也不能放行结构性错误
			if _, err := s.Link(tc.link, tc.target, true); err == nil {
				t.Fatalf("Link(%q, %q, force) 也应该报错", tc.link, tc.target)
			}
		})
	}
}

// 相对目标会相对 bin/ 解析，必须存成绝对路径。
func TestLinkStoresAnAbsoluteTarget(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := newStore(t)
	if err := os.WriteFile("tool.sh", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link, err := s.Link("tool", "tool.sh", false)
	if err != nil {
		t.Fatal(err)
	}
	dest, _ := os.Readlink(link)
	if !filepath.IsAbs(dest) {
		t.Fatalf("目标 = %q，应该是绝对路径", dest)
	}
	if _, err := os.Stat(link); err != nil {
		t.Fatalf("链接是断的：%v", err)
	}
}

func TestUnlink(t *testing.T) {
	s := newStore(t)
	target := mkexec(t, t.TempDir(), "tool")
	link, err := s.Link("tool", target, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Unlink(link); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("链接没删掉")
	}
	// 可重入：uninstall 半路挂掉再跑一次不能报错
	if err := s.Unlink(link); err != nil {
		t.Fatalf("重复 Unlink 应该是空操作：%v", err)
	}
	// 目标本身不能被删
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("Unlink 不该动链接指向的文件：%v", err)
	}
}

// 坏掉的清单不能变成「删 /usr/bin/sudo」的授权。
func TestUnlinkRefusesOutsideBinDir(t *testing.T) {
	s := newStore(t)
	outside := mkexec(t, t.TempDir(), "sudo")
	for _, p := range []string{outside, filepath.Join(s.BinDir(), "sub", "x"), s.Root} {
		if err := s.Unlink(p); err == nil {
			t.Errorf("Unlink(%q) 应该被拒", p)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("被拒的文件不能被删掉：%v", err)
	}
}

func TestInPath(t *testing.T) {
	s := newStore(t)
	other := t.TempDir()
	aliasDir := t.TempDir()
	alias := filepath.Join(aliasDir, "link-to-bin")
	if err := os.Symlink(s.BinDir(), alias); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"不在", other, false},
		{"空 PATH", "", false},
		{"精确匹配", s.BinDir(), true},
		{"混在中间", other + ":" + s.BinDir() + ":/usr/bin", true},
		{"末尾带斜杠", s.BinDir() + string(os.PathSeparator), true},
		{"路径里有 ..", filepath.Join(s.BinDir(), "..", dirBin), true},
		{"经过符号链接", alias, true},
		{"空项当成当前目录", ":" + other, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			if got := s.InPath(); got != tc.want {
				t.Fatalf("InPath() = %v，想要 %v（PATH=%q）", got, tc.want, tc.path)
			}
		})
	}
}

// PATH 里的相对项要按当前目录解析。
func TestInPathHandlesRelativeEntries(t *testing.T) {
	base := t.TempDir()
	t.Setenv("PATH", "")
	s, err := Open(filepath.Join(base, "toy"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	t.Setenv("PATH", filepath.Join("toy", dirBin))
	if !s.InPath() {
		t.Fatal("PATH 里的相对项没被当成 BinDir")
	}
	t.Setenv("PATH", ".")
	if s.InPath() {
		t.Fatal("当前目录不是 BinDir，不该算在 PATH 里")
	}
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if len(c.Indexes) != 1 {
		t.Fatalf("想要一个内置索引，拿到 %d 个", len(c.Indexes))
	}
	if c.Indexes[0].Name == "" || c.Indexes[0].URL == "" {
		t.Fatalf("内置索引 %+v 少东西", c.Indexes[0])
	}
	if len(c.Mirrors) != 0 {
		t.Fatalf("镜像列表默认应该是空的，拿到 %v", c.Mirrors)
	}
	if c.Hosts == nil {
		t.Fatal("Hosts 应该是可以直接写的空 map")
	}
	c.Hosts["x"] = host.Host{} // 不该 panic
}

// 铁律 5 的漏洞：exec.LookPath 在相对 PATH 项里找到东西时，会连同 exec.ErrDot
// 一起返回。只判 err == nil 的话，PATH 里随便一个相对项（"." 或者最常见的那个
// 空项，`export PATH=:$PATH` 就会写出来）就把整条 PATH 的遮蔽检查静默关掉，
// 于是 `[bin] sudo = "payload"` 装得上去。
func TestLinkShadowSurvivesARelativePATHEntry(t *testing.T) {
	base := t.TempDir()
	sysDir := filepath.Join(base, "sys")
	if err := os.MkdirAll(sysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sudo := mkexec(t, sysDir, "sudo")
	t.Chdir(base)

	pathVals := map[string]string{
		"相对项":        "sys",
		"空项就是当前目录":   string(os.PathListSeparator) + sysDir,
		"相对项排在绝对项前面": "sys" + string(os.PathListSeparator) + sysDir,
	}
	for name, pathVal := range pathVals {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", "")
			s, err := Open(filepath.Join(t.TempDir(), "toy"))
			if err != nil {
				t.Fatal(err)
			}
			if name == "空项就是当前目录" {
				// 当前目录里也放一个 sudo，这样空项才真的命中。
				mkexec(t, base, "sudo")
			}
			t.Setenv("PATH", pathVal)
			target := mkexec(t, t.TempDir(), "payload")

			if _, err := s.Link("sudo", target, false); !errors.Is(err, ErrShadow) {
				t.Fatalf("PATH=%q：err = %v，想要 ErrShadow（PATH 上确实有 %s）", pathVal, err, sudo)
			}
			if _, statErr := os.Lstat(filepath.Join(s.BinDir(), "sudo")); statErr == nil {
				t.Fatal("被拒之后不该留下链接")
			}
		})
	}
}

// 两个不同的包不能落到同一个安装目录：装后一个会盖掉前一个的文件，
// 卸后一个会把前一个的可执行文件一起删掉。
func TestPkgDirNeverCollides(t *testing.T) {
	s := newStore(t)
	long := strings.Repeat("x", 200)
	segs := [][4]string{
		{"github", "a/b", "n", "1"},
		{"github", "a:b", "n", "1"},
		{"github", "a_b", "n", "1"},
		{"github", long + "1", "n", "1"},
		{"github", long + "2", "n", "1"},
		{"github", "..", "n", "1"},
		{"github", ".", "n", "1"},
		{"github", "", "n", "1"},
		{"github", "Derrity", "tinyserve", "1.2.3"},
		{"github", "Derrity", "tinyserve", "1.2.3+build"},
		// 只差大小写的这几组在 macOS 默认的 APFS 上是同一个目录名，
		// 所以它们必须靠 hash 后缀分开，而不是靠文件系统区分大小写。
		{"github", "derrity", "tinyserve", "1.2.3"},
		{"github", "DERRITY", "tinyserve", "1.2.3"},
		{"github", "derrity", "TinySERVE", "1.2.3"},
		{"github", "derrity", "tinyserve", "1.2.3-RC1"},
		{"github", "derrity", "tinyserve", "1.2.3-rc1"},
	}
	seen := map[string][4]string{}
	for _, sg := range segs {
		p := s.PkgDir(sg[0], sg[1], sg[2], sg[3])
		if prev, dup := seen[p]; dup {
			t.Errorf("%v 和 %v 撞到同一个目录 %q", sg, prev, p)
		}
		seen[p] = sg
	}
	// 小写的段不能被 hash 后缀搞得没法读
	want := filepath.Join(s.Root, dirPkg, "github", "derrity", "tinyserve", "1.2.3")
	if got := s.PkgDir("github", "derrity", "tinyserve", "1.2.3"); got != want {
		t.Errorf("普通路径 = %q，想要 %q", got, want)
	}
	// 带大写的段小写化之后仍然要认得出来，只是多一段 hash。
	if got := s.PkgDir("github", "Derrity", "tinyserve", "1.2.3"); !strings.Contains(got, "/derrity-") {
		t.Errorf("大写的 owner 应该折成 derrity-<hash>，实际 %q", got)
	}
}

// 清单文件名同理：APFS 上只差大小写的两个 id 会写进同一个文件，
// 后装的那个包会把先装的清单整份盖掉，于是先装的既卸不掉也升不了。
func TestManifestPathNeverCollidesOnCaseInsensitiveFS(t *testing.T) {
	s := newStore(t)
	ids := []string{
		"github:Derrity/tinyserve",
		"github:derrity/tinyserve",
		"github:derrity/TinyServe",
		"https://example.com/toy.toml",
		"https://EXAMPLE.com/toy.toml",
	}
	seen := map[string]string{}
	for _, id := range ids {
		p, err := s.manifestPath(id)
		if err != nil {
			t.Fatalf("manifestPath(%q): %v", id, err)
		}
		fold := strings.ToLower(p) // 大小写不敏感的文件系统就是这么比的
		if prev, dup := seen[fold]; dup {
			t.Errorf("%q 和 %q 撞到同一个清单文件 %q", id, prev, p)
		}
		seen[fold] = id
	}
}

// 装一次就把密码写进 ~/.toy/installed/*.toml 的包管理器不能叫安全边界：
// 清单长期躺在磁盘上、还会被打印出来，凭据不能跟着落进去。
func TestManifestSourceDropsTheCredentials(t *testing.T) {
	s := newStore(t)
	m := sampleManifest("https://example.com/toy.toml", "toy")
	m.Source = "https://alice:hunter2@example.com/toy_1.0_linux_amd64.tar.gz"
	if err := s.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	path, err := s.manifestPath(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("密码落到磁盘上了：\n%s", raw)
	}
	got, err := s.LoadManifest(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://alice:xxxxx@example.com/toy_1.0_linux_amd64.tar.gz"
	if got.Source != want {
		t.Fatalf("source = %q，想要 %q", got.Source, want)
	}
}

// redactURL 只该动带密码的 URL，别的一律原样 —— 它也会被用在
// 「实际下载成功的那个地址」上，改坏了就是对用户说谎。
func TestRedactURLLeavesEverythingElseAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"https://example.com/toy.toml",
		"https://alice@example.com/toy.toml", // 只有用户名，没有密码
		"file:///tmp/toy.toml",
		"github:Derrity/tinyserve",
		"不是 URL 的一句话 @ ://",
	} {
		if got := redactURL(s); got != s {
			t.Errorf("redactURL(%q) = %q，应该原样返回", s, got)
		}
	}
	const in = "https://u:p@example.com/x"
	if got := redactURL(in); got != "https://u:xxxxx@example.com/x" {
		t.Errorf("redactURL(%q) = %q", in, got)
	}
}

// keys/ 里是私钥。MkdirAll 对已存在的目录是空操作，所以一个之前建松了的
// keys/ 会一直松着 —— Open 必须收紧它。
func TestOpenTightensALoosePreexistingKeysDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, dirKeys), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, dirKeys), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.KeysDir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("keys/ 权限 = %o，Open 没把已存在的目录收紧", perm)
	}
}

// 链到目录上会造出一个「存在但跑不了」的命令，和断链是同一种坏法。
func TestLinkRejectsADirectoryTarget(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	for _, force := range []bool{false, true} {
		if _, err := s.Link("tool", dir, force); err == nil {
			t.Fatalf("force=%v：把命令链到目录 %s 上应该被拒", force, dir)
		}
	}
}

// 首尾空白和控制字符的命令名在 shell 里根本敲不出来，而 "sudo " 长得像 sudo
// 却逃得过遮蔽检查（LookPath("sudo ") 什么都找不到）。
func TestLinkRejectsUntypableNames(t *testing.T) {
	s := newStore(t)
	sysDir := t.TempDir()
	mkexec(t, sysDir, "sudo")
	t.Setenv("PATH", sysDir)
	target := mkexec(t, t.TempDir(), "payload")

	for _, name := range []string{"sudo ", " sudo", "   ", "a\nb", "a\tb", "a\x00b", "a\x7fb"} {
		t.Run(strconv.Quote(name), func(t *testing.T) {
			if _, err := s.Link(name, target, false); err == nil {
				t.Fatalf("Link(%q) 应该被拒", name)
			}
			if _, err := s.Link(name, target, true); err == nil {
				t.Fatalf("Link(%q, force) 也应该被拒 —— 这是结构性错误，不是安全裁决", name)
			}
		})
	}
	entries, _ := os.ReadDir(s.BinDir())
	if len(entries) != 0 {
		t.Fatalf("bin/ 里冒出了 %d 个东西", len(entries))
	}
}

// 清单和 config 的值一路来自 toy.toml / 索引，里面塞一整段 TOML 不能把
// 别的字段改掉 —— 尤其是 trust（铁律 8：信任等级必须如实）。
func TestDiskFormatResistsTOMLInjection(t *testing.T) {
	s := newStore(t)

	m := sampleManifest("github:a/b", "b")
	m.Trust = TrustUnverified
	m.Signer = "x\"\ntrust = \"" + TrustSigned + "\"\nsigner = \"伪造的作者\"\n\"y"
	m.Source = "u\"\n[[index]]\nname = \"boom\"\n\"v"
	if err := s.SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadManifest(m.ID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Trust != TrustUnverified {
		t.Fatalf("注入成功了：trust = %q，想要 %q", got.Trust, TrustUnverified)
	}
	if got.Signer != m.Signer || got.Source != m.Source {
		t.Fatalf("值没原样存回来：\n signer = %q\n source = %q", got.Signer, got.Source)
	}

	evil := "e]\n[hosts.pwned]\nraw = \"https://attacker.example.com/{owner}\"\n["
	c := DefaultConfig()
	c.Hosts[evil] = host.Host{Name: evil, Raw: "r", Clone: "c", Release: "l", Web: "w"}
	if err := s.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	back, err := s.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if _, bad := back.Hosts["pwned"]; bad {
		t.Fatal("注入成功了：冒出一个 pwned 托管方")
	}
	if len(back.Hosts) != 1 || back.Hosts[evil].Raw != "r" {
		t.Fatalf("hosts = %+v", back.Hosts)
	}
}

// 旧版手写编码器写出来的文件（没有 [hosts] 父表头、没有缩进）必须照常读得回来 ——
// 换成 BurntSushi 不需要任何迁移。
func TestReadsTheOldHandWrittenFormat(t *testing.T) {
	s := newStore(t)
	old := `# toy 的配置：索引列表、镜像列表、托管方覆盖。规格 §6。

mirrors = ["https://m.example.com"]

[[index]]
name = "default"
url = "file:///tmp/i.json"

[hosts.gitea]
raw = "https://g/{owner}/{repo}/raw/tag/{ref}/{path}"
clone = "https://g/{owner}/{repo}"
release = "https://g/{owner}/{repo}/releases/download/{tag}/{asset}"
web = "https://g/{owner}/{repo}"
`
	if err := os.WriteFile(filepath.Join(s.Root, configName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Indexes) != 1 || c.Indexes[0].Name != "default" || c.Indexes[0].URL != "file:///tmp/i.json" {
		t.Fatalf("index = %+v", c.Indexes)
	}
	if len(c.Mirrors) != 1 || c.Mirrors[0] != "https://m.example.com" {
		t.Fatalf("mirrors = %+v", c.Mirrors)
	}
	h, ok := c.Hosts["gitea"]
	if !ok || h.Name != "gitea" || h.Clone != "https://g/{owner}/{repo}" {
		t.Fatalf("hosts = %+v", c.Hosts)
	}

	oldManifest := `# toy 安装清单，自动生成。

id = "github:a/b"
name = "b"
version = "1.0.0"
links = ["/x/bin/b"]
trust = "host"
`
	p := filepath.Join(s.Root, dirInstalled, "x.toml")
	if err := os.WriteFile(p, []byte(oldManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "github:a/b" || m.Trust != "host" || len(m.Links) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
}
