package nodeagent

import (
	"context"
	"net"
	"net/http"
	"os"
)

type peerUIDKey struct{}

// withPeerUID records the uid of the process on the other end of the socket.
func withPeerUID(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	uid, err := peerUID(uc)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, peerUIDKey{}, uid)
}

// ownerOnly refuses callers running as another user. The socket's 0600 mode
// already does; this holds even if the mode is loosened by hand.
func ownerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := r.Context().Value(peerUIDKey{}).(int)
		if peerCredSupported && (!ok || uid != os.Getuid()) {
			writeJSON(w, http.StatusForbidden, ControlError{Error: "only the user the agent runs as may control it"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
