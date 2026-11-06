package xiaohongshu

import "testing"

func TestExactCommentBinding(t *testing.T) {
	for _, test := range []struct {
		id, author, expectedID, expectedAuthor string
		ok                                     bool
	}{
		{"comment-a", "user-a", "comment-a", "user-a", true},
		{"comment-a", "user-a", "comment-a", "", true},
		{"comment-b", "user-a", "comment-a", "user-a", false},
		{"comment-a", "user-b", "comment-a", "user-a", false},
		{"comment-a", "", "comment-a", "user-a", false},
		{"", "user-a", "", "user-a", false},
	} {
		if exactCommentBinding(test.id, test.author, test.expectedID, test.expectedAuthor) != test.ok {
			t.Fatalf("wrong binding: %+v", test)
		}
	}
}
