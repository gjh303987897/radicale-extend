package weekimage

import (
	"context"
	"image"
	"testing"
	"time"
)

func TestTimedLayoutCardsStaySeparateAndInsideDay(t *testing.T) {
	start, _ := sample()
	tests := []struct {
		name  string
		spans [][2]int
	}{
		{"nested short event", [][2]int{{1170, 1310}, {1218, 1223}}},
		{"same start", [][2]int{{540, 600}, {540, 570}, {540, 550}}},
		{"adjacent long events", [][2]int{{540, 600}, {600, 660}}},
		{"adjacent short events", [][2]int{{540, 545}, {545, 550}, {550, 555}}},
		{"overlap chain", [][2]int{{540, 600}, {570, 630}, {600, 660}, {630, 690}}},
		{"bottom edge", [][2]int{{1390, 1391}, {1439, 1440}}},
		{"cross midnight", [][2]int{{-60, 10}, {5, 15}, {1438, 1445}}},
		{"overflow before next cluster", [][2]int{{540, 541}, {540, 541}, {540, 541}, {540, 541}, {540, 541}, {540, 541}, {590, 600}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := make([]Event, len(tt.spans))
			for i, s := range tt.spans {
				events[i] = Event{Start: start.Add(time.Duration(s[0]) * time.Minute), End: start.Add(time.Duration(s[1]) * time.Minute)}
			}
			items := segments(events, start, false)
			original := append([]segment(nil), items...)
			cards, collapsed, err := layoutTimed(context.Background(), items, 100, 258, 100, 1540, 0, 24)
			if err != nil {
				t.Fatal(err)
			}
			bounds := image.Rect(100, 100, 358, 1540)
			represented := 0
			hidden := 0
			for i, c := range cards {
				if c.rect.Empty() || !c.rect.In(bounds) {
					t.Fatalf("card outside day: %v", c.rect)
				}
				if c.collapsed > 0 {
					hidden += c.collapsed
					represented += c.collapsed
				} else {
					represented++
					if c.rect.Dy() < minTimedHeight-2 {
						t.Fatalf("unreadable short card: %v", c.rect)
					}
				}
				for j := 0; j < i; j++ {
					if c.rect.Overlaps(cards[j].rect) {
						t.Fatalf("cards overlap: %v and %v", c.rect, cards[j].rect)
					}
				}
			}
			if represented != len(items) || hidden != collapsed {
				t.Fatal("events lost in layout", represented, len(items), hidden, collapsed)
			}
			for i, s := range items {
				if s != original[i] {
					t.Fatal("layout modified calendar times", s, original[i])
				}
			}
			if tt.name == "adjacent long events" {
				for _, c := range cards {
					if c.rect.Dx() != 239 {
						t.Fatal("non-overlapping event lost full width", c.rect)
					}
				}
			}
		})
	}
}

func TestTimedLayoutDenseDayIsBounded(t *testing.T) {
	start, _ := sample()
	events := make([]Event, 2000)
	for i := range events {
		events[i] = Event{Start: start.Add(9 * time.Hour), End: start.Add(10 * time.Hour)}
	}
	cards, collapsed, err := layoutTimed(context.Background(), segments(events, start, false), 100, 258, 100, 1540, 0, 24)
	if err != nil || len(cards) != 5 || collapsed != 1996 {
		t.Fatal(len(cards), collapsed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = layoutTimed(ctx, segments(events, start, false), 100, 258, 100, 1540, 0, 24); err == nil {
		t.Fatal("cancelled layout succeeded")
	}
}
