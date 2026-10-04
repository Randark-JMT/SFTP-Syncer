package syncer

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
	xproxy "golang.org/x/net/proxy"

	"sftp-syncer/internal/config"
)

// dialSSH 建立一条到目标主机的 SSH 连接：按主机配置直连或经 HTTP/HTTPS/
// SOCKS5 代理拨号，然后在已连通的传输层连接上完成 SSH 握手。
func dialSSH(cfg config.Config, sshConfig *ssh.ClientConfig) (*ssh.Client, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	timeout := sshConfig.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	conn, err := dialTransport(cfg, addr, timeout)
	if err != nil {
		return nil, err
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConfig)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// dialTransport 返回一条通向 addr 的传输层连接：未启用代理时等价于
// net.DialTimeout；启用代理时完成代理握手后返回已建立的隧道连接。
func dialTransport(cfg config.Config, addr string, timeout time.Duration) (net.Conn, error) {
	switch cfg.ProxyMode {
	case "", config.ProxyModeNone:
		return net.DialTimeout("tcp", addr, timeout)
	case config.ProxyModeSOCKS5:
		return dialViaSOCKS5(cfg, addr, timeout)
	case config.ProxyModeHTTP, config.ProxyModeHTTPS:
		return dialViaHTTPConnect(cfg, addr, timeout)
	default:
		return nil, fmt.Errorf("不支持的代理类型：%s", cfg.ProxyMode)
	}
}

func proxyAddr(cfg config.Config) string {
	return net.JoinHostPort(cfg.ProxyHost, strconv.Itoa(cfg.ProxyPort))
}

// dialViaSOCKS5 通过 SOCKS5 代理拨号，可选用户名/密码认证（RFC 1929）。
// 握手整体受 timeout 约束，避免代理无响应时无限阻塞。
func dialViaSOCKS5(cfg config.Config, addr string, timeout time.Duration) (net.Conn, error) {
	var auth *xproxy.Auth
	if cfg.ProxyUsername != "" {
		auth = &xproxy.Auth{User: cfg.ProxyUsername, Password: cfg.ProxyPassword}
	}
	dialer, err := xproxy.SOCKS5("tcp", proxyAddr(cfg), auth, &net.Dialer{Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("配置 SOCKS5 代理失败: %w", err)
	}

	var conn net.Conn
	if ctxDialer, ok := dialer.(xproxy.ContextDialer); ok {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		conn, err = ctxDialer.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SOCKS5 代理连接失败: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// dialViaHTTPConnect 通过 HTTP CONNECT 建立隧道；代理类型为 https 时先与代理
// 服务器建立 TLS 连接再发起 CONNECT。可选 Basic 代理认证。
func dialViaHTTPConnect(cfg config.Config, addr string, timeout time.Duration) (net.Conn, error) {
	var conn net.Conn
	var err error
	if cfg.ProxyMode == config.ProxyModeHTTPS {
		raw, err := net.DialTimeout("tcp", proxyAddr(cfg), timeout)
		if err != nil {
			return nil, fmt.Errorf("连接 HTTPS 代理失败: %w", err)
		}
		tlsConn := tls.Client(raw, &tls.Config{ServerName: cfg.ProxyHost})
		_ = tlsConn.SetDeadline(time.Now().Add(timeout))
		if err = tlsConn.Handshake(); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("与 HTTPS 代理的 TLS 握手失败: %w", err)
		}
		conn = tlsConn
	} else {
		conn, err = net.DialTimeout("tcp", proxyAddr(cfg), timeout)
		if err != nil {
			return nil, fmt.Errorf("连接 HTTP 代理失败: %w", err)
		}
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := writeHTTPConnectRequest(conn, cfg, addr); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// ReadResponse 可能把隧道对端紧随 200 响应发来的数据（如 SSH banner）
	// 读入缓冲，因此后续读取必须经由该 bufio.Reader，避免数据丢失。
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取代理 CONNECT 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 请求被拒绝: %s", resp.Status)
	}
	// 注意不能关闭 resp.Body：对 CONNECT 的 2xx 响应其 Body 就是隧道本身，
	// Close 会一直读到对端断开。失败分支已直接关闭底层连接，无需处理。

	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

func writeHTTPConnectRequest(conn net.Conn, cfg config.Config, addr string) error {
	header := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if cfg.ProxyUsername != "" {
		credentials := base64.StdEncoding.EncodeToString([]byte(cfg.ProxyUsername + ":" + cfg.ProxyPassword))
		header += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	header += "\r\n"
	if _, err := conn.Write([]byte(header)); err != nil {
		return fmt.Errorf("发送代理 CONNECT 请求失败: %w", err)
	}
	return nil
}

// bufferedConn 让读取先经过 bufio.Reader（其中可能已缓冲了隧道对端的数据），
// 写入直连底层连接。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}
