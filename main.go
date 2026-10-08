// Command gpg-agent-proxy 把 Windows 版 GnuPG 的 gpg-agent 端點轉成一般 TCP
// 端點，讓 OpenSSH 的 RemoteForward 可以把它轉送到遠端主機上使用。
//
// 背景：Windows 版 GnuPG 2.5 的 gpg-agent 並不是用檔案 socket，而是監聽
// 127.0.0.1 上的一個隨機 TCP 埠，並且要求每個連線先送出 16 位元組的 nonce
// 才會回應。埠號與 nonce 記在
//
//	%LOCALAPPDATA%\gnupg\S.gpg-agent.extra
//
// （第一行是埠號，接著是 16 位元組的 raw nonce）。因為沒有可轉送的檔案
// socket，ssh 的 RemoteForward 無法直接指向它；本程式改為監聽一個一般 TCP
// 埠，對每個進來的連線自行連上真正的 agent、補送 nonce，再雙向搬運資料。
//
// 主控台輸出全部使用英文，方便與 ssh 的訊息並排閱讀。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// defaultListenAddr 是預設監聽位置，需與 ssh config 的 RemoteForward 目標一致。
	defaultListenAddr = "127.0.0.1:16688"
	nonceSize         = 16
	dialTimeout       = 10 * time.Second
	copyBufferSize    = 32 * 1024
	shutdownGrace     = 2 * time.Second
)

// endpoint 是一次連線所需的 agent 資訊。
type endpoint struct {
	port  int
	nonce []byte
}

// readEndpoint 從 socketFile 讀出埠號與 nonce。
// 刻意每次連線都重新讀取：gpg-agent 重啟後埠號與 nonce 都會改變，快取會失效。
func readEndpoint(socketFile string) (endpoint, error) {
	raw, err := os.ReadFile(socketFile)
	if err != nil {
		return endpoint{}, err
	}
	nl := bytes.IndexByte(raw, '\n')
	if nl <= 0 {
		return endpoint{}, fmt.Errorf("unexpected content in %s: %q", socketFile, clip(raw, 32))
	}
	port, err := strconv.Atoi(string(bytes.TrimSpace(raw[:nl])))
	if err != nil {
		return endpoint{}, fmt.Errorf("bad port in %s: %w", socketFile, err)
	}
	if len(raw) < nl+1+nonceSize {
		return endpoint{}, fmt.Errorf("nonce too short in %s (%d bytes)", socketFile, len(raw)-nl-1)
	}
	nonce := make([]byte, nonceSize)
	copy(nonce, raw[nl+1:nl+1+nonceSize])
	return endpoint{port: port, nonce: nonce}, nil
}

// clip 把過長的內容截短，只用於錯誤訊息。
func clip(raw []byte, n int) string {
	if len(raw) <= n {
		return string(raw)
	}
	return string(raw[:n]) + "..."
}

// proxy 負責監聽、轉送與關閉所有連線。
type proxy struct {
	socketFile string
	lg         *log.Logger

	stopping atomic.Bool
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	handlers sync.WaitGroup
}

// track 登錄或移除一條需要跟著程式一起關閉的連線。
func (p *proxy) track(c net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.conns[c] = struct{}{}
		return
	}
	delete(p.conns, c)
}

// closeTracked 關掉目前所有在用的連線，讓正在搬資料的 goroutine 立刻結束。
func (p *proxy) closeTracked() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// serve 持續接受連線，直到 listener 被關閉。
func (p *proxy) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !p.stopping.Load() {
				p.lg.Printf("accept failed: %v", err)
			}
			return
		}
		if p.stopping.Load() { // Ctrl+C 與新連線同時發生
			_ = conn.Close()
			return
		}
		p.handlers.Add(1)
		go p.handle(conn)
	}
}

// handle 處理一條來自 ssh 的連線：連上真正的 agent、補送 nonce、雙向搬運資料。
func (p *proxy) handle(client net.Conn) {
	defer p.handlers.Done()
	p.track(client, true)
	defer func() {
		p.track(client, false)
		_ = client.Close()
	}()

	ep, err := readEndpoint(p.socketFile)
	if err != nil {
		p.lg.Printf("error: %v", err)
		return
	}

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ep.port)), dialTimeout)
	if err != nil {
		p.lg.Printf("error: cannot reach gpg-agent on 127.0.0.1:%d: %v", ep.port, err)
		return
	}
	p.track(upstream, true)
	defer func() {
		p.track(upstream, false)
		_ = upstream.Close()
	}()

	if _, err := upstream.Write(ep.nonce); err != nil {
		p.lg.Printf("error: cannot send nonce to gpg-agent: %v", err)
		return
	}
	p.lg.Printf("connect -> 127.0.0.1:%d", ep.port)

	var upBytes, downBytes int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // client -> agent
		defer wg.Done()
		n, _ := io.CopyBuffer(upstream, client, make([]byte, copyBufferSize))
		atomic.AddInt64(&upBytes, n)
		closeWrite(upstream)
	}()
	go func() { // agent -> client
		defer wg.Done()
		n, _ := io.CopyBuffer(client, upstream, make([]byte, copyBufferSize))
		atomic.AddInt64(&downBytes, n)
		closeWrite(client)
	}()
	wg.Wait()
	p.lg.Printf("closed  up=%dB down=%dB", atomic.LoadInt64(&upBytes), atomic.LoadInt64(&downBytes))
}

