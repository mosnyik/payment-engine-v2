package audit

import (
	"context"

	"github.com/google/uuid"
)

// Identity is a mutable box the audit middleware injects into the request
// context before calling downstream handlers. gateway.HMACMiddleware,
// adminauth.Middleware, and tenant.PortalMiddleware each fill in whichever
// field they authenticate, once they resolve it.
//
// A pointer is necessary, not just convenient: each of those middlewares
// derives its own child context via context.WithValue and calls
// next.ServeHTTP(w, r.WithContext(childCtx)) — that reassignment is local to
// their own stack frame, so a wrapping middleware's code that runs *after*
// next() returns never sees it. The audit middleware runs outermost and
// would otherwise have no way to learn who got authenticated further in.
// Handing every layer the same pointer sidesteps that: whichever middleware
// fills a field in, the audit middleware reads the same object back.
type Identity struct {
	TenantID *uuid.UUID
	APIKeyID *uuid.UUID
	AdminID  *uuid.UUID
}

type identityKey struct{}

// IdentityFromContext returns the Identity box for the current request, if
// the audit middleware is wired in front of this handler. Auth middlewares
// call this and set whichever field they just authenticated; a middleware
// stack without audit wired in front of it simply gets ok=false and skips
// attribution, so this is safe to call unconditionally.
func IdentityFromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(*Identity)
	return id, ok
}

func newIdentityContext(ctx context.Context) (context.Context, *Identity) {
	id := &Identity{}
	return context.WithValue(ctx, identityKey{}, id), id
}
