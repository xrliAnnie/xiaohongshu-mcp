package xiaohongshu

import (
	"testing"
)

func TestTopicSelectionRequiresOneExactLabel(t *testing.T) {
	if index, err := exactTopicIndex([]string{"other", "#approved", "approved extra"}, "approved"); err != nil || index != 1 {
		t.Fatal("did not select exact second topic", index, err)
	}
	for _, labels := range [][]string{{"approved extra"}, {"#approved", "approved"}, {}, {"Approved"}} {
		if _, err := exactTopicIndex(labels, "approved"); err == nil {
			t.Fatal("accepted ambiguous or inexact topic")
		}
	}
	if _, err := exactTopicIndex([]string{"#approved"}, " approved "); err == nil {
		t.Fatal("rewrote whitespace")
	}
	if index, err := exactTopicIndex([]string{"##approved"}, "#approved"); err != nil || index != 0 {
		t.Fatal("rewrote literal leading hash")
	}
}
