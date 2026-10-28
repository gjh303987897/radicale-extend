package weekimage

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/image/font/gofont/goregular"
)

func testFont(t testing.TB) string {
	t.Helper()
	if path := os.Getenv("WEEK_TEST_FONT"); path != "" {
		return path
	}
	p := filepath.Join(t.TempDir(), "font.ttf")
	if err := os.WriteFile(p, goregular.TTF, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func sample() (time.Time, []Event) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Date(2026, 10, 29, 0, 0, 0, 0, loc)
	at := func(day, hour, minute int) time.Time {
		return start.AddDate(0, 0, day).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	events := []Event{
		{Start: at(0, 0, 0), End: at(3, 0, 0), Title: "产品发布周", Calendar: "work", CalendarName: "工作日历", AllDay: true},
		{Start: at(0, 9, 0), End: at(0, 10, 30), Title: "年度体检", Location: "健康中心", Calendar: "personal", CalendarName: "个人生活"},
		{Start: at(0, 9, 30), End: at(0, 11, 0), Title: "项目评审 · 新功能方案", Location: "会议室 A", Calendar: "work", CalendarName: "工作日历"},
		{Start: at(0, 12, 0), End: at(0, 12, 30), Title: "午间散步", Calendar: "personal", CalendarName: "个人生活"},
		{Start: at(0, 14, 0), End: at(0, 15, 30), Title: "设计交付与视觉走查", Location: "设计工作室", Calendar: "design", CalendarName: "创作与设计"},
		{Start: at(0, 22, 30), End: at(1, 1, 0), Title: "数据同步窗口", Calendar: "work", CalendarName: "工作日历"},
		{Start: at(1, 10, 0), End: at(1, 11, 0), Title: "下一阶段规划", Calendar: "work", CalendarName: "工作日历"},
		{Start: at(1, 12, 0), End: at(1, 13, 0), Title: "团队午餐", Location: "街角餐厅", Calendar: "personal", CalendarName: "个人生活"},
		{Start: at(2, 9, 0), End: at(2, 12, 0), Title: "周末骑行", Location: "滨河绿道", Calendar: "personal", CalendarName: "个人生活"},
		{Start: at(3, 18, 0), End: at(3, 20, 0), Title: "家庭聚餐", Calendar: "personal", CalendarName: "个人生活"},
		{Start: at(4, 9, 0), End: at(4, 9, 45), Title: "周例会", Calendar: "work", CalendarName: "工作日历"},
		{Start: at(5, 15, 0), End: at(5, 16, 30), Title: "客户演示", Calendar: "design", CalendarName: "创作与设计"},
		{Start: at(6, 10, 0), End: at(6, 11, 0), Title: "结项复盘", Calendar: "work", CalendarName: "工作日历"},
	}
	return start, events
}
func TestRenderWeeklyPNG(t *testing.T) {
	start, events := sample()
	result, err := Render(context.Background(), Options{Start: start, Generated: start.Add(8 * time.Hour), FontFile: testFont(t)}, events)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(result.PNG))
	if err != nil || cfg.Width != Width || cfg.Height != Height || len(result.PNG) > MaxPNG || result.Events != len(events) {
		t.Fatal(cfg, len(result.PNG), result, err)
	}
	if output := os.Getenv("WEEK_PREVIEW"); output != "" {
		if err = os.WriteFile(output, result.PNG, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCrossDayOverlapAndBounds(t *testing.T) {
	start, events := sample()
	items := segments(events, start.AddDate(0, 0, 1), false)
	if len(items) != 3 || !items[0].continued || items[0].a != 0 || items[0].b != 60 {
		t.Fatal(items)
	}
	day := segments(events, start.AddDate(0, 0, 3), true)
	if len(day) != 0 {
		t.Fatal("all-day end is exclusive", day)
	}
	busy := []Event{}
	for i := 0; i < 9; i++ {
		busy = append(busy, Event{Start: start.Add(9 * time.Hour), End: start.Add(10 * time.Hour), Title: "Busy", Calendar: "work"})
	}
	r, err := Render(context.Background(), Options{Start: start, Generated: start, FontFile: testFont(t)}, busy)
	if err != nil || r.Collapsed != 5 {
		t.Fatal(r.Collapsed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Render(ctx, Options{Start: start, FontFile: testFont(t)}, nil); err == nil {
		t.Fatal("cancelled render succeeded")
	}
	if len([]rune(Text("long private text", 4))) > 5 {
		t.Fatal("unbounded label")
	}
}
func BenchmarkRender(b *testing.B) {
	start, events := sample()
	options := Options{Start: start, Generated: start.Add(8 * time.Hour), FontFile: testFont(b)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := Render(context.Background(), options, events)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(result.PNG)), "PNG-bytes")
	}
}

func TestHiddenEventsDoNotLeakTitlesLocationsOrCalendarNames(t *testing.T) {
	start, events := sample()
	event := events[1]
	event.Hidden = true
	options := Options{Start: start, Generated: start.Add(8 * time.Hour), FontFile: testFont(t)}
	a, err := Render(context.Background(), options, []Event{event})
	if err != nil {
		t.Fatal(err)
	}
	event.Title = "different secret"
	event.Location = "private address"
	event.CalendarName = "private calendar name"
	b, err := Render(context.Background(), options, []Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.PNG, b.PNG) {
		t.Fatal("hidden text changed rendered pixels")
	}
}
