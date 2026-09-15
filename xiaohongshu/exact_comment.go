package xiaohongshu

import (
	"errors"
	"github.com/go-rod/rod"
)

var errCommentUnbound = errors.New("comment_unbound")

func exactCommentBinding(id, author, expectedID, expectedAuthor string) bool {
	return expectedID != "" && id == expectedID && (expectedAuthor == "" || author == expectedAuthor)
}

// Read only the selected comment's own author marker. Nested replies cannot
// satisfy an author constraint, and missing/ambiguous markup fails closed.
func verifyExactComment(el *rod.Element, id, author string) error {
	result, err := el.Eval(`function() {
  if (!this.isConnected || !this.id.startsWith('comment-')) return null;
  if (Array.from(document.querySelectorAll('[id]')).filter(e => e.id === this.id).length !== 1) return null;
  const markers = Array.from(this.querySelectorAll('.author [data-user-id], .author[data-user-id]'))
    .filter(e => e.closest('[id^="comment-"]') === this);
  return {id:this.id.slice(8), author:markers.length === 1 ? markers[0].getAttribute('data-user-id') : ''};
 }`)
	if err != nil || result.Value.Nil() {
		return errCommentUnbound
	}
	if !exactCommentBinding(result.Value.Get("id").Str(), result.Value.Get("author").Str(), id, author) {
		return errCommentUnbound
	}
	return nil
}
