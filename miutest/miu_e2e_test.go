package miutest

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/airportr/miaospeed/interfaces"
	clashvendor "github.com/airportr/miaospeed/vendors/clash"
)

func buildClashVendor(name, payload string) interfaces.Vendor {
	return (&clashvendor.Clash{}).Build(name, payload)
}

// 需要本机先起 miux(miu 入站 127.0.0.1:34567,TLS),psk 走环境变量 MIU_PSK。
func miuVendor(t *testing.T, vision bool) interfaces.Vendor {
	psk := strings.TrimSpace(os.Getenv("MIU_PSK"))
	if psk == "" {
		t.Skip("MIU_PSK not set")
	}
	v := buildClashVendor("miu-local", fmt.Sprintf(`
name: miu-local
type: miu
server: 127.0.0.1
port: 34567
psk: "%s"
sni: miaospeed-dev
skip-cert-verify: true
udp: true
vision: %v
`, psk, vision))
	if v.Status() != interfaces.VStatusOperational {
		t.Fatalf("miu proxy not operational")
	}
	if info := v.ProxyInfo(); info.Type != interfaces.Miu {
		t.Fatalf("proxy type = %q, want Miu", info.Type)
	}
	return v
}

func TestMiuDialTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	body := strings.Repeat("miu-over-miaospeed ", 20000) // ~380 KiB,超过一个流控窗口
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))

	for _, vision := range []bool{false, true} {
		t.Run(fmt.Sprintf("vision=%v", vision), func(t *testing.T) {
			v := miuVendor(t, vision)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			url := "http://" + ln.Addr().String() + "/"
			start := time.Now()
			conn, err := v.DialTCP(ctx, url, interfaces.ROptionsTCP)
			if err != nil {
				t.Fatalf("DialTCP: %v", err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", ln.Addr())
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			defer resp.Body.Close()
			buf := new(strings.Builder)
			if _, err := bufio.NewReader(resp.Body).WriteTo(buf); err != nil {
				t.Fatalf("read body: %v", err)
			}
			if buf.String() != body {
				t.Fatalf("body mismatch: got %d bytes, want %d", buf.Len(), len(body))
			}
			t.Logf("status=%d bytes=%d elapsed=%s", resp.StatusCode, buf.Len(), time.Since(start))
		})
	}
}

func TestMiuDialUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("echo:"), b[:n]...), addr)
		}
	}()
	v := miuVendor(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := v.DialUDP(ctx, "http://"+pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.WriteTo([]byte("ping"), pc.LocalAddr()); err != nil {
		t.Fatalf("write: %v", err)
	}
	b := make([]byte, 2048)
	n, _, err := conn.ReadFrom(b)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b[:n]) != "echo:ping" {
		t.Fatalf("got %q", b[:n])
	}
}
