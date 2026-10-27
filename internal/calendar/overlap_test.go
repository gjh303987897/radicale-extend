package calendar

import (
	"testing"
	"time"
)

func TestOverlappingRecurrencesAndExclusiveEnd(t *testing.T) {
	data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:20261026T230000Z\r\nDTEND:20261027T020000Z\r\nRRULE:FREQ=DAILY;COUNT=4\r\nEXDATE:20261027T230000Z\r\nSUMMARY:Night\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:a\r\nRECURRENCE-ID:20261028T230000Z\r\nDTSTART:20261028T010000Z\r\nDTEND:20261028T040000Z\r\nSUMMARY:Moved\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	items, err := ExpandOverlapping(cal, from, from.AddDate(0, 0, 1), time.UTC, 100)
	if err != nil || len(items) != 1 || items[0].View.Summary != "Moved" {
		t.Fatal(items, err)
	}
	cal, err = Parse("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:day\r\nDTSTART;VALUE=DATE:20261026\r\nDTEND;VALUE=DATE:20261028\r\nSUMMARY:All day\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	if err != nil {
		t.Fatal(err)
	}
	items, err = ExpandOverlapping(cal, from.AddDate(0, 0, -1), from, time.UTC, 100)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	items, err = ExpandOverlapping(cal, from, from.AddDate(0, 0, 1), time.UTC, 100)
	if err != nil || len(items) != 0 {
		t.Fatal("DTEND is exclusive", items, err)
	}
}
