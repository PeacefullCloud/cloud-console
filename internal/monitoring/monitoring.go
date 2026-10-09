// Package monitoring samples instance resource usage.
//
// Incus only exposes live counters, so the console samples them on a timer and
// keeps a rolling history. That history powers the CPU/memory graphs on the
// instance page.
package monitoring

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	incusapi "github.com/lxc/incus/v6/shared/api"

	"github.com/peaceful/cloud-console/internal/core"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// Windows offered by the metrics tab.
var (
	WindowHour   = time.Hour
	WindowDay    = 24 * time.Hour
	WindowWeek   = 7 * 24 * time.Hour
	WindowMonth  = 30 * 24 * time.Hour
	windowLabels = map[string]time.Duration{
		"hour":  WindowHour,
		"day":   WindowDay,
		"week":  WindowWeek,
		"month": WindowMonth,
	}
)

// WindowFor resolves a UI window key such as "hour" or "week".
func WindowFor(key string) time.Duration {
	if d, ok := windowLabels[key]; ok {
		return d
	}
	return WindowHour
}

// Service samples metrics and serves chart data.
type Service struct {
	app *core.App

	mu   sync.Mutex
	last map[string]cpuSample
}

type cpuSample struct {
	ts       time.Time
	cpuNanos int64
	cpus     int
}

// New creates the monitoring service.
func New(app *core.App) *Service {
	return &Service{app: app, last: map[string]cpuSample{}}
}

// Run samples every instance on a timer until the context is cancelled.
func (s *Service) Run(ctx context.Context) {
	interval := time.Duration(s.app.Cfg.MetricsIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Also prune old rows on a slower cadence.
	pruneTicker := time.NewTicker(time.Hour)
	defer pruneTicker.Stop()

	// Take a first sample straight away so the dashboard is not empty.
	if err := s.SampleAll(ctx); err != nil {
		s.app.Log.Warn("initial metrics sample failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.SampleAll(ctx); err != nil {
				s.app.Log.Warn("metrics sample failed", "err", err)
			}
		case <-pruneTicker.C:
			s.prune()
		}
	}
}

// SampleAll records one sample per instance.
func (s *Service) SampleAll(ctx context.Context) error {
	fulls, err := s.app.Incus.Instances(ctx)
	if err != nil {
		return err
	}

	batch := make([]models.Metric, 0, len(fulls))
	for i := range fulls {
		if m := s.measure(&fulls[i]); m != nil {
			batch = append(batch, *m)
		}
	}
	return s.app.DB.InsertMetrics(batch)
}

// Sample records a sample for a single instance.
func (s *Service) Sample(ctx context.Context, name string) error {
	full, err := s.app.Incus.InstanceFull(name)
	if err != nil {
		return err
	}
	return s.sample(ctx, full)
}

func (s *Service) sample(_ context.Context, full *incusapi.InstanceFull) error {
	m := s.measure(full)
	if m == nil {
		return nil
	}
	return s.app.DB.InsertMetric(m)
}

// measure turns an instance's live state into a sample, or nil when the
// instance is not running. It keeps the CPU baseline between calls.
func (s *Service) measure(full *incusapi.InstanceFull) *models.Metric {
	state := full.State
	if state == nil {
		return nil
	}

	inst := full.Instance
	now := time.Now()

	// CPU usage is cumulative nanoseconds; a percentage needs a delta.
	cpus := incus.InstanceCPU(inst)
	if cpus <= 0 {
		cpus = s.hostCPUs()
	}
	if cpus <= 0 {
		cpus = 1
	}

	var pct float64
	s.mu.Lock()
	if prev, ok := s.last[inst.Name]; ok {
		elapsed := now.Sub(prev.ts).Seconds()
		delta := state.CPU.Usage - prev.cpuNanos
		if elapsed > 0 && delta > 0 {
			pct = (float64(delta) / 1e9) / elapsed / float64(cpus) * 100
			if pct > 100 {
				pct = 100
			}
		}
	}
	s.last[inst.Name] = cpuSample{ts: now, cpuNanos: state.CPU.Usage, cpus: cpus}
	s.mu.Unlock()

	rx, tx := incus.StateNetworkTotals(state)

	return &models.Metric{
		InstanceName: inst.Name,
		TS:           now,
		CPUPct:       math.Round(pct*100) / 100,
		MemUsed:      state.Memory.Usage,
		MemTotal:     state.Memory.Total,
		DiskUsed:     incus.StateDiskUsed(state),
		DiskTotal:    incus.InstanceDiskBytes(inst),
		NetRxBytes:   rx,
		NetTxBytes:   tx,
	}
}

