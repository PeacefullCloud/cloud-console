package monitoring

import (
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/models"
)

func series(n int) []models.Metric {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]models.Metric, n)
	for i := range out {
		out[i] = models.Metric{
			InstanceName: "web",
			TS:           base.Add(time.Duration(i) * time.Minute),
			CPUPct:       float64(i % 10),
			MemUsed:      int64(1000 + i),
			MemTotal:     4000,
			NetRxBytes:   int64(i * 100),
		}
	}
	return out
}

func TestDownsampleLeavesSmallSeriesAlone(t *testing.T) {
	in := series(100)
	if got := Downsample(in, 480); len(got) != 100 {
		t.Fatalf("len = %d", len(got))
	}
}

func TestDownsampleCapsPointsAndKeepsOrder(t *testing.T) {
	in := series(43200) // a month at one sample a minute
	got := Downsample(in, 480)
	if len(got) > 480 || len(got) < 400 {
		t.Fatalf("len = %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].TS.After(got[i-1].TS) {
			t.Fatalf("points out of order at %d", i)
		}
	}
}

func TestDownsampleAveragesGaugesAndKeepsLastCounter(t *testing.T) {
	in := []models.Metric{
		{CPUPct: 10, MemUsed: 100, MemTotal: 1000, NetRxBytes: 5},
		{CPUPct: 30, MemUsed: 300, MemTotal: 1000, NetRxBytes: 9},
		{CPUPct: 50, MemUsed: 500, MemTotal: 1000, NetRxBytes: 20},
		{CPUPct: 70, MemUsed: 700, MemTotal: 1000, NetRxBytes: 30},
	}
	got := Downsample(in, 2)
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].CPUPct != 20 || got[0].MemUsed != 200 || got[0].NetRxBytes != 9 {
		t.Errorf("first bucket = %+v", got[0])
	}
	if got[1].CPUPct != 60 || got[1].MemUsed != 600 || got[1].NetRxBytes != 30 {
		t.Errorf("second bucket = %+v", got[1])
	}
}

func TestDownsampleEdgeCases(t *testing.T) {
	if got := Downsample(nil, 10); len(got) != 0 {
		t.Errorf("nil -> %v", got)
	}
	if got := Downsample(series(5), 0); len(got) != 5 {
		t.Errorf("max 0 changed the series: %d", len(got))
	}
	if got := Downsample(series(7), 3); len(got) > 3 {
		t.Errorf("len = %d", len(got))
	}
}
