package xiaohongshu

import (
	"context"
	"errors"
	"time"
)

var errInteractionState = errors.New("interaction_state_unverified")
var errInteractionUnknown = errors.New("interaction_outcome_unknown")

// Never infer false from unavailable state and never compensate for an unknown
// first click with another click: a delayed first result could invert the action.
func singleToggle(ctx context.Context, target bool, read func() (bool, error), click func() error, wait func(context.Context) error) error {
	if ctx.Err() != nil {
		return errInteractionState
	}
	before, err := read()
	if err != nil || ctx.Err() != nil {
		return errInteractionState
	}
	if before == target {
		return nil
	}
	if err = click(); err != nil {
		return errInteractionUnknown
	}
	if wait(ctx) != nil || ctx.Err() != nil {
		return errInteractionUnknown
	}
	after, err := read()
	if err != nil || after != target || ctx.Err() != nil {
		return errInteractionUnknown
	}
	return nil
}
func waitToggleState(ctx context.Context) error {
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
