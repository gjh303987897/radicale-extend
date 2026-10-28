// Package weekimage renders a bounded, browser-free rolling calendar image.
package weekimage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const Width, Height = 1960, 1840
const MaxPNG = 4 << 20

type Event struct {
	Start, End                              time.Time
	Title, Location, Calendar, CalendarName string
	AllDay, Hidden                          bool
}
type Options struct {
	Start, Generated time.Time
	FontFile         string
	FontIndex        int
	Truncated        bool
}
type Result struct {
	PNG               []byte
	Events, Collapsed int
}
type ink struct{ accent, fill color.RGBA }

var palette = []ink{
	{rgb(0x3275E7), rgb(0xE8F0FF)}, {rgb(0x248A83), rgb(0xE4F4EF)},
	{rgb(0x8558CD), rgb(0xF0E9FA)}, {rgb(0xC77B20), rgb(0xFCF0DC)},
	{rgb(0xC24E72), rgb(0xFCEAF0)}, {rgb(0x4D8A4E), rgb(0xEBF4E5)},
	{rgb(0x4C7F9E), rgb(0xE6F0F6)},
}

func rgb(n uint32) color.RGBA { return color.RGBA{uint8(n >> 16), uint8(n >> 8), uint8(n), 255} }
func eventInk(key string) ink {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return palette[int(h.Sum32())%len(palette)]
}
func box(dst draw.Image, r image.Rectangle, c color.RGBA) {
	draw.Draw(dst, r, &image.Uniform{C: c}, image.Point{}, draw.Src)
}
func rounded(dst draw.Image, r image.Rectangle, radius int, c color.RGBA) {
	radius = min(radius, min(r.Dx(), r.Dy())/2)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		inset := 0
		if radius > 0 {
			d := radius - (y - r.Min.Y)
			if y >= r.Max.Y-radius {
				d = radius - (r.Max.Y - 1 - y)
			}
			if d > 0 {
				inset = radius - int(math.Sqrt(float64(radius*radius-d*d)))
			}
		}
		box(dst, image.Rect(r.Min.X+inset, y, r.Max.X-inset, y+1), c)
	}
}

type painter struct {
	img                              *image.RGBA
	small, body, medium, date, title font.Face
	colors                           map[string]ink
}

func (p *painter) color(key string) ink {
	if c, ok := p.colors[key]; ok {
		return c
	}
	return eventInk(key)
}
func (p *painter) text(dst draw.Image, x, y int, text string, face font.Face, c color.RGBA) {
	d := font.Drawer{Dst: dst, Src: &image.Uniform{C: c}, Face: face, Dot: fixed.P(x, y)}
	d.DrawString(text)
}
func Text(text string, limit int) string {
	var out strings.Builder
	count := 0
	for _, r := range text {
		if count >= limit {
			out.WriteRune('…')
			break
		}
		if unicode.IsControl(r) {
			r = ' '
		}
		out.WriteRune(r)
		count++
	}
	return strings.TrimSpace(out.String())
}
func fit(text string, face font.Face, width int) string {
	runes := []rune(Text(text, 160))
	if font.MeasureString(face, string(runes)).Ceil() <= width {
		return string(runes)
	}
	ellipsis := "…"
	width -= font.MeasureString(face, ellipsis).Ceil()
	advance := fixed.Int26_6(0)
	for i, r := range runes {
		a, _ := face.GlyphAdvance(r)
		advance += a
		if advance.Ceil() > width {
			return string(runes[:i]) + ellipsis
		}
	}
	return string(runes)
}

type segment struct {
	event     Event
	a, b      float64
	continued bool
	lane      int
}

func segments(events []Event, day time.Time, allDay bool) []segment {
	end := day.AddDate(0, 0, 1)
	out := []segment{}
	for _, e := range events {
		if e.AllDay != allDay || !e.Start.Before(end) {
			continue
		}
		if !e.End.After(day) && !(e.End.Equal(e.Start) && !e.Start.Before(day)) {
			continue
		}
		s, t := e.Start.In(day.Location()), e.End.In(day.Location())
		a := float64(s.Hour()*60+s.Minute()) + float64(s.Second())/60
		b := float64(t.Hour()*60+t.Minute()) + float64(t.Second())/60
		if e.Start.Before(day) {
			a = 0
		}
		if !e.End.Before(end) {
			b = 1440
		}
		if b <= a {
			b = min(1440, a+max(15, e.End.Sub(e.Start).Minutes()))
		}
		out = append(out, segment{event: e, a: a, b: b, continued: e.Start.Before(day)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].a != out[j].a {
			return out[i].a < out[j].a
		}
		if out[i].b != out[j].b {
			return out[i].b > out[j].b
		}
		return out[i].event.Title < out[j].event.Title
	})
	return out
}

