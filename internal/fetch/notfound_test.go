package fetch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"testing"
)

// 404 必须能被【按类型】认出来，而 500 无论原因短语写了什么都不许被认成 404。
//
// 为什么这条测试值得存在：上层拿"上游确实没有 toy.toml"当一个安全决策
// （没有就退到索引条目内联的那份打包说明，见 cmd/toy 的规则 3）。resp.Status
// 里的原因短语是服务器原样发来的，所以只要那个判断是按错误文本做的，
// 一台上游回一句 `HTTP/1.1 500 HTTP 404 Not Found` 就能让一次服务端故障
// 把用户从作者的声明切到索引的声明，而且没有任何地方会说。
func TestNotFoundIsTypedNotTextual(t *testing.T) {
	for _, c := range []struct {
		name       string
		statusLine string
		wantNotEx  bool
	}{
		{"真的 404", "HTTP/1.1 404 Not Found", true},
		{"500 不算", "HTTP/1.1 500 Internal Server Error", false},
		{"原因短语里写着 HTTP 404 的 500 也不算", "HTTP/1.1 500 HTTP 404 Not Found", false},
		{"原因短语里写着 404 的 403 也不算", "HTTP/1.1 403 404 Not Found", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := serveStatus(t, c.statusLine)
			_, err := GetBytes(context.Background(), url, Options{Name: "toy.toml"})
			if err == nil {
				t.Fatal("这些状态码都不是 200，应该报错")
			}
			if got := errors.Is(err, fs.ErrNotExist); got != c.wantNotEx {
				t.Fatalf("errors.Is(err, fs.ErrNotExist) = %v，想要 %v；err = %v", got, c.wantNotEx, err)
			}
		})
	}
}

// serveStatus 起一个只会回一条指定状态行的服务器。用裸 listener 是因为
// net/http 的服务端只写它自己那套标准原因短语，写不出上面要试的那几条。
func serveStatus(t *testing.T, statusLine string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				bufio.NewReader(c).ReadString('\n')
				fmt.Fprint(c, statusLine+"\r\nContent-Length: 0\r\n\r\n")
			}(c)
		}
	}()
	return "http://" + ln.Addr().String() + "/toy.toml"
}
