package index

import "testing"

// 审计：索引条目的 source 可以是 file://，于是一个远程索引能让客户端去读本机的文件。
func TestAuditIndexEntryCanPointAtLocalFile(t *testing.T) {
	for _, src := range []string{
		"file:///home/victim/.aws/credentials",
		"file:///etc/shadow",
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
	} {
		if validEntry(Entry{ID: "a/b", Source: src}) {
			t.Errorf("索引条目的 source 放行了 %q", src)
		}
	}
}