func label(e Event) string {
	if e.Hidden {
		return "私人日程"
	}
	if strings.TrimSpace(e.Title) == "" {
		return "无标题日程"
	}
	return e.Title
}

func (p *painter) eventCard(r image.Rectangle, s segment) {
	if r.Empty() {
		return
	}
	colors := p.color(s.event.Calendar)
	rounded(p.img, r, 8, colors.fill)
	box(p.img, image.Rect(r.Min.X, r.Min.Y+6, r.Min.X+4, r.Max.Y-6), colors.accent)
	clip := p.img.SubImage(r.Inset(4)).(*image.RGBA)
	title := label(s.event)
	if s.continued {
		title = "续 · " + title
	}
	clock := s.event.Start.In(s.event.End.Location()).Format("15:04")
	if !s.event.End.Equal(s.event.Start) {
		clock += "–" + s.event.End.Format("15:04")
	}
	x, y, w := r.Min.X+10, r.Min.Y, r.Dx()-18
	if r.Dy() < 42 {
		p.text(clip, x, y+20, fit(clock+" "+title, p.body, w), p.body, colors.accent)
		return
	}
	if r.Dy() < 64 {
		p.text(clip, x, y+19, fit(title, p.body, w), p.body, colors.accent)
		p.text(clip, x, y+37, fit(clock, p.small, w), p.small, colors.accent)
		return
	}
	p.text(clip, x, y+19, fit(clock, p.small, w), p.small, colors.accent)
	p.text(clip, x, y+43, fit(title, p.body, w), p.body, rgb(0x243348))
	if r.Dy() > 90 && !s.event.Hidden && s.event.Location != "" {
		p.text(clip, x, y+67, fit(s.event.Location, p.small, w), p.small, rgb(0x687385))
	}
}

func (p *painter) timed(ctx context.Context, items []segment, colX, colW, top, bottom, first, last int) (int, error) {
	unit := float64(bottom-top) / float64((last-first)*60)
	minMinutes := 28 / unit
	for i := range items {
		items[i].b = max(items[i].b, items[i].a+minMinutes)
	}
	collapsed := 0
	for begin := 0; begin < len(items); {
		if err := ctx.Err(); err != nil {
			return collapsed, err
		}
		end := begin + 1
		clusterEnd := items[begin].b
		for end < len(items) && items[end].a < clusterEnd {
			clusterEnd = max(clusterEnd, items[end].b)
			end++
		}
		ends := []float64{}
		for i := begin; i < end; i++ {
			lane := 0
			for lane < len(ends) && ends[lane] > items[i].a {
				lane++
			}
			if lane == len(ends) {
				ends = append(ends, items[i].b)
			} else {
				ends[lane] = items[i].b
			}
			items[i].lane = lane
		}
		lanes := min(len(ends), 5)
		hidden := 0
		for i := begin; i < end; i++ {
			s := items[i]
			if len(ends) > 5 && s.lane >= 4 {
				hidden++
				continue
			}
			x := colX + 8 + s.lane*(colW-16)/lanes
			y := top + int((s.a-float64(first*60))*unit)
			finish := top + int((s.b-float64(first*60))*unit)
			y = min(y, bottom-28)
			finish = min(bottom, max(finish, y+28))
			p.eventCard(image.Rect(x, y+1, x+(colW-16)/lanes-3, finish-1), s)
		}
		if hidden > 0 {
			x := colX + 8 + 4*(colW-16)/lanes
			y := min(bottom-32, top+int((items[begin].a-float64(first*60))*unit))
			r := image.Rect(x, y, x+(colW-16)/lanes-3, min(bottom, y+64))
			rounded(p.img, r, 8, rgb(0xECEFF4))
			clip := p.img.SubImage(r).(*image.RGBA)
			p.text(clip, x+5, y+22, fmt.Sprintf("+%d", hidden), p.body, rgb(0x687385))
			collapsed += hidden
		}
		begin = end
	}
	return collapsed, nil
}

