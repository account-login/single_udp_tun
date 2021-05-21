package single_udp_tun

import (
	"context"
	"github.com/account-login/ctxlog"
	"net"
	"net/http"
	_ "net/http/pprof"
)

func UDPAddrEqual(lhs, rhs *net.UDPAddr) bool {
	return lhs.IP.Equal(rhs.IP) && lhs.Zone == rhs.Zone && lhs.Port == rhs.Port
}

func assert(condition bool) {
	if !condition {
		panic("assertion failure")
	}
}

func StartDebugServer(ctx context.Context, addr string) (server *http.Server) {
	if addr == "" {
		addr = ":http"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		ctxlog.Errorf(ctx, "StartDebugServer: %v", err)
		return nil
	}
	ctxlog.Debugf(ctx, "StartDebugServer: addr: %v", ln.Addr())

	server = &http.Server{Addr: addr, Handler: nil}
	go func() {
		err := server.Serve(ln)
		if err != nil {
			ctxlog.Errorf(ctx, "StartDebugServer: %v", err)
		}
	}()
	return
}
