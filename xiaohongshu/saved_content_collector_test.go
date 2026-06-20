package xiaohongshu

import "testing"

// 纯逻辑单测（无浏览器）。运行：
//   go test ./xiaohongshu -run 'TestBoardCollector|TestBoardStopReason'

func bn(id string) BoardNote { return BoardNote{NoteID: id} }

func TestBoardCollector_AddDedup(t *testing.T) {
	c := newBoardCollector(0) // 0 = 不限
	if got := c.add([]BoardNote{bn("a"), bn("b"), bn("a")}); got != 2 {
		t.Fatalf("first add: want 2 new, got %d", got)
	}
	if got := c.add([]BoardNote{bn("b"), bn("c")}); got != 1 {
		t.Fatalf("second add: want 1 new (c), got %d", got)
	}
	if c.count() != 3 {
		t.Fatalf("count: want 3, got %d", c.count())
	}
}

func TestBoardCollector_SkipsEmptyID(t *testing.T) {
	c := newBoardCollector(0)
	if got := c.add([]BoardNote{bn(""), bn("x"), bn("")}); got != 1 {
		t.Fatalf("want 1 new (empty IDs skipped), got %d", got)
	}
	if c.count() != 1 {
		t.Fatalf("count: want 1, got %d", c.count())
	}
}

func TestBoardCollector_ResultTruncatesToLimit(t *testing.T) {
	c := newBoardCollector(2)
	c.add([]BoardNote{bn("a"), bn("b"), bn("c")})
	res := c.result()
	if len(res) != 2 {
		t.Fatalf("result len: want 2 (truncated), got %d", len(res))
	}
	if res[0].NoteID != "a" || res[1].NoteID != "b" {
		t.Fatalf("result order/content wrong: %+v", res)
	}
}

func TestBoardCollector_ResultNoLimit(t *testing.T) {
	c := newBoardCollector(0)
	c.add([]BoardNote{bn("a"), bn("b"), bn("c")})
	if len(c.result()) != 3 {
		t.Fatalf("limit=0 should not truncate, got %d", len(c.result()))
	}
}

func TestBoardCollector_ReachedLimit(t *testing.T) {
	c := newBoardCollector(2)
	c.add([]BoardNote{bn("a")})
	if c.reachedLimit() {
		t.Fatal("1/2 should not be reached")
	}
	c.add([]BoardNote{bn("b")})
	if !c.reachedLimit() {
		t.Fatal("2/2 should be reached")
	}
	// limit=0 永远未达
	if newBoardCollector(0).reachedLimit() {
		t.Fatal("limit=0 should never be reached")
	}
}

func TestBoardStopReason(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		added []string
		st    boardState
		want  stopReason
	}{
		{
			name:  "limit reached wins over hasMore",
			limit: 1, added: []string{"a", "b"},
			st:   boardState{EntryPresent: true, HasMoreKnown: true, HasMore: true},
			want: stopLimitReached,
		},
		{
			name:  "authoritative end: entry present, hasMore known false",
			limit: 0, added: []string{"a"},
			st:   boardState{EntryPresent: true, HasMoreKnown: true, HasMore: false},
			want: stopHasMoreFalse,
		},
		{
			name:  "hasMore true -> continue",
			limit: 0, added: []string{"a"},
			st:   boardState{EntryPresent: true, HasMoreKnown: true, HasMore: true},
			want: stopContinue,
		},
		{
			name:  "entry absent -> continue (do NOT treat as end)",
			limit: 0, added: nil,
			st:   boardState{EntryPresent: false},
			want: stopContinue,
		},
		{
			name:  "hasMore unknown -> continue (do NOT treat as end)",
			limit: 0, added: []string{"a"},
			st:   boardState{EntryPresent: true, HasMoreKnown: false, HasMore: false},
			want: stopContinue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newBoardCollector(tc.limit)
			notes := make([]BoardNote, 0, len(tc.added))
			for _, id := range tc.added {
				notes = append(notes, bn(id))
			}
			c.add(notes)
			if got := boardStopReason(c, tc.st); got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}