// maxChartPoints caps how many samples a chart receives. A month at one
// sample a minute is 43,200 points, far more than a chart can show and more
// than is worth rendering and sending.
const maxChartPoints = 480

// History returns samples for an instance within a window, thinned to at most
// maxChartPoints.
func (s *Service) History(instanceName string, window time.Duration) ([]models.Metric, error) {
	samples, err := s.app.DB.ListMetrics(instanceName, time.Now().Add(-window))
	if err != nil {
		return nil, err
	}
	return Downsample(samples, maxChartPoints), nil
}

// Downsample reduces samples (oldest first) to at most max points by averaging
// gauges over equal-sized groups. Network counters are cumulative, so the last
// value of each group is kept.
func Downsample(samples []models.Metric, max int) []models.Metric {
	if max < 1 || len(samples) <= max {
		return samples
	}

	size := (len(samples) + max - 1) / max
	out := make([]models.Metric, 0, max)
	for start := 0; start < len(samples); start += size {
		end := start + size
		if end > len(samples) {
			end = len(samples)
		}
		group := samples[start:end]

		var cpu float64
		var mem, memTotal, disk, diskTotal int64
		for _, m := range group {
			cpu += m.CPUPct
			mem += m.MemUsed
			memTotal += m.MemTotal
			disk += m.DiskUsed
			diskTotal += m.DiskTotal
		}
		n := int64(len(group))
		last := group[len(group)-1]
		out = append(out, models.Metric{
			InstanceName: last.InstanceName,
			TS:           group[len(group)/2].TS,
			CPUPct:       math.Round(cpu/float64(n)*100) / 100,
			MemUsed:      mem / n,
			MemTotal:     memTotal / n,
			DiskUsed:     disk / n,
			DiskTotal:    diskTotal / n,
			NetRxBytes:   last.NetRxBytes,
			NetTxBytes:   last.NetTxBytes,
		})
	}
	return out
}

// prune deletes samples older than the retention window.
func (s *Service) prune() {
	cutoff := time.Now().Add(-time.Duration(s.app.Cfg.MetricsRetentionHours) * time.Hour)
	n, err := s.app.DB.PruneMetrics(cutoff)
	if err != nil {
		s.app.Log.Warn("could not prune metrics", "err", err)
		return
	}
	if n > 0 {
		s.app.Log.Info("pruned old metric samples", "count", n)
	}
}

// Forget drops the in-memory baseline for an instance that no longer exists.
func (s *Service) Forget(instanceName string) {
	s.mu.Lock()
	delete(s.last, instanceName)
	s.mu.Unlock()
}

func (s *Service) hostCPUs() int {
	if s.app.Incus == nil {
		return 0
	}
	res, err := s.app.Incus.ServerResources()
	if err != nil || res == nil {
		return 0
	}
	return int(res.CPU.Total)
}

// --- charts ---------------------------------------------------------------

// Series is everything a template needs to draw a small line chart.
type Series struct {
	Points  string  // polyline "x,y x,y"
	Area    string  // closed path for the filled area
	Max     float64 // scale maximum
	HasData bool
	Labels  []string // first, middle and last x-axis labels
	Unit    string
	Last    float64
	Avg     float64
}

