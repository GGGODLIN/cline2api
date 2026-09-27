package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// ============================================================================
// zen 出口代理池
// 配置 http/https/socks5(h) 代理后，所有发往 opencode.ai 的请求经代理池轮询出去；
// 命中限流时冷却当前出口（Retry-After 优先），冷却期内轮询自动跳过。
// HTTPS 统一走 uTLS Chrome_120 指纹 + HTTP/2，避免 Go 原生 TLS 指纹被 Cloudflare 风控。
// ============================================================================

var (
	zenHTTPClient  = &http.Client{Transport: buildZenTransport()}
	zenProxyCount  atomic.Uint64
	zenTransportMu sync.Mutex

	zenProxyCooldowns   = map[int]time.Time{} // 代理索引 → 冷却截止
	zenProxyCooldownsMu sync.Mutex
)

func getZenHTTPClient() *http.Client {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	return zenHTTPClient
}

// rebuildZenTransport 代理或配置变化时重建 HTTP 客户端。
func rebuildZenTransport() {
	zenTransportMu.Lock()
	defer zenTransportMu.Unlock()
	zenHTTPClient = &http.Client{Transport: buildZenTransport()}
}

func buildZenTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	t.DialContext = zenDialContext
	// https 走 uTLS Chrome 指纹 + HTTP/2（完整浏览器指纹含 h2）
	t.RegisterProtocol("https", zenHTTP2Transport())
	return t
}

// zenTLSHandshakeTimeout 限制 TLS 握手时长：GFW 式黑洞（TCP 通、TLS 静默丢弃）
// 会让无超时的握手永久挂起，请求既不失败也不返回（故障转移也无法激活）。
const zenTLSHandshakeTimeout = 15 * time.Second

func zenHTTP2Transport() *http2.Transport {
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			raw, err := zenDialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				raw.Close()
				return nil, err
			}
			uconn := utls.UClient(raw, &utls.Config{
				ServerName: host,
				NextProtos: []string{"h2", "http/1.1"},
			}, utls.HelloChrome_120)
			// 握手限时；握手成功后 cancel 不影响已建立的连接
			hsCtx, cancel := context.WithTimeout(ctx, zenTLSHandshakeTimeout)
			defer cancel()
			if err := uconn.HandshakeContext(hsCtx); err != nil {
				raw.Close()
				return nil, err
			}
			return uconn, nil
		},
	}
}

func zenDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	p, _ := pickZenProxy()
	if p == "" {
		// 未配置 zen 代理时回退系统代理（HTTPS_PROXY / HTTP_PROXY），再直连
		if u, envErr := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}}); envErr == nil && u != nil {
			return dialViaProxy(ctx, u.String(), network, addr)
		}
		d := &net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	}
	return dialViaProxy(ctx, p, network, addr)
}

// pickZenProxy 按策略选代理，返回 (代理URL, 索引)；未配置返回 ("", -1)。
// 冷却中的代理线性探测跳过；轮询计数与日志索引保持一致。
func pickZenProxy() (string, int) {
	cfg := getZenConfig()
	n := len(cfg.Proxies)
	if n == 0 {
		return "", -1
	}
	idx := int(zenProxyCount.Add(1)-1) % n
	switch cfg.ProxyStrategy {
	case "random":
		idx = randIntn(n)
	case "fill":
		idx = 0
	}
	for i := 0; i < n; i++ {
		if zenProxyAvailable(idx) {
			break
		}
		idx = (idx + 1) % n
	}
	return cfg.Proxies[idx], idx
}

// lastZenProxyIdx 最近一次实际使用的代理索引（日志/冷却定位用）。
func lastZenProxyIdx() int {
	v := int64(zenProxyCount.Load())
	if v <= 0 {
		return -1
	}
	n := len(getZenConfig().Proxies)
	if n == 0 {
		n = 1
	}
	return int((v - 1) % int64(n))
}

func cooldownZenProxy(idx int, d time.Duration) {
	if idx < 0 {
		return
	}
	if d <= 0 {
		d = 10 * time.Minute
	}
	zenProxyCooldownsMu.Lock()
	zenProxyCooldowns[idx] = time.Now().Add(d)
	zenProxyCooldownsMu.Unlock()
	log.Printf("  zen proxy[%d] cooled down for %v", idx+1, d)
}

func zenProxyAvailable(idx int) bool {
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	until, ok := zenProxyCooldowns[idx]
	if !ok {
		return true
	}
	if time.Now().After(until) {
		delete(zenProxyCooldowns, idx)
		return true
	}
	return false
}

// zenProxyCooldownStatus 供管理后台展示：代理 URL → 冷却截止时刻。
func zenProxyCooldownStatus() map[string]string {
	cfg := getZenConfig()
	zenProxyCooldownsMu.Lock()
	defer zenProxyCooldownsMu.Unlock()
	out := map[string]string{}
	for idx, until := range zenProxyCooldowns {
		if idx >= 0 && idx < len(cfg.Proxies) && time.Now().Before(until) {
			out[cfg.Proxies[idx]] = until.Format("15:04:05")
		}
	}
	return out
}

func maskProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

// dialViaProxy 统一拨号入口：http/https 走 CONNECT 隧道，socks5(h) 走 SOCKS5 握手。
func dialViaProxy(ctx context.Context, raw, network, addr string) (net.Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad proxy url: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
		return dialHTTPProxy(ctx, u, network, addr)
	case "socks5", "socks5h":
		auth := &proxy.Auth{}
		if u.User != nil {
			auth.User = u.User.Username()
			auth.Password, _ = u.User.Password()
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		type ctxDialer interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}
		if cd, ok := d.(ctxDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		// 旧接口无 ctx：包装转换
		type result struct {
			c   net.Conn
			err error
		}
		ch := make(chan result, 1)
		go func() {
			c, err := d.Dial(network, addr)
			ch <- result{c, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.c, r.err
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

// dialHTTPProxy 经 http(s) 代理建立 CONNECT 隧道。
func dialHTTPProxy(ctx context.Context, u *url.URL, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second}
	rawConn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "https" {
		tlsConn := tls.Client(rawConn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()})
		hsCtx, hsCancel := context.WithTimeout(ctx, zenTLSHandshakeTimeout)
		defer hsCancel()
		if err := tlsConn.HandshakeContext(hsCtx); err != nil {
			rawConn.Close()
			return nil, err
		}
		rawConn = tlsConn
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u.User != nil {
		cred := base64.StdEncoding.EncodeToString([]byte(u.User.String()))
		req.Header.Set("Proxy-Authorization", "Basic "+cred)
	}
	if err := req.Write(rawConn); err != nil {
		rawConn.Close()
		return nil, err
	}

	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		rawConn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawConn.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("proxy CONNECT %s: %s %s", u.Host, resp.Status, strings.TrimSpace(string(b)))
	}
	return rawConn, nil
}
