package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
)

// cloneTLSConfig 返回一份最小化的 TLS 配置副本（socks5 隧道上的 TLS 升级用）。
func cloneTLSConfig() *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	}
}

// tlsClient 在已建立的 TCP 连接上做 TLS 握手，等价于 tls.Client 但显式接收地址。
func tlsClient(conn net.Conn, addr string, cfg *tls.Config) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	cfgCopy := cfg.Clone()
	cfgCopy.ServerName = host
	return tls.Client(conn, cfgCopy), nil
}
