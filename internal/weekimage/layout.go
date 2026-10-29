package weekimage

import (
	"context"
	"errors"
	"image"
	"sort"
)

const minTimedHeight = 48

type timedCard struct {
	segment
	rect            image.Rectangle
	lane, collapsed int
}

// layoutTimed allocates lanes using the displayed intervals, including the
// minimum readable height and bottom-edge adjustment. Calendar times remain
// unchanged: a five-minute event still shows its actual five-minute range.
func layoutTimed(ctx context.Context, items []segment, colX, colW, top, bottom, first, last int) ([]timedCard, int, error) {
	return layoutTimedHeight(ctx, items, colX, colW, top, bottom, first, last, minTimedHeight)
}
func layoutTimedHeight(ctx context.Context, items []segment, colX, colW, top, bottom, first, last, minHeight int) ([]timedCard, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if colW < 32 || bottom <= top || last <= first {
		return nil, 0, errors.New("invalid weekly layout bounds")
	}
	unit := float64(bottom-top) / float64((last-first)*60)
	height := min(minHeight, bottom-top)
	positions := make([]timedCard, len(items))
	for i, s := range items {
		y := top + int((s.a-float64(first*60))*unit)
		finish := top + int((s.b-float64(first*60))*unit)
		y = max(top, min(y, bottom-height))
		finish = min(bottom, max(finish, y+height))
		positions[i] = timedCard{segment: s, rect: image.Rect(0, y, 0, finish)}
	}
	sort.SliceStable(positions, func(i, j int) bool {
		if positions[i].rect.Min.Y != positions[j].rect.Min.Y {
			return positions[i].rect.Min.Y < positions[j].rect.Min.Y
		}
		if positions[i].a != positions[j].a {
			return positions[i].a < positions[j].a
		}
		return positions[i].b > positions[j].b
	})
	cards := make([]timedCard, 0, len(items))
	collapsed := 0
	for begin := 0; begin < len(positions); {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		end := begin + 1
		clusterEnd := positions[begin].rect.Max.Y
		for end < len(positions) && positions[end].rect.Min.Y < clusterEnd {
			clusterEnd = max(clusterEnd, positions[end].rect.Max.Y)
			end++
		}
		ends := []int{}
		for i := begin; i < end; i++ {
			lane := 0
			for lane < len(ends) && ends[lane] > positions[i].rect.Min.Y {
				lane++
			}
			if lane == len(ends) {
				ends = append(ends, positions[i].rect.Max.Y)
			} else {
				ends[lane] = positions[i].rect.Max.Y
			}
			positions[i].lane = lane
		}
		lanes := min(len(ends), 5)
		cellWidth := (colW - 16) / lanes
		hidden := 0
		for i := begin; i < end; i++ {
			c := positions[i]
			if len(ends) > 5 && c.lane >= 4 {
				hidden++
				continue
			}
			x := colX + 8 + c.lane*cellWidth
			c.rect = image.Rect(x, c.rect.Min.Y+1, x+cellWidth-3, c.rect.Max.Y-1)
			cards = append(cards, c)
		}
		if hidden > 0 {
			x := colX + 8 + 4*cellWidth
			// The overflow badge must also stay inside its overlap cluster;
			// a taller badge would cover a following full-width event.
			h := min(64, clusterEnd-positions[begin].rect.Min.Y)
			y := min(bottom-h, positions[begin].rect.Min.Y)
			cards = append(cards, timedCard{rect: image.Rect(x, y, x+cellWidth-3, y+h), collapsed: hidden})
			collapsed += hidden
		}
		begin = end
	}
	return cards, collapsed, nil
}
