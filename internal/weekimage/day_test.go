package weekimage

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"testing"
	"time"
)

func TestRenderDayPNGFiltersOtherDates(t *testing.T) {
	start, events := sample()
	options := Options{Start: start, Generated: start.Add(8 * time.Hour), Days: 1, FontFile: testFont(t)}
	r, err := Render(context.Background(), options, events)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(r.PNG))
	if err != nil || cfg.Width != DayWidth || cfg.Height != DayHeight || r.Events != 6 || len(r.PNG) > MaxPNG {
		t.Fatal(cfg, r.Events, len(r.PNG), err)
	}
	// Includes the cross-midnight event starting today, but never the title,
	// calendar legend or count of an event starting on tomorrow's boundary.
	events = append(events, Event{Start: start.AddDate(0, 0, 1), End: start.AddDate(0, 0, 1).Add(time.Hour), Title: "tomorrow secret", Calendar: "outside", CalendarName: "outside private calendar"})
	after, err := Render(context.Background(), options, events)
	if err != nil || !bytes.Equal(r.PNG, after.PNG) {
		t.Fatal("other dates changed day image", err)
	}
	if output := os.Getenv("DAY_PREVIEW"); output != "" {
		if err = os.WriteFile(output, r.PNG, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDayBoundsMidnightPrivacyAndEmpty(t *testing.T) {
	start, _ := sample()
	options := Options{Start: start, Generated: start.Add(8 * time.Hour), Days: 1, FontFile: testFont(t)}
	events := []Event{
		{Start: start.Add(-time.Hour), End: start, Title: "ended yesterday"},
		{Start: start.Add(-time.Hour), End: start.Add(time.Minute), Title: "ongoing", Hidden: true},
		{Start: start, End: start, Title: "instant", Hidden: true},
		{Start: start.AddDate(0, 0, 1), End: start.AddDate(0, 0, 1), Title: "tomorrow instant"},
	}
	a, err := Render(context.Background(), options, events)
	if err != nil || a.Events != 2 {
		t.Fatal(a.Events, err)
	}
	events[1].Title = "another private title"
	events[1].Location = "private place"
	events[1].CalendarName = "private calendar"
	b, err := Render(context.Background(), options, events)
	if err != nil || !bytes.Equal(a.PNG, b.PNG) {
		t.Fatal("hidden day detail leaked", err)
	}
	empty, err := Render(context.Background(), options, nil)
	if err != nil || empty.Events != 0 || len(empty.PNG) == 0 {
		t.Fatal("empty day image failed", err)
	}
	options.Days = 2
	if _, err = Render(context.Background(), options, events); err == nil {
		t.Fatal("unsupported image span accepted")
	}
}

func BenchmarkRenderDay(b *testing.B) {
	start, events := sample()
	options := Options{Start: start, Generated: start.Add(8 * time.Hour), Days: 1, FontFile: testFont(b)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := Render(context.Background(), options, events)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(r.PNG)), "PNG-bytes")
	}
}
