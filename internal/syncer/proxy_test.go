package syncer

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"sftp-syncer/internal/config"
)

// startEchoTarget 启动一个回显 TCP 服务器：连接建立后立即发送 banner，
// 之后把收到的数据原样写回，用于验证隧道两端的数据通路。
func startEchoTarget(t *testing.T, banner string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				if banner != "" {
					_, _ = c.Write([]byte(banner))
				}
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// startConnectProxy 启动一个最小 HTTP CONNECT 代理：wantAuth 非空时校验
// Basic 代理认证（格式 user:pass），通过后与目标建立连接并双向转发。
func startConnectProxy(t *testing.T, wantAuth string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleHTTPConnect(conn, wantAuth)
		}
	}()
	return ln.Addr().String()
}

func handleHTTPConnect(conn net.Conn, wantAuth string) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		writeProxyStatus(conn, "405 Method Not Allowed")
		return
	}
	if wantAuth != "" {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(wantAuth))
		if req.Header.Get("Proxy-Authorization") != want {
			writeProxyStatus(conn, "407 Proxy Authentication Required")
			return
		}
	}
	target, err := net.DialTimeout("tcp", req.Host, 5*time.Second)
	if err != nil {
		writeProxyStatus(conn, "502 Bad Gateway")
		return
	}
	defer func() { _ = target.Close() }()
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	go func() { _, _ = io.Copy(target, br) }()
	_, _ = io.Copy(conn, target)
}

func writeProxyStatus(conn net.Conn, status string) {
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Length: 0\r\n\r\n", status)
}

// startSOCKS5Proxy 启动一个最小 SOCKS5 代理。wantUser 非空时只接受用户名/
// 密码认证方式（RFC 1929）并校验凭据，然后连接目标并双向转发。
func startSOCKS5Proxy(t *testing.T, wantUser, wantPass string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5(conn, wantUser, wantPass)
		}
	}()
	return ln.Addr().String()
}

func handleSOCKS5(conn net.Conn, wantUser, wantPass string) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)

	// 方法协商：VER NMETHODS METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	method := byte(0x00)
	if wantUser != "" {
		method = 0x02
	}
	offered := false
	for _, m := range methods {
		if m == method {
			offered = true
			break
		}
	}
	if !offered {
		_, _ = conn.Write([]byte{0x05, 0xFF})
		return
	}
	if _, err := conn.Write([]byte{0x05, method}); err != nil {
		return
	}

	if method == 0x02 {
		// 认证子协商：VER ULEN UNAME PLEN PASSWD
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 0x01 {
			return
		}
		uname := make([]byte, int(hdr[1]))
		if _, err := io.ReadFull(br, uname); err != nil {
			return
		}
		if _, err := io.ReadFull(br, hdr[:1]); err != nil {
			return
		}
		passwd := make([]byte, int(hdr[0]))
		if _, err := io.ReadFull(br, passwd); err != nil {
			return
		}
		status := byte(0x00)
		if string(uname) != wantUser || string(passwd) != wantPass {
			status = 0x01
		}
		if _, err := conn.Write([]byte{0x01, status}); err != nil || status != 0 {
			return
		}
	}

	// 连接请求：VER CMD RSV ATYP DST.ADDR DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[0] != 0x05 || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	case 0x03:
		if _, err := io.ReadFull(br, req[:1]); err != nil {
			return
		}
		buf := make([]byte, int(req[0]))
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		host = string(buf)
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		host = net.IP(buf).String()
	default:
		return
	}
	var port uint16
	if err := binary.Read(br, binary.BigEndian, &port); err != nil {
		return
	}

	target, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 5*time.Second)
	if err != nil {
		_, _ = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = target.Close() }()
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(target, br) }()
	_, _ = io.Copy(conn, target)
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func checkTunnel(t *testing.T, conn net.Conn, wantBanner string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	if wantBanner != "" {
		banner, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读取 banner 失败: %v", err)
		}
		if banner != wantBanner {
			t.Fatalf("banner 不符：got %q want %q", banner, wantBanner)
		}
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读取回显失败: %v", err)
	}
	if echo != "ping\n" {
		t.Fatalf("回显不符：got %q", echo)
	}
}

func TestDialTransportDirect(t *testing.T) {
	targetAddr := startEchoTarget(t, "")
	conn, err := dialTransport(config.Config{}, targetAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialTransport: %v", err)
	}
	defer func() { _ = conn.Close() }()
	checkTunnel(t, conn, "")
}

func TestDialViaHTTPConnectWithAuth(t *testing.T) {
	targetAddr := startEchoTarget(t, "SSH-2.0-testserver\r\n")
	host, port := splitAddr(t, startConnectProxy(t, "user:pass"))

	cfg := config.Config{
		ProxyMode:     config.ProxyModeHTTP,
		ProxyHost:     host,
		ProxyPort:     port,
		ProxyUsername: "user",
		ProxyPassword: "pass",
	}
	conn, err := dialTransport(cfg, targetAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialTransport: %v", err)
	}
	defer func() { _ = conn.Close() }()
	checkTunnel(t, conn, "SSH-2.0-testserver\r\n")
}

func TestDialViaHTTPConnectAuthRejected(t *testing.T) {
	targetAddr := startEchoTarget(t, "")
	host, port := splitAddr(t, startConnectProxy(t, "user:pass"))

	cfg := config.Config{
		ProxyMode:     config.ProxyModeHTTP,
		ProxyHost:     host,
		ProxyPort:     port,
		ProxyUsername: "user",
		ProxyPassword: "wrong",
	}
	_, err := dialTransport(cfg, targetAddr, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("期望 407 拒绝错误，got %v", err)
	}
}

func TestDialViaSOCKS5WithAuth(t *testing.T) {
	targetAddr := startEchoTarget(t, "SSH-2.0-socks\r\n")
	host, port := splitAddr(t, startSOCKS5Proxy(t, "u", "p"))

	cfg := config.Config{
		ProxyMode:     config.ProxyModeSOCKS5,
		ProxyHost:     host,
		ProxyPort:     port,
		ProxyUsername: "u",
		ProxyPassword: "p",
	}
	conn, err := dialTransport(cfg, targetAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialTransport: %v", err)
	}
	defer func() { _ = conn.Close() }()
	checkTunnel(t, conn, "SSH-2.0-socks\r\n")
}

func TestDialViaSOCKS5NoAuth(t *testing.T) {
	targetAddr := startEchoTarget(t, "")
	host, port := splitAddr(t, startSOCKS5Proxy(t, "", ""))

	cfg := config.Config{
		ProxyMode: config.ProxyModeSOCKS5,
		ProxyHost: host,
		ProxyPort: port,
	}
	conn, err := dialTransport(cfg, targetAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialTransport: %v", err)
	}
	defer func() { _ = conn.Close() }()
	checkTunnel(t, conn, "")
}

func TestDialTransportRejectsUnknownProxyMode(t *testing.T) {
	cfg := config.Config{ProxyMode: "ftp"}
	if _, err := dialTransport(cfg, "127.0.0.1:22", time.Second); err == nil {
		t.Fatal("期望未知代理类型报错")
	}
}
