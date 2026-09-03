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

// A 404 must be recognisable BY TYPE, and a 500 must never be read as a 404 no matter what
// its reason phrase says.
//
// Why this test earns its place: a layer above turns "upstream really has no toy.toml" into
// a security decision (no toy.toml means fall back to the packaging instructions inlined in
// the index entry, see rule 3 in cmd/toy). The reason phrase in resp.Status is whatever the
// server chose to send, so the moment that decision is made on error text, one upstream
// answering `HTTP/1.1 500 HTTP 404 Not Found` can flip a user from the author's declaration
// to the index's declaration on a server-side glitch -- and nothing anywhere would say so.
func TestNotFoundIsTypedNotTextual(t *testing.T) {
	for _, c := range []struct {
		name       string
		statusLine string
		wantNotEx  bool
	}{
		{"a real 404", "HTTP/1.1 404 Not Found", true},
		{"a 500 does not count", "HTTP/1.1 500 Internal Server Error", false},
		{"nor does a 500 whose reason phrase says HTTP 404", "HTTP/1.1 500 HTTP 404 Not Found", false},
		{"nor does a 403 whose reason phrase says 404", "HTTP/1.1 403 404 Not Found", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := serveStatus(t, c.statusLine)
			_, err := GetBytes(context.Background(), url, Options{Name: "toy.toml"})
			if err == nil {
				t.Fatal("none of these status codes is 200, so this should error")
			}
			if got := errors.Is(err, fs.ErrNotExist); got != c.wantNotEx {
				t.Fatalf("errors.Is(err, fs.ErrNotExist) = %v, wanted %v; err = %v", got, c.wantNotEx, err)
			}
		})
	}
}

// serveStatus starts a server that answers with one fixed status line. It uses a raw
// listener because net/http's server only ever writes its own standard reason phrases,
// and it cannot produce the ones we need to test above.
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
