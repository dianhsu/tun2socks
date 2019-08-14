package proxy

import (
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/common/dns"
	"github.com/xjasonlyu/tun2socks/common/log"
	"github.com/xjasonlyu/tun2socks/common/lsof"
	"github.com/xjasonlyu/tun2socks/common/pool"
	"github.com/xjasonlyu/tun2socks/common/stats"
	"github.com/xjasonlyu/tun2socks/core"
	"github.com/xjasonlyu/tun2socks/proxy/socks"
)

type tcpHandler struct {
	proxyHost string
	proxyPort int

	fakeDns       dns.FakeDns
	sessionStater stats.SessionStater
}

func NewTCPHandler(proxyHost string, proxyPort int, fakeDns dns.FakeDns, sessionStater stats.SessionStater) core.TCPConnHandler {
	return &tcpHandler{
		proxyHost:     proxyHost,
		proxyPort:     proxyPort,
		fakeDns:       fakeDns,
		sessionStater: sessionStater,
	}
}

func (h *tcpHandler) relay(localConn, remoteConn net.Conn) {
	var once sync.Once
	closeOnce := func() {
		once.Do(func() {
			localConn.Close()
			remoteConn.Close()
		})
	}

	// Close
	defer closeOnce()

	// WaitGroup
	var wg sync.WaitGroup
	wg.Add(1)

	// Up Link
	go func() {
		buf := pool.BufPool.Get().([]byte)
		defer pool.BufPool.Put(buf[:cap(buf)])
		if _, err := io.CopyBuffer(remoteConn, localConn, buf); err != nil {
			closeOnce()
		} else {
			localConn.SetDeadline(time.Now())
			remoteConn.SetDeadline(time.Now())
			tcpCloseRead(remoteConn)
		}
		wg.Done()
	}()

	// Down Link
	buf := pool.BufPool.Get().([]byte)
	if _, err := io.CopyBuffer(localConn, remoteConn, buf); err != nil {
		closeOnce()
	} else {
		localConn.SetDeadline(time.Now())
		remoteConn.SetDeadline(time.Now())
		tcpCloseRead(localConn)
	}
	pool.BufPool.Put(buf[:cap(buf)])

	wg.Wait() // Wait for Up Link done

	// Remove session
	if h.sessionStater != nil {
		h.sessionStater.RemoveSession(localConn)
	}
}

func (h *tcpHandler) Handle(conn net.Conn, target *net.TCPAddr) error {
	// Alias
	var localConn = conn

	// Replace with a domain name if target address IP is a fake IP.
	var targetHost = target.IP.String()
	if h.fakeDns != nil {
		if host, exist := h.fakeDns.IPToHost(target.IP); exist {
			targetHost = host
		}
	}

	proxyAddr := net.JoinHostPort(h.proxyHost, strconv.Itoa(h.proxyPort))
	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(target.Port))
	// Dial
	remoteConn, err := socks.Dial(proxyAddr, targetAddr)
	if err != nil {
		log.Warnf("Dial %v error: %v", proxyAddr, err)
		return err
	}

	// Get name of the process.
	var process = lsof.GetProcessName(localConn.LocalAddr())
	if h.sessionStater != nil {
		sess := &stats.Session{
			ProcessName:   process,
			Network:       localConn.LocalAddr().Network(),
			DialerAddr:    remoteConn.LocalAddr().String(),
			ClientAddr:    localConn.LocalAddr().String(),
			TargetAddr:    targetAddr,
			UploadBytes:   0,
			DownloadBytes: 0,
			SessionStart:  time.Now(),
		}
		h.sessionStater.AddSession(localConn, sess)

		remoteConn = stats.NewSessionConn(remoteConn, sess)
	}

	// Set keepalive
	tcpKeepAlive(localConn)
	tcpKeepAlive(remoteConn)

	// Relay connections
	go h.relay(localConn, remoteConn)

	log.Access(process, "proxy", "tcp", localConn.LocalAddr().String(), targetAddr)
	return nil
}