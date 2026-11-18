package cred

import (
	"context"
	"fmt"
	"time"
)

// RefreshWindow: a credential with less than this left is refreshed.
const RefreshWindow = 48 * time.Hour

// Rotation is what one refresh run did.
type Rotation struct {
	Refreshed bool      `json:"refreshed"`
	Version   int       `json:"version"` // the version now in use
	Expires   time.Time `json:"expires"`
	Disabled  []int     `json:"disabled,omitempty"`
	Destroyed []int     `json:"destroyed,omitempty"`
}

// Rotate refreshes a credential when it has less than RefreshWindow left (or
// always, with force): it adds the new version, disables the one it replaced,
// and destroys any older one, so at most two versions are ever kept (the
// live one, and the one before it, disabled). Only the refresher job and
// the owner run this; the server never refreshes, so writers never race.
//
// If the refresh call fails, nothing is written and the current version is
// still in use. If the refresh succeeded but Add fails, the new token is lost
// and the provider may already have retired the old refresh token: the error
// says to re-enroll.
func Rotate(ctx context.Context, v Vault, p Provider, now time.Time, force bool) (*Rotation, error) {
	name, err := SecretName(p.ID())
	if err != nil {
		return nil, err
	}
	cur, err := v.Latest(ctx, name)
	if err != nil {
		return nil, err
	}
	exp, ok := p.Expires(cur.Data)
	if !ok {
		return nil, ErrNotRefreshable
	}
	if !force && exp.Sub(now) >= RefreshWindow {
		return &Rotation{Version: cur.N, Expires: exp}, nil
	}
	next, err := p.Refresh(ctx, cur.Data)
	if err != nil {
		return nil, fmt.Errorf("refreshing %s (version %d still in use): %w", name, cur.N, err)
	}
	var n int
	for attempt := 0; attempt < 3; attempt++ {
		if n, err = v.Add(ctx, name, next); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("refreshed %s but couldn't store it (%w); the old refresh token may no longer work: re-enroll with `ob cred enroll %s`", name, err, p.ID())
	}
	newExp, _ := p.Expires(next)
	r := &Rotation{Refreshed: true, Version: n, Expires: newExp}

	vs, err := v.List(ctx, name)
	if err != nil {
		return r, fmt.Errorf("stored version %d, but couldn't list older versions to retire: %w", n, err)
	}
	for _, ver := range vs {
		switch {
		case ver.N == n || ver.State == "DESTROYED":
		case ver.N == cur.N:
			if ver.State == "ENABLED" {
				if err := v.Disable(ctx, name, ver.N); err != nil {
					return r, fmt.Errorf("stored version %d, but couldn't disable version %d: %w", n, ver.N, err)
				}
				r.Disabled = append(r.Disabled, ver.N)
			}
		default:
			if err := v.Destroy(ctx, name, ver.N); err != nil {
				return r, fmt.Errorf("stored version %d, but couldn't destroy version %d: %w", n, ver.N, err)
			}
			r.Destroyed = append(r.Destroyed, ver.N)
		}
	}
	return r, nil
}

// Store writes a freshly enrolled credential as a new version and disables
// every older enabled one, so the server reads the new one. Older versions
// are destroyed except the one just replaced, as in Rotate.
func Store(ctx context.Context, v Vault, p Provider, s Secret) (int, error) {
	name, err := SecretName(p.ID())
	if err != nil {
		return 0, err
	}
	n, err := v.Add(ctx, name, s)
	if err != nil {
		return 0, err
	}
	vs, err := v.List(ctx, name)
	if err != nil {
		return n, err
	}
	prev := 0
	for _, ver := range vs {
		if ver.N != n && ver.State != "DESTROYED" && ver.N > prev {
			prev = ver.N
		}
	}
	for _, ver := range vs {
		switch {
		case ver.N == n || ver.State == "DESTROYED":
		case ver.N == prev:
			if ver.State == "ENABLED" {
				if err := v.Disable(ctx, name, ver.N); err != nil {
					return n, err
				}
			}
		default:
			if err := v.Destroy(ctx, name, ver.N); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}
