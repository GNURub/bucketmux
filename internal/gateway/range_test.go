package gateway

import "testing"

func TestParseSingleRange(t *testing.T) {
	tests := []struct {
		name   string
		header string
		size   int64
		start  int64
		end    int64
		ok     bool
	}{
		{name: "explicit", header: "bytes=2-5", size: 10, start: 2, end: 5, ok: true},
		{name: "open ended", header: "bytes=6-", size: 10, start: 6, end: 9, ok: true},
		{name: "suffix", header: "bytes=-4", size: 10, start: 6, end: 9, ok: true},
		{name: "clamp", header: "bytes=6-99", size: 10, start: 6, end: 9, ok: true},
		{name: "invalid start", header: "bytes=10-11", size: 10, ok: false},
		{name: "multi range unsupported", header: "bytes=1-2,4-5", size: 10, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, ok := parseSingleRange(tt.header, tt.size)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if start != tt.start || end != tt.end {
				t.Fatalf("range = %d-%d, want %d-%d", start, end, tt.start, tt.end)
			}
		})
	}
}
