package main

import (
	"context"
	"testing"
)

func TestLegacyWriteServiceCannotBypassFounderGate(t *testing.T) {
	service := &XiaohongshuService{}
	ctx := context.Background()
	// The first legacy method dereferences this nil request before any browser call.
	// Before the gate exists it fails here without ever accessing an account.
	defer func() {
		if recover() != nil {
			t.Fatal("legacy service evaluated write input before requiring authorization")
		}
	}()
	checks := []func() error{
		func() error { _, err := service.PublishContent(ctx, nil); return err },
		func() error { _, err := service.PublishVideo(ctx, nil); return err },
		func() error {
			_, err := service.PostCommentToFeed(ctx, "synthetic-feed", "synthetic-token", "text")
			return err
		},
		func() error {
			_, err := service.ReplyCommentToFeed(ctx, "synthetic-feed", "synthetic-token", "comment", "user", "text")
			return err
		},
		func() error { _, err := service.LikeFeed(ctx, "synthetic-feed", "synthetic-token"); return err },
		func() error { _, err := service.UnlikeFeed(ctx, "synthetic-feed", "synthetic-token"); return err },
		func() error { _, err := service.FavoriteFeed(ctx, "synthetic-feed", "synthetic-token"); return err },
		func() error { _, err := service.UnfavoriteFeed(ctx, "synthetic-feed", "synthetic-token"); return err },
	}
	for i, check := range checks {
		err := check()
		if err == nil || err.Error() != "founder_write_gate_absent" {
			t.Fatalf("legacy writer %d did not require gate: %v", i, err)
		}
	}
}