// Render uses one fixed RGBA canvas (~14 MiB), a ReaderAt-backed font, and a
// bounded PNG buffer. Callers serialize rendering rather than spawning workers.
func Render(ctx context.Context, options Options, events []Event) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if options.Start.IsZero() || len(events) > 2000 {
		return Result{}, errors.New("invalid weekly render bounds")
	}
	file, err := os.Open(options.FontFile)
	if err != nil {
		return Result{}, errors.New("weekly font unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > 64<<20 {
		return Result{}, errors.New("weekly font size invalid")
	}
	collection, err := opentype.ParseCollectionReaderAt(file)
	if err != nil || options.FontIndex < 0 || options.FontIndex >= collection.NumFonts() {
		return Result{}, errors.New("weekly font format/index invalid")
	}
	f, err := collection.Font(options.FontIndex)
	if err != nil {
		return Result{}, errors.New("weekly font parse failed")
	}
	faces := []font.Face{}
	defer func() {
		for _, face := range faces {
			_ = face.Close()
		}
	}()
	for _, size := range []float64{14, 17, 20, 30, 40} {
		face, e := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
		if e != nil {
			return Result{}, e
		}
		faces = append(faces, face)
	}
	p := painter{img: image.NewRGBA(image.Rect(0, 0, Width, Height)), small: faces[0], body: faces[1], medium: faces[2], date: faces[3], title: faces[4]}
	box(p.img, p.img.Bounds(), rgb(0xF5F6FA))
	start := time.Date(options.Start.Year(), options.Start.Month(), options.Start.Day(), 0, 0, 0, 0, options.Start.Location())
	end := start.AddDate(0, 0, 6)
	generated := options.Generated.In(start.Location())
	todayIndex := -1
	for d := 0; d < 7; d++ {
		if start.AddDate(0, 0, d).Format("2006-01-02") == generated.Format("2006-01-02") {
			todayIndex = d
		}
	}
	p.text(p.img, 48, 78, "未来七天", p.title, rgb(0x202B3D))
	p.text(p.img, 50, 116, fmt.Sprintf("%s — %s  ·  %s", start.Format("2006.01.02"), end.Format("2006.01.02"), start.Location()), p.body, rgb(0x768195))
	rounded(p.img, image.Rect(Width-380, 44, Width-48, 94), 24, rgb(0xE7EEFC))
	p.text(p.img, Width-358, 77, fmt.Sprintf("%d 项日程  /  滚动周视图", len(events)), p.medium, rgb(0x3B6DB9))
	type legend struct{ key, name string }
	legends := []legend{}
	seen := map[string]bool{}
	for _, e := range events {
		if !seen[e.Calendar] {
			name := e.CalendarName
			if name == "" {
				name = "日历"
			}
			if e.Hidden {
				name = "隐藏日历"
			}
			legends = append(legends, legend{e.Calendar, name})
			seen[e.Calendar] = true
		}
	}
	sort.Slice(legends, func(i, j int) bool { return legends[i].key < legends[j].key })
	p.colors = map[string]ink{}
	used := map[ink]bool{}
	for _, l := range legends {
		c := eventInk(l.key)
		if len(used) < len(palette) {
			for used[c] {
				for i, v := range palette {
					if c == v {
						c = palette[(i+1)%len(palette)]
						break
					}
				}
			}
		}
		p.colors[l.key] = c
		used[c] = true
	}
	for i, l := range legends {
		if i >= 7 {
			break
		}
		x := 50 + i*260
		rounded(p.img, image.Rect(x, 143, x+10, 153), 5, p.color(l.key).accent)
		p.text(p.img, x+20, 154, fit(l.name, p.body, 225), p.body, rgb(0x586579))
	}
	rounded(p.img, image.Rect(36, 182, Width-36, 1744), 20, rgb(0xE7EAF0))
	rounded(p.img, image.Rect(36, 179, Width-36, 1740), 20, rgb(0xFFFFFF))
	plotX, colW := 112, (Width-148)/7
	all := make([][]segment, 7)
	timed := make([][]segment, 7)
	allRows, first, last := 1, 7, 22
	dstShift := false
	for d := 0; d < 7; d++ {
		day := start.AddDate(0, 0, d)
		all[d] = segments(events, day, true)
		timed[d] = segments(events, day, false)
		allRows = max(allRows, min(len(all[d]), 3))
		if day.AddDate(0, 0, 1).Sub(day) != 24*time.Hour {
			dstShift = true
		}
		for i := range timed[d] {
			s := &timed[d][i]
			s.event.Start = s.event.Start.In(start.Location())
			s.event.End = s.event.End.In(start.Location())
			first = min(first, int(math.Floor(s.a/60)))
			last = max(last, int(math.Ceil(s.b/60)))
		}
	}
	first = max(0, first)
	last = min(24, last)
	gridTop, gridBottom := 290+allRows*34+14, 1700
	weekdays := []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	collapsed := 0
	for d := 0; d < 7; d++ {
		if err = ctx.Err(); err != nil {
			return Result{}, err
		}
		day := start.AddDate(0, 0, d)
		x := plotX + d*colW
		shade := rgb(0xFFFFFF)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			shade = rgb(0xFAFBFE)
		}
		if d == todayIndex {
			shade = rgb(0xF3F7FF)
		}
		box(p.img, image.Rect(x, 194, x+colW-1, 1728), shade)
		name := weekdays[day.Weekday()]
		if d == todayIndex {
			name += " · 今天"
		}
		p.text(p.img, x+18, 223, name, p.medium, rgb(0x7A8698))
		if d == todayIndex {
			rounded(p.img, image.Rect(x+11, 234, x+122, 273), 14, rgb(0x377AE5))
			p.text(p.img, x+21, 264, day.Format("01/02"), p.date, rgb(0xFFFFFF))
		} else {
			p.text(p.img, x+18, 264, day.Format("01/02"), p.date, rgb(0x26354B))
		}
		for row, s := range all[d] {
			if row >= allRows {
				break
			}
			y := 290 + row*34
			if len(all[d]) > allRows && row == allRows-1 {
				count := len(all[d]) - row
				collapsed += count
				p.text(p.img, x+14, y+21, fmt.Sprintf("+%d 项全天日程", count), p.small, rgb(0x758296))
				break
			}
			colors := p.color(s.event.Calendar)
			rounded(p.img, image.Rect(x+8, y, x+colW-8, y+28), 6, colors.fill)
			p.text(p.img, x+15, y+20, fit(label(s.event), p.body, colW-32), p.body, colors.accent)
		}
		box(p.img, image.Rect(x, 279, x+colW, 280), rgb(0xE9EDF4))
		if d > 0 {
			box(p.img, image.Rect(x, 194, x+1, 1728), rgb(0xE8ECF3))
		}
	}
	p.text(p.img, 48, 310, "全天", p.small, rgb(0x8390A3))
	unit := float64(gridBottom-gridTop) / float64(last-first)
	for hour := first; hour <= last; hour++ {
		y := gridTop + int(float64(hour-first)*unit)
		box(p.img, image.Rect(plotX, y, plotX+7*colW, y+1), rgb(0xE5EAF2))
		p.text(p.img, 48, y+5, fmt.Sprintf("%02d:00", hour), p.small, rgb(0x8994A5))
		if hour < last {
			half := y + int(unit/2)
			box(p.img, image.Rect(plotX, half, plotX+7*colW, half+1), rgb(0xF0F3F8))
		}
	}
	for d := 0; d < 7; d++ {
		n, e := p.timed(ctx, timed[d], plotX+d*colW, colW, gridTop, gridBottom, first, last)
		if e != nil {
			return Result{}, e
		}
		collapsed += n
	}
	if todayIndex >= 0 && generated.Hour() >= first && generated.Hour() < last {
		y := gridTop + int((float64(generated.Hour())+float64(generated.Minute())/60-float64(first))*unit)
		x := plotX + todayIndex*colW
		box(p.img, image.Rect(x, y, x+colW, y+2), rgb(0xEB6767))
		rounded(p.img, image.Rect(x-4, y-3, x+4, y+5), 4, rgb(0xEB6767))
	}
	footer := fmt.Sprintf("生成于 %s  ·  同色代表同一日历  ·  时间按本地时区排列", generated.Format("01-02 15:04"))
	p.text(p.img, 48, 1791, footer, p.body, rgb(0x7A879B))
	warnings := []string{}
	if collapsed > 0 {
		warnings = append(warnings, fmt.Sprintf("%d 个密集日程块以 +N 折叠，完整信息请用 /today、/next 或日历客户端查看", collapsed))
	}
	if options.Truncated {
		warnings = append(warnings, "已达事件数量上限，部分日程未显示")
	}
	if dstShift {
		warnings = append(warnings, "包含夏令时切换日，时段按当地钟表时间显示")
	}
	if len(events) == 0 {
		warnings = append(warnings, "未来七天暂无日程，留一点时间给自己。")
	}
	p.text(p.img, 48, 1822, fit(strings.Join(warnings, "  ·  "), p.small, Width-96), p.small, rgb(0x8793A5))
	var data bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err = encoder.Encode(limitWriter{writer: &data, limit: MaxPNG}, p.img); err != nil {
		return Result{}, err
	}
	return Result{PNG: data.Bytes(), Events: len(events), Collapsed: collapsed}, nil
}

type limitWriter struct {
	writer *bytes.Buffer
	limit  int
}

func (w limitWriter) Write(p []byte) (int, error) {
	if w.writer.Len()+len(p) > w.limit {
		return 0, errors.New("weekly PNG size limit exceeded")
	}
	return w.writer.Write(p)
}