// closeWrite 對 TCP 連線做 half-close，讓對端收到 EOF 而不是一直等待。
func closeWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}

// waitHandlers 等待所有連線收尾，最多等 shutdownGrace，避免關不掉。
func (p *proxy) waitHandlers() {
	done := make(chan struct{})
	go func() {
		p.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		p.lg.Printf("warning: some connections did not finish in time")
	}
}

// newLogger 建立輸出到主控台（必要時同時寫入檔案）的 logger，格式為「時:分:秒 訊息」。
func newLogger(path string) (*log.Logger, func(), error) {
	var w io.Writer = os.Stdout
	closer := func() {}
	if path != "" {
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, err
		}
		w = io.MultiWriter(os.Stdout, fh)
		closer = func() { _ = fh.Close() }
	}
	return log.New(w, "", log.Ltime), closer, nil
}

// defaultSocketFile 回傳 Windows 上 gpg-agent「extra」socket 資訊檔的預設位置。
// extra 是 GnuPG 專門給轉送用的低權限端點，比主 socket 更適合對外。
func defaultSocketFile() string {
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "gnupg", "S.gpg-agent.extra")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "AppData", "Local", "gnupg", "S.gpg-agent.extra")
	}
	return filepath.Join("gnupg", "S.gpg-agent.extra")
}

// portOf 只取 "host:port" 的埠號部分，用於提示訊息。
func portOf(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return addr
}

// run 是主流程，回傳行程結束碼。
func run(listenAddr, socketFile, logPath string) int {
	lg, closeLog, err := newLogger(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open log file: %v\n", err)
		return 1
	}
	defer closeLog()

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		lg.Printf("cannot listen on %s: %v", listenAddr, err)
		lg.Printf("another instance already running? try: netstat -ano | findstr :%s", portOf(listenAddr))
		return 1
	}

	p := &proxy{socketFile: socketFile, lg: lg, conns: map[net.Conn]struct{}{}}

	lg.Printf("gpg-agent-proxy listening on %s", ln.Addr())
	lg.Printf("upstream agent endpoint file: %s", socketFile)
	if ep, err := readEndpoint(socketFile); err != nil {
		lg.Printf("warning: cannot read that file yet: %v", err)
		lg.Printf("         start the agent first (Kleopatra, or: gpg-connect-agent /bye)")
	} else {
		lg.Printf("gpg-agent endpoint: 127.0.0.1:%d (%d-byte nonce)", ep.port, len(ep.nonce))
	}
	lg.Printf("press Ctrl+C to stop")

	var stopOnce sync.Once
	stop := func(sig os.Signal) {
		stopOnce.Do(func() {
			if sig != nil {
				lg.Printf("signal %v received", sig)
			}
			p.stopping.Store(true)
			lg.Printf("shutting down ...")
			_ = ln.Close()
			p.closeTracked()
		})
	}

	sigCh := make(chan os.Signal, 1)
	// 在 Windows 上，Go 會把 Ctrl+C 與 Ctrl+Break 都對應成 os.Interrupt，
	// 關閉主控台視窗則對應成 SIGTERM，所以不需要平台專屬的訊號清單。
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		sig, ok := <-sigCh
		if !ok {
			return
		}
		stop(sig)
	}()

	p.serve(ln)
	stop(nil)
	p.waitHandlers()
	lg.Printf("stopped")
	return 0
}

// usage 印出用途與參數說明。
func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, `gpg-agent-proxy - forward the Windows gpg-agent over ssh

Windows GnuPG's gpg-agent has no filesystem socket: it listens on a random
127.0.0.1 TCP port and expects a 16-byte nonce before answering. This program
re-exposes it as a plain TCP endpoint so OpenSSH's RemoteForward can carry it
to a remote host, where gpg and git can then sign with the local YubiKey.

Typical ssh config on Windows:

    Host myhost
        HostName 203.0.113.10
        User yashi
        RemoteForward /run/user/1000/gnupg/S.gpg-agent 127.0.0.1:16688
        StreamLocalBindUnlink yes

Options:
`)
	flag.PrintDefaults()
}

func main() {
	listenAddr := flag.String("listen", defaultListenAddr, "local TCP address to listen on (the ssh RemoteForward target)")
	socketFile := flag.String("socket", defaultSocketFile(), "path of the gpg-agent endpoint file (S.gpg-agent.extra)")
	logPath := flag.String("log", "", "also append the console log to this file (default: console only)")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	os.Exit(run(*listenAddr, *socketFile, *logPath))
}
