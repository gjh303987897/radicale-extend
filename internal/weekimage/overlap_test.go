package weekimage

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

func overlapSample() (time.Time, []Event) {
	start, _ := sample()
	at := func(h, m int) time.Time { return start.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	return start, []Event{
		{Start: start, End: start.AddDate(0, 0, 1), AllDay: true, Title: "生活安排", Calendar: "personal", CalendarName: "个人生活"},
		{Start: at(16, 30), End: at(18, 30), Title: "阅读与笔记", Calendar: "personal"},
		{Start: at(18, 30), End: at(19, 30), Title: "晚餐与散步", Calendar: "personal"},
		{Start: at(19, 30), End: at(21, 50), Title: "整理本周学习笔记与阅读计划", Calendar: "personal"},
		{Start: at(20, 18), End: at(20, 23), Title: "Drink", Calendar: "personal"},
		{Start: at(22, 0), End: at(23, 0), Title: "运动", Calendar: "personal"},
	}
}

// A narrow five-minute card used to spend its entire line on the time range,
// so neither its title nor the end of its time range affected the image.
func TestShortOverlapKeepsTitleAndFullClock(t *testing.T) {
	start, events := overlapSample()
	options := Options{Start: start, Generated: start.Add(19*time.Hour + 41*time.Minute), FontFile: testFont(t)}
	before, err := Render(context.Background(), options, events)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title", "end"} {
		t.Run(field, func(t *testing.T) {
			changed := append([]Event(nil), events...)
			if field == "title" {
				changed[4].Title = "Break"
			} else {
				changed[4].End = changed[4].End.Add(time.Minute)
			}
			after, err := Render(context.Background(), options, changed)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(before.PNG, after.PNG) {
				t.Fatalf("short overlapping event's %s is invisible", field)
			}
		})
	}
	if output := os.Getenv("WEEK_OVERLAP_PREVIEW"); output != "" {
		events[4].Title = "喝水提醒"
		preview, err := Render(context.Background(), options, events)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(output, preview.PNG, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