// BuildSeries converts values into SVG geometry for a width x height box.
func BuildSeries(values []float64, times []time.Time, unit string) Series {
	s := Series{Unit: unit}
	if len(values) == 0 {
		return s
	}

	s.HasData = true

	max := 0.0
	sum := 0.0
	for _, v := range values {
		if v > max {
			max = v
		}
		sum += v
	}
	s.Last = values[len(values)-1]
	s.Avg = sum / float64(len(values))

	// Round the axis up to a friendly number so the line does not touch the top.
	s.Max = niceMax(max)

	const (
		width  = 100.0
		height = 100.0
	)

	var points []string
	for i, v := range values {
		x := 0.0
		if len(values) > 1 {
			x = float64(i) / float64(len(values)-1) * width
		}
		y := height - (v/s.Max)*height
		if y < 0 {
			y = 0
		}
		points = append(points, fmt.Sprintf("%.2f,%.2f", x, y))
	}

	for i, p := range points {
		if i > 0 {
			s.Points += " "
		}
		s.Points += p
	}

	// Close the polyline along the bottom edge to fill the area.
	baseline := fmt.Sprintf("%.2f", height)
	s.Area = "M " + points[0] + " L " + s.Points +
		" L " + fmt.Sprintf("%.2f,%s", width, baseline) + " L 0," + baseline + " Z"

	if len(times) >= 2 {
		s.Labels = []string{
			times[0].Format("15:04"),
			times[len(times)/2].Format("15:04"),
			times[len(times)-1].Format("15:04"),
		}
	}
	return s
}

// niceMax rounds a maximum up to a readable axis value.
func niceMax(max float64) float64 {
	switch {
	case max <= 0:
		return 1
	case max <= 10:
		return 10
	case max <= 25:
		return 25
	case max <= 50:
		return 50
	case max <= 100:
		return 100
	default:
		return math.Ceil(max/100) * 100
	}
}

// PercentSeries extracts CPU percentages from metric samples.
func PercentSeries(samples []models.Metric) Series {
	values := make([]float64, 0, len(samples))
	times := make([]time.Time, 0, len(samples))
	for _, m := range samples {
		values = append(values, m.CPUPct)
		times = append(times, m.TS)
	}
	return BuildSeries(values, times, "%")
}

// MemorySeries extracts memory usage as a percentage of the limit.
func MemorySeries(samples []models.Metric) Series {
	values := make([]float64, 0, len(samples))
	times := make([]time.Time, 0, len(samples))

	for _, m := range samples {
		total := m.MemTotal
		if total <= 0 {
			total = 1
		}
		values = append(values, float64(m.MemUsed)/float64(total)*100)
		times = append(times, m.TS)
	}
	return BuildSeries(values, times, "%")
}

// --- host -----------------------------------------------------------------

// Host summarises the physical server.
type Host struct {
	CPUTotal     int
	MemoryTotal  int64
	MemoryUsed   int64
	StorageName  string
	StorageUsed  int64
	StorageTotal int64
	Pools        []Pool
	ServerName   string
	Version      string
	Driver       string
	Uptime       time.Duration
}

// Pool is one storage pool with its capacity.
type Pool struct {
	Name   string
	Driver string
	Used   int64
	Total  int64
}

// UsagePercent returns the host memory usage as a percentage.
func (h Host) UsagePercent() float64 {
	if h.MemoryTotal <= 0 {
		return 0
	}
	return float64(h.MemoryUsed) / float64(h.MemoryTotal) * 100
}

// HostStats reads server capacity from Incus.
func (s *Service) HostStats() (*Host, error) {
	host := &Host{}

	if server, err := s.app.Incus.Server(); err == nil && server != nil {
		host.ServerName = server.Environment.ServerName
		host.Version = server.Environment.ServerVersion
		host.Driver = server.Environment.Driver
	}

	res, err := s.app.Incus.ServerResources()
	if err != nil {
		return nil, err
	}

	host.CPUTotal = int(res.CPU.Total)
	host.MemoryTotal = int64(res.Memory.Total)
	host.MemoryUsed = int64(res.Memory.Used)

	pools, err := s.app.Incus.StoragePools()
	if err != nil {
		s.app.Log.Warn("could not list storage pools", "err", err)
		return host, nil
	}

	for _, p := range pools {
		entry := Pool{Name: p.Name, Driver: p.Driver}

		if usage, err := s.app.Incus.StoragePoolResources(p.Name); err == nil {
			entry.Used = int64(usage.Space.Used)
			entry.Total = int64(usage.Space.Total)
		}
		host.Pools = append(host.Pools, entry)
	}

	sort.Slice(host.Pools, func(i, j int) bool { return host.Pools[i].Name < host.Pools[j].Name })

	if len(host.Pools) > 0 {
		host.StorageName = host.Pools[0].Name
		host.StorageUsed = host.Pools[0].Used
		host.StorageTotal = host.Pools[0].Total
	}

	return host, nil
}
