package apikeys

import "context"

type identityKey struct{}

// WithIdentity stores the authenticated caller for handlers downstream of the
// auth interceptor.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the authenticated caller, or nil outside an
// authenticated RPC (exempt methods, tests).
func IdentityFrom(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityKey{}).(*Identity)
	return id
}

type restrictedKey struct{}

// WithRestricted marks the calls made for a restricted request: from the
// x-ktms-data-classification header, or the proxy's own judge calls for one.
func WithRestricted(ctx context.Context) context.Context {
	return context.WithValue(ctx, restrictedKey{}, true)
}

// RestrictedFrom reports whether ctx carries WithRestricted.
func RestrictedFrom(ctx context.Context) bool {
	v, _ := ctx.Value(restrictedKey{}).(bool)
	return v
}
