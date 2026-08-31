package toyfile

import (
	"net/url"
	"testing"
)

// 审计：[artifact]/[script] 的值里带 scheme 的绝对 URL 一个都没被拦下。
// checkRelPath 只挡了 "/" 开头、".."、"?#"，而 "file:///etc/shadow" 三样都不沾。
func TestAuditAbsoluteURLAssetsAreAccepted(t *testing.T) {
	for _, v := range []string{
		"file:///etc/shadow",
		"file:///dev/zero",
		"https://evil.example/x.tar.gz",
		"http://169.254.169.254/latest/meta-data/",
	} {
		src := "name = \"t\"\n[bin]\nt = \"t\"\n[artifact]\n\"linux/amd64\" = \"" + v + "\"\n"
		tf, err := Parse([]byte(src))
		if err != nil {
			t.Logf("被拦下：%q → %v", v, err)
			continue
		}
		asset, _, err := tf.Asset("linux", "amd64", "1.0.0", "v1.0.0")
		if err != nil {
			t.Logf("Asset 拦下：%q → %v", v, err)
			continue
		}
		base, _ := url.Parse("https://good.example/pkg/toy.toml")
		rel, _ := url.Parse(asset)
		t.Errorf("放行了：%q，裸地址那条路上会解析成 %s", v, base.ResolveReference(rel))
	}
}
