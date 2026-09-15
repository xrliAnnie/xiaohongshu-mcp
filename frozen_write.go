package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

var errFrozenWrite = errors.New("invalid_write_input")

type frozenArtifact struct {
	ArtifactID string `json:"artifactId"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"sizeBytes"`
	MIMEType   string `json:"mimeType"`
}
type frozenAccount struct {
	ProviderInstanceID string `json:"providerInstanceId"`
	AccountUserID      string `json:"accountUserId"`
	AccountEpoch       int64  `json:"accountEpoch"`
	ProviderGeneration string `json:"providerGeneration"`
}
type frozenTarget struct {
	FeedID    string  `json:"feedId"`
	CommentID *string `json:"commentId"`
	UserID    *string `json:"userId"`
}
type frozenPayload struct {
	Title      *string  `json:"title"`
	Content    *string  `json:"content"`
	Tags       []string `json:"tags"`
	Visibility *string  `json:"visibility"`
	IsOriginal *bool    `json:"isOriginal"`
	ScheduleAt *string  `json:"scheduleAt"`
	Unlike     *bool    `json:"unlike"`
	Unfavorite *bool    `json:"unfavorite"`
}
type frozenUpstream struct {
	BinarySHA256     string `json:"binarySha256"`
	ToolSchemaDigest string `json:"toolSchemaDigest"`
	GuardProtocol    int64  `json:"guardProtocol"`
}
type frozenWrite struct {
	SchemaVersion          int64            `json:"schemaVersion"`
	Purpose                string           `json:"purpose"`
	ProposalID             string           `json:"proposalId"`
	RequesterUID           int64            `json:"requesterUid"`
	AuthorityPolicyVersion int64            `json:"authorityPolicyVersion"`
	ProjectID              string           `json:"projectId"`
	LeadID                 string           `json:"leadId"`
	OperationID            string           `json:"operationId"`
	Account                frozenAccount    `json:"account"`
	Target                 *frozenTarget    `json:"target"`
	Payload                frozenPayload    `json:"payload"`
	Media                  []frozenArtifact `json:"media"`
	Upstream               frozenUpstream   `json:"upstream"`
}

var frozenUUID = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)

func frozenString(s string, min, max int) bool {
	n := len(utf16.Encode([]rune(s)))
	return utf8.ValidString(s) && n >= min && n <= max
}
func frozenID(s string) bool                    { return frozenString(s, 1, 256) }
func nullableFrozenID(s *string) bool           { return s == nil || frozenID(*s) }
func safeFrozenInteger(n int64, min int64) bool { return n >= min && n <= maxPermitInteger }

