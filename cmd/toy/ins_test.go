package main

import (
	"strings"
	"testing"

	"github.com/Cirraya-LLC/toy/internal/host"
	"github.com/Cirraya-LLC/toy/internal/ref"
	"github.com/Cirraya-LLC/toy/internal/semver"
	"github.com/Cirraya-LLC/toy/internal/store"
)

func insTestResolved(t *testing.T, s, tag string) *resolved {
	t.Helper()
	r, err := ref.Parse(s)
	if err != nil {
		t.Fatalf("ref.Parse(%q)：%v", s, err)
	}
	res := &resolved{Ref: r, Owner: r.Owner, Repo: r.Repo, Tag: tag, Trust: store.TrustHost}
	if r.Kind == ref.KindHost {
		res.HostName = r.Host
		res.Host = host.Builtin()[r.Host]
		res.Version = semver.V{Raw: tag}
	}
	return res
}

// 升级判断错了就是装错版本，或者永远不升级。
func TestInsIsNewer(t *testing.T) {
	cases := []struct {
		old, remote string
		want        bool
	}{
		{"v1.2.0", "v1.3.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.3.0", "v1.2.0", false}, // 远端更旧就别动，不然一次降级
		{"v1.2.0", "v1.2.1", true},
		// 有一边不是 semver 就只能按「tag 变了」判断，不能当成 0.0.0
		// —— 那会把 release-2024 一路"升级"成谁也没要的东西。
		{"release-2024", "release-2025", true},
		{"release-2024", "release-2024", false},
	}
	for _, c := range cases {
		got := insIsNewer(store.Manifest{Tag: c.old}, &resolved{Tag: c.remote})
		if got != c.want {
			t.Errorf("insIsNewer(%q → %q) = %v，想要 %v", c.old, c.remote, got, c.want)
		}
	}
}

// 铁律 6：钉住的是某个 ref@version 的哈希。id 里不带 tag 的话，正常升级
// 会被报成「哈希变了」，而那句警告天天出现就没人再当回事。
func TestInsTrustIDCarriesTheTag(t *testing.T) {
	res := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	if got, want := insPackageID(res), "github:Derrity/tinyserve"; got != want {
		t.Fatalf("insPackageID = %q，想要 %q", got, want)
	}
	if got, want := insTrustID(res), "github:Derrity/tinyserve@v1.2.0"; got != want {
		t.Fatalf("insTrustID = %q，想要 %q", got, want)
	}
	// 裸地址没有 tag，id 就是那个地址本身 —— 同一个地址内容变了必须拦下来。
	u := insTestResolved(t, "https://example.com/x/toy.toml", "")
	if got := insTrustID(u); got != "https://example.com/x/toy.toml" {
		t.Fatalf("裸地址的 trust id 是 %q", got)
	}
}

// {tag} 是上游给的，展开之后才是真正拼进下载地址的那一串（铁律 7、8）。
func TestInsSafeAsset(t *testing.T) {
	if err := insSafeAsset("gitwip", "scripts/gitwip.sh"); err != nil {
		t.Fatalf("正常的相对路径不该被拒：%v", err)
	}
	for _, bad := range []string{"", "/etc/passwd", "../../x.sh", "a.sh?raw=1", "a.sh#frag"} {
		if err := insSafeAsset("gitwip", bad); err == nil {
			t.Errorf("%q 应该被拒", bad)
		}
	}
}

// 裸地址那条路没有托管方，资产只能相对 toy.toml 自己的地址解析。
func TestInsAssetURL(t *testing.T) {
	u := insTestResolved(t, "https://example.com/tools/toy.toml", "")
	got, err := insAssetURL(u, "tinyserve.tar.gz", false)
	if err != nil || got != "https://example.com/tools/tinyserve.tar.gz" {
		t.Fatalf("裸地址的资产地址 = %q（err=%v）", got, err)
	}

	h := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	rel, err := insAssetURL(h, "tinyserve_1.2.0_linux_amd64.tar.gz", false)
	if err != nil || !strings.Contains(rel, "/releases/download/v1.2.0/") {
		t.Fatalf("release 地址 = %q（err=%v）", rel, err)
	}
	// [script] 的值是仓库里的路径，走 raw 模板 —— 走 release 的话带斜杠的
	// 路径会被转义成 %2F，取回来的是一个不存在的资产。
	raw, err := insAssetURL(h, "scripts/gitwip.sh", true)
	if err != nil || !strings.Contains(raw, "scripts/gitwip.sh") {
		t.Fatalf("raw 地址 = %q（err=%v）", raw, err)
	}
}

// 铁律 8：信任等级那一行必须说清楚信的是谁。
func TestInsTrustDetail(t *testing.T) {
	h := insTestResolved(t, "github:Derrity/tinyserve", "v1.2.0")
	if got, want := insTrustDetail(h), "github.com/Derrity"; got != want {
		t.Fatalf("托管方那一档的说明 = %q，想要 %q", got, want)
	}
	h.Trust, h.Signer = store.TrustSigned, "a3f91b2c…"
	if got, want := insTrustDetail(h), "Derrity (key a3f91b2c…)"; got != want {
		t.Fatalf("已签名那一档的说明 = %q，想要 %q", got, want)
	}
}

// TestInsTrustDowngradeNeedsForce 钉住「签名不能悄悄消失」：
// 验签名本身拦不住这件事 —— 把 sig 从索引条目里删掉，客户端验都没得验，
// 那个勾就没了，而没人会注意到少了一个勾（规格 §4 的 TOFU、铁律 8）。
func TestInsTrustDowngradeNeedsForce(t *testing.T) {
	signed := &store.Manifest{Name: "tinyserve", Trust: store.TrustSigned, Signer: "a3f9…"}
	for _, c := range []struct {
		name string
		old  *store.Manifest
		res  *resolved
		want bool
	}{
		{"签名没了要拦", signed, &resolved{Trust: store.TrustHost}, true},
		{"降到未验证更要拦", signed, &resolved{Trust: store.TrustUnverified}, true},
		{"还是已签名就放行", signed, &resolved{Trust: store.TrustSigned}, false},
		{"第一次装没得比", nil, &resolved{Trust: store.TrustHost}, false},
		{"本来就没签过不算降级",
			&store.Manifest{Trust: store.TrustHost}, &resolved{Trust: store.TrustHost}, false},
	} {
		if got := insTrustDowngraded(c.old, c.res); got != c.want {
			t.Errorf("%s：insTrustDowngraded = %v，想要 %v", c.name, got, c.want)
		}
	}
}
