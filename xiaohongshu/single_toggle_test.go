package xiaohongshu

import (
	"context"
	"errors"
	"testing"
)

func TestSingleToggleNeverRetriesUnknownOutcome(t *testing.T) {
	for _, kind := range []string{"unchanged", "read error", "click error"} {
		t.Run(kind, func(t *testing.T) {
			reads, clicks := 0, 0
			read := func() (bool, error) {
				reads++
				if reads > 1 && kind == "read error" {
					return false, errors.New("synthetic")
				}
				return false, nil
			}
			click := func() error {
				clicks++
				if kind == "click error" {
					return errors.New("synthetic")
				}
				return nil
			}
			if err := singleToggle(context.Background(), true, read, click, func(context.Context) error { return nil }); err == nil {
				t.Fatal("reported unknown as success")
			}
			if clicks != 1 {
				t.Fatal("retried", clicks)
			}
		})
	}
}
func TestSingleToggleRequiresKnownInitialStateAndHonorsNoop(t *testing.T) {
	for _, kind := range []string{"unknown", "already desired", "canceled"} {
		clicks := 0
		ctx, cancel := context.WithCancel(context.Background())
		if kind == "canceled" {
			cancel()
		}
		defer cancel()
		err := singleToggle(ctx, true, func() (bool, error) {
			if kind == "unknown" {
				return false, errors.New("missing")
			}
			return true, nil
		}, func() error { clicks++; return nil }, func(context.Context) error { return nil })
		if clicks != 0 {
			t.Fatal("clicked without needed known transition")
		}
		if (kind == "already desired") != (err == nil) {
			t.Fatal("wrong result", kind, err)
		}
	}
}
func TestSingleToggleConfirmsOneTransition(t *testing.T) {
	reads, clicks := 0, 0
	err := singleToggle(context.Background(), false, func() (bool, error) { reads++; return reads == 1, nil }, func() error { clicks++; return nil }, func(context.Context) error { return nil })
	if err != nil || clicks != 1 || reads != 2 {
		t.Fatal("not one verified transition", err, clicks, reads)
	}
}
func TestInteractStateMissingBooleansNeverBecomeFalse(t *testing.T) {
	for _, raw := range []string{`{"feed":{"note":{"interactInfo":{}}}}`, `{"feed":{"note":{"interactInfo":{"liked":false}}}}`, `{"other":{"note":{"interactInfo":{"liked":false,"collected":false}}}}`} {
		if _, _, err := parseInteractState(raw, "feed"); err == nil {
			t.Fatal("accepted absent state")
		}
	}
	if liked, collected, err := parseInteractState(`{"feed":{"note":{"interactInfo":{"liked":false,"collected":true}}}}`, "feed"); err != nil || liked || !collected {
		t.Fatal("valid state failed")
	}
}
