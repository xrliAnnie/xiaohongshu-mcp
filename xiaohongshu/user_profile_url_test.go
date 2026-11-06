package xiaohongshu

import (
	"net/url"
	"testing"
)

func TestUserProfileURLKeepsTokenAndUserInTheirOwnFields(t *testing.T) {
	for _, token := range []string{"ordinary-token", "token&xsec_source=other", "token#fragment", "token+plus/%?中文"} {
		for _, userID := range []string{"user-a", "user/with?query#fragment%"} {
			parsed, err := url.Parse(makeUserProfileURL(userID, token))
			if err != nil {
				t.Fatal(err)
			}
			query := parsed.Query()
			if parsed.Fragment != "" || len(query) != 2 || len(query["xsec_token"]) != 1 || query.Get("xsec_token") != token || query.Get("xsec_source") != "pc_note" {
				t.Fatal("profile token changed URL structure")
			}
			if parsed.EscapedPath() != "/user/profile/"+url.PathEscape(userID) {
				t.Fatal("user ID changed profile path structure")
			}
		}
	}
}