// Wire must already be fully normalized by authority. Missing defaults, aliases,
// duplicates, unknown fields and noncanonical JSON cannot become a different write.
func verifyFrozenWrite(raw []byte, digest string, now int64) (frozenWrite, error) {
	var w frozenWrite
	if len(raw) > 1024*1024 || !utf8.Valid(raw) || !journalDigest.MatchString(digest) || !safeFrozenInteger(now, 0) {
		return w, errFrozenWrite
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&w) != nil {
		return frozenWrite{}, errFrozenWrite
	}
	encoded, err := canonicalFrozenWrite(w)
	if err != nil || !bytes.Equal(raw, encoded) {
		return frozenWrite{}, errFrozenWrite
	}
	sum := sha256.Sum256(append([]byte("flywheel:xhs-write:v1\n"), encoded...))
	if hex.EncodeToString(sum[:]) != digest || !validFrozenWrite(w, now) {
		return frozenWrite{}, errFrozenWrite
	}
	return w, nil
}
func validFrozenWrite(w frozenWrite, now int64) bool {
	a, p := w.Account, w.Payload
	if w.SchemaVersion != 1 || w.Purpose != "xiaohongshu_founder_write" || !frozenUUID.MatchString(w.ProposalID) || !safeFrozenInteger(w.RequesterUID, 0) || !safeFrozenInteger(w.AuthorityPolicyVersion, 1) || !frozenID(w.ProjectID) || !frozenID(w.LeadID) || !frozenID(a.ProviderInstanceID) || !frozenID(a.AccountUserID) || !frozenID(a.ProviderGeneration) || !safeFrozenInteger(a.AccountEpoch, 1) {
		return false
	}
	if w.Upstream.GuardProtocol != 1 || !journalDigest.MatchString(w.Upstream.BinarySHA256) || !journalDigest.MatchString(w.Upstream.ToolSchemaDigest) || w.Media == nil || len(w.Media) > 18 || p.Tags == nil || len(p.Tags) > 100 {
		return false
	}
	for _, tag := range p.Tags {
		if !frozenID(tag) {
			return false
		}
	}
	if p.Title != nil && !frozenString(*p.Title, 0, 40) || p.Content != nil && !frozenString(*p.Content, 1, 16000) {
		return false
	}
	for _, m := range w.Media {
		if !frozenID(m.ArtifactID) || !journalDigest.MatchString(m.SHA256) || m.SizeBytes < 1 || m.SizeBytes > 10*1024*1024 {
			return false
		}
		switch m.MIMEType {
		case "image/png", "image/jpeg", "image/webp", "video/mp4":
		default:
			return false
		}
	}
	op := strings.TrimPrefix(w.OperationID, "xiaohongshu.")
	if w.OperationID != "xiaohongshu."+op {
		return false
	}
	publish := op == "publish_content" || op == "publish_with_video"
	if publish {
		if w.Target != nil || p.Title == nil || p.Content == nil || len(w.Media) == 0 || p.Visibility == nil || p.IsOriginal == nil || p.Unlike != nil || p.Unfavorite != nil {
			return false
		}
		units := 0
		for _, unit := range utf16.Encode([]rune(*p.Title)) {
			if unit > 127 {
				units += 2
			} else {
				units++
			}
		}
		if (units+1)/2 > 20 {
			return false
		}
		switch *p.Visibility {
		case "公开可见", "仅自己可见", "仅互关好友可见":
		default:
			return false
		}
		if op == "publish_with_video" {
			if len(w.Media) != 1 || w.Media[0].MIMEType != "video/mp4" {
				return false
			}
		} else {
			for _, m := range w.Media {
				if m.MIMEType == "video/mp4" {
					return false
				}
			}
		}
		if p.ScheduleAt != nil {
			s := *p.ScheduleAt
			scheduled, err := time.Parse("2006-01-02T15:04:05.000Z", s)
			if err != nil || scheduled.UTC().Format("2006-01-02T15:04:05.000Z") != s || scheduled.UnixMilli() < now+3600000 || scheduled.UnixMilli() > now+14*86400000 {
				return false
			}
		}
		return true
	}
	t := w.Target
	if t == nil || !frozenID(t.FeedID) || !nullableFrozenID(t.CommentID) || !nullableFrozenID(t.UserID) || len(w.Media) != 0 || p.Title != nil || len(p.Tags) != 0 || p.Visibility != nil || p.IsOriginal != nil || p.ScheduleAt != nil {
		return false
	}
	if op == "reply_comment_in_feed" {
		if t.CommentID == nil {
			return false
		}
	} else if t.CommentID != nil || t.UserID != nil {
		return false
	}
	switch op {
	case "post_comment_to_feed", "reply_comment_in_feed":
		return p.Content != nil && p.Unlike == nil && p.Unfavorite == nil
	case "like_feed":
		return p.Content == nil && p.Unlike != nil && p.Unfavorite == nil
	case "favorite_feed":
		return p.Content == nil && p.Unlike == nil && p.Unfavorite != nil
	default:
		return false
	}
}

// Struct roundtrip makes all nullable/default keys explicit. Keys in this schema
// are fixed ASCII; arbitrary model object keys never enter this encoder.
func canonicalFrozenWrite(w frozenWrite) ([]byte, error) {
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	var value any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err = d.Decode(&value); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	var encode func(any) error
	encode = func(v any) error {
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			if x {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case string:
			if !utf8.ValidString(x) {
				return errFrozenWrite
			}
			appendPermitString(&b, x)
		case json.Number:
			n, e := strconv.ParseInt(string(x), 10, 64)
			if e != nil || n < -maxPermitInteger || n > maxPermitInteger {
				return errFrozenWrite
			}
			b.WriteString(strconv.FormatInt(n, 10))
		case []any:
			b.WriteByte('[')
			for i, item := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				if e := encode(item); e != nil {
					return e
				}
			}
			b.WriteByte(']')
		case map[string]any:
			keys := make([]string, 0, len(x))
			for key := range x {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, key := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				appendPermitString(&b, key)
				b.WriteByte(':')
				if e := encode(x[key]); e != nil {
					return e
				}
			}
			b.WriteByte('}')
		default:
			return errFrozenWrite
		}
		return nil
	}
	if err = encode(value); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
