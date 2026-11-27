package tools

import (
	"context"
	"fmt"

	"github.com/daybeam/vortex/schemas"
)

// OwnerIsolationEnabled reports whether empty ownership is a local compatibility
// value rather than a shared-access fallback.
func OwnerIsolationEnabled(app *App) bool {
	return app != nil && (app.OwnerIsolation || app.Registry != nil && len(app.Registry.System.APIKeys) > 0)
}

// CheckResourceOwnership authorizes durable resources using the stable owner,
// not the per-connection task identity.
func CheckResourceOwnership(ctx context.Context, app *App, owner string) bool {
	if EffectiveTier(ctx, app) == TierAdmin {
		return true
	}
	if owner == "" {
		return !OwnerIsolationEnabled(app)
	}
	return owner == CallerOwnerID(ctx)
}

// RequireResourceOwner resolves the server-side owner for a new resource.
func RequireResourceOwner(ctx context.Context, app *App) (string, error) {
	owner := CallerOwnerID(ctx)
	if owner == "" && EffectiveTier(ctx, app) != TierAdmin && OwnerIsolationEnabled(app) {
		return "", fmt.Errorf("owner identity is required in multi-user mode")
	}
	return owner, nil
}

func ResourceScopeIsUnrestricted(ctx context.Context, app *App) bool {
	return EffectiveTier(ctx, app) == TierAdmin || !OwnerIsolationEnabled(app)
}

func GetVisibleSchedule(ctx context.Context, app *App, id string) *schemas.Schedule {
	if app == nil || app.SStore == nil {
		return nil
	}
	return app.SStore.GetVisible(id, CallerOwnerID(ctx), ResourceScopeIsUnrestricted(ctx, app))
}
