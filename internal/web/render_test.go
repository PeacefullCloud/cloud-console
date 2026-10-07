package web

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peaceful/cloud-console/internal/backups"
	"github.com/peaceful/cloud-console/internal/domains"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/instances"
	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/monitoring"
	"github.com/peaceful/cloud-console/internal/snapshots"
)

// newTestRenderer loads the real templates from the repository so a syntax or
// field error fails the test rather than a user's first page load.
func newTestRenderer(t *testing.T) *Renderer {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	renderer, err := NewRenderer(os.DirFS("../.."), true, log)
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}
	return renderer
}

func sampleBase() baseData {
	return baseData{
		Title:         "Test",
		Nav:           "instances",
		CSRF:          "test-token",
		User:          &models.User{ID: 1, Username: "admin", Role: "admin"},
		Version:       "test",
		IncusSocket:   "/var/lib/incus/unix.socket",
		InstanceCount: 2,
		Jobs: []models.Job{{
			ID:       "abc123",
			Kind:     "backup.create",
			Target:   "wp-example",
			Status:   "running",
			Progress: 40,
			Message:  "Exporting the archive",
		}},
	}
}

func sampleInstance() instances.Instance {
	return instances.Instance{
		Name:        "wp-example",
		Kind:        "container",
		Status:      "Running",
		Running:     true,
		IP:          "10.10.0.21",
		IPs:         []string{"10.10.0.21", "fd42::21"},
		Image:       "ubuntu/24.04",
		ImageLabel:  "Ubuntu 24.04 LTS",
		CPU:         2,
		MemoryBytes: 4 << 30,
		DiskBytes:   50 << 30,
		StoragePool: "default",
		CreatedAt:   time.Now().Add(-72 * time.Hour),
		StartedAt:   time.Now().Add(-2 * time.Hour),
		Owner:       "admin",
		PrimaryHost: "example.com",
		Notes:       "Customer site",
		CPUPercent:  12.5,
		MemUsed:     1 << 30,
		DiskUsed:    8 << 30,
		NetRx:       1 << 20,
		NetTx:       2 << 20,
		Processes:   42,
		Tracked:     true,
		Domains: []models.Domain{
			{ID: 1, InstanceName: "wp-example", Domain: "example.com", Port: 80},
		},
	}
}

func sampleSeries() (monitoring.Series, monitoring.Series) {
	now := time.Now()
	samples := make([]models.Metric, 0, 30)
	for i := 0; i < 30; i++ {
		samples = append(samples, models.Metric{
			InstanceName: "wp-example",
			TS:           now.Add(-time.Duration(30-i) * time.Minute),
			CPUPct:       float64(i%9) * 3.5,
			MemUsed:      int64(1<<30) + int64(i)*int64(1<<26),
			MemTotal:     4 << 30,
			DiskUsed:     8 << 30,
			DiskTotal:    50 << 30,
		})
	}
	return monitoring.PercentSeries(samples), monitoring.MemorySeries(samples)
}

func TestRenderEveryPage(t *testing.T) {
	renderer := newTestRenderer(t)
	inst := sampleInstance()
	cpuSeries, memSeries := sampleSeries()

	host := &monitoring.Host{
		CPUTotal:     8,
		MemoryTotal:  32 << 30,
		MemoryUsed:   9 << 30,
		StorageName:  "default",
		StorageUsed:  120 << 30,
		StorageTotal: 1000 << 30,
		ServerName:   "incus-01",
		Version:      "6.23",
		Driver:       "lxc | qemu",
		Pools: []monitoring.Pool{
			{Name: "default", Driver: "zfs", Used: 120 << 30, Total: 1000 << 30},
		},
	}

	pool := poolView{Name: "default", Driver: "zfs", Used: 120 << 30, Total: 1000 << 30, Free: 880 << 30}
	pool.Instances = []instances.Instance{inst}

	filtered := sampleBase()
	filtered.Query = "nothing-matches"

	cases := []struct {
		page string
		data any
	}{
		{"dashboard", dashboardData{
			baseData:  sampleBase(),
			Instances: []instances.Instance{inst},
			Totals: instances.Totals{
				Instances: 1, Running: 1, VCPUs: 2, MemoryBytes: 4 << 30,
				DiskBytes: 50 << 30, Domains: 1, Snapshots: 3, Backups: 2, BackupBytes: 1 << 30,
			},
			Host:      host,
			Activity:  []models.Activity{{ID: 1, TS: time.Now(), Username: "admin", Action: "Create instance", Target: "wp-example", Status: "ok"}},
			S3Enabled: true,
		}},
		{"instances", instancesData{
			baseData:  sampleBase(),
			Instances: []instances.Instance{inst},
			Groups:    []poolGroup{{Name: "default", Driver: "zfs", Instances: []instances.Instance{inst}}},
			Running:   1,
		}},
		{"instances", instancesData{
			baseData: filtered,
		}},
		{"create", createData{
			baseData:     sampleBase(),
			Platforms:    platformInfos(incus.DefaultCatalog()),
			Images:       incus.DefaultCatalog(),
			Sizes:        DefaultSizes(),
			Pools:        []poolInfo{{Name: "default", Driver: "zfs"}},
			DefaultImage: "ubuntu-24.04",
			Form:         createForm{Platform: "os", Image: "ubuntu-24.04", Size: DefaultSizes()[2].Value()},
		}},
		{"instance", instanceData{
			baseData:          sampleBase(),
			Inst:              &inst,
			Tab:               "overview",
			Tabs:              instanceTabs(),
			Window:            "hour",
			WindowLabel:       "hour",
			Windows:           windowOptions("hour"),
			SampleInterval:    60,
			CPUSeries:         cpuSeries,
			MemSeries:         memSeries,
			Images:            incus.DefaultCatalog(),
			Sizes:             DefaultSizes(),
			S3Enabled:         true,
			S3Bucket:          "incus-backups",
			CaddyEnabled:      true,
			SuggestedSnapshot: snapshots.SuggestName(time.Now()),
			SuggestedBackup:   backups.SuggestName("wp-example", time.Now()),
			Snapshots: []snapshots.Snapshot{{
				Name: "snap-20261007-1200", InstanceName: "wp-example",
				CreatedAt: time.Now().Add(-time.Hour), Note: "before update", CreatedBy: "admin", Tracked: true,
			}},
			Backups: []backups.Backup{{
				Name: "wp-example-20261007-1200", InstanceName: "wp-example", Target: "s3",
				Status: "complete", SizeBytes: 512 << 20, S3Key: "peaceful-cloud/wp-example/x.tar.gz",
				CreatedAt: time.Now().Add(-2 * time.Hour), CreatedBy: "admin",
				LocalPath: "/data/backups/x.tar.gz", LocalExists: true, InS3: true,
			}},
			Domains: []domains.Record{{
				ID: 1, Domain: "example.com", InstanceName: "wp-example",
				Port: 80, Upstream: "10.10.0.21:80", Reachable: true, CreatedAt: time.Now(),
			}},
			Activity: []models.Activity{{ID: 1, TS: time.Now(), Username: "admin", Action: "Reboot instance", Status: "ok"}},
			Disks:    []diskView{{Name: "data", Type: "disk", Pool: "default", Source: "vol-1", Path: "/var/www"}},
			Networks: []networkView{{Name: "eth0", Type: "nic", Network: "incusbr0", MAC: "00:16:3e:aa:bb:cc", State: "up"}},
		}},
		{"snapshots", snapshotsData{
			baseData: sampleBase(),
			Snapshots: []snapshots.Snapshot{{
				Name: "snap-20261007-1200", InstanceName: "wp-example",
				CreatedAt: time.Now().Add(-time.Hour), Note: "before update",
			}},
		}},
		{"backups", backupsData{
			baseData:   sampleBase(),
			S3Enabled:  true,
			S3Bucket:   "incus-backups",
			TotalBytes: 512 << 20,
			Backups: []backups.Backup{{
				Name: "wp-example-20261007-1200", InstanceName: "wp-example",
				Status: "complete", SizeBytes: 512 << 20, LocalExists: true, InS3: true,
				CreatedAt: time.Now(),
			}},
		}},
		{"backups", backupsData{baseData: sampleBase()}},
		{"domains", domainsData{
			baseData: sampleBase(),
			Domains: []domains.Record{{
				ID: 1, Domain: "example.com", InstanceName: "wp-example",
				Port: 80, Upstream: "10.10.0.21:80", Reachable: true, CreatedAt: time.Now(),
			}},
			CaddyPreview: "example.com {\n\treverse_proxy 10.10.0.21:80\n}\n",
			CaddyEnabled: true,
			PublicIP:     "203.0.113.10",
		}},
		{"storage", storageData{
			baseData: sampleBase(),
			Host:     &hostView{Host: host, Pools: []poolView{pool}},
		}},
		{"networking", networkingData{
			baseData: sampleBase(),
			Networks: []networkInfo{{
				Name: "incusbr0", Type: "bridge", Managed: "yes", Address: "10.10.0.1/24", UsedBy: 1,
			}},
			Instances:    []instances.Instance{inst},
			CaddyEnabled: true,
			CaddyPath:    "/etc/caddy/Caddyfile",
		}},
		{"activity", activityData{
			baseData: sampleBase(),
			Activity: []models.Activity{{ID: 1, TS: time.Now(), Username: "admin", Action: "Create instance", Target: "wp-example", Status: "ok"}},
			Total:    120,
			Page:     2,
			Pages:    3,
			Limit:    50,
			Jobs:     []models.Job{{ID: "abc", Kind: "instance.create", Target: "x", Status: "done", CreatedBy: "admin", CreatedAt: time.Now()}},
		}},
		{"settings", settingsData{
			baseData:          sampleBase(),
			Host:              host,
			Users:             []models.User{{ID: 1, Username: "admin", Role: "admin"}},
			S3Configured:      true,
			S3Bucket:          "incus-backups",
			S3Endpoint:        "s3.example.com",
			S3Region:          "us-east-1",
			S3Prefix:          "peaceful-cloud",
			S3AccessKeyMasked: "AKI••••••••23",
			CaddyEnabled:      true,
			CaddyPath:         "/etc/caddy/Caddyfile",
			CaddyPreview:      "example.com {\n\treverse_proxy 10.10.0.21:80\n}\n",
			SessionCount:      1,
			SessionTTL:        168,
			MetricsInterval:   60,
			MetricsRetention:  168,
			JobWorkers:        3,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.page, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderer.Execute(&buf, tc.page, tc.data); err != nil {
				t.Fatalf("render %s: %v", tc.page, err)
			}
			out := buf.String()
			if !strings.Contains(out, "</html>") {
				t.Errorf("page %s produced incomplete HTML", tc.page)
			}
			if strings.Contains(out, "{{") {
				t.Errorf("page %s contains an unrendered template action", tc.page)
			}
		})
	}
}

// TestRenderInstanceTabs checks every tab partial, plus the HTMX fragments.
func TestRenderInstanceTabs(t *testing.T) {
	renderer := newTestRenderer(t)
	inst := sampleInstance()

	for _, tab := range instanceTabs() {
		tab := tab
		t.Run(tab.ID, func(t *testing.T) {
			cpuSeries, memSeries := sampleSeries()
			data := instanceData{
				baseData:          sampleBase(),
				Inst:              &inst,
				Tab:               tab.ID,
				Tabs:              instanceTabs(),
				Window:            "hour",
				WindowLabel:       "hour",
				Windows:           windowOptions("hour"),
				SampleInterval:    60,
				CPUSeries:         cpuSeries,
				MemSeries:         memSeries,
				Images:            incus.DefaultCatalog(),
				Sizes:             DefaultSizes(),
				S3Enabled:         true,
				S3Bucket:          "b",
				CaddyEnabled:      true,
				SuggestedSnapshot: "snap-20261007-1200",
				SuggestedBackup:   "wp-example-20261007-1200",
				Snapshots: []snapshots.Snapshot{{
					Name: "s1", InstanceName: inst.Name, CreatedAt: time.Now(), Tracked: true,
				}},
				Backups: []backups.Backup{{
					Name: "b1", InstanceName: inst.Name, Status: "complete", LocalExists: true,
					CreatedAt: time.Now(),
				}},
				Domains: []domains.Record{{
					ID: 3, Domain: "example.com", InstanceName: inst.Name, Upstream: "10.10.0.21:80", Reachable: true,
				}},
				Activity: []models.Activity{{ID: 1, TS: time.Now(), Username: "admin", Action: "Reboot instance", Status: "ok"}},
				Disks:    []diskView{{Name: "data", Pool: "default", Source: "v1", Path: "/data"}},
				Networks: []networkView{{Name: "eth0", Network: "incusbr0", MAC: "00:11:22:33:44:55", State: "up"}},
			}

			var buf bytes.Buffer
			if err := renderer.ExecutePartial(&buf, "instance", "tab_"+tab.ID, data); err != nil {
				t.Fatalf("render tab %s: %v", tab.ID, err)
			}
			if buf.Len() == 0 {
				t.Errorf("tab %s rendered nothing", tab.ID)
			}
		})
	}
}

func TestRenderHTMXFragments(t *testing.T) {
	renderer := newTestRenderer(t)

	t.Run("job_list", func(t *testing.T) {
		var buf bytes.Buffer
		data := jobsData{Jobs: []models.Job{{ID: "1", Kind: "instance.create", Target: "x", Status: "running", Progress: 30}}}
		if err := renderer.ExecutePartial(&buf, "dashboard", "job_list", data); err != nil {
			t.Fatalf("render job list: %v", err)
		}
		if !strings.Contains(buf.String(), "job-tracker") {
			t.Error("job tracker container is missing")
		}
	})

	t.Run("job_list_empty", func(t *testing.T) {
		var buf bytes.Buffer
		if err := renderer.ExecutePartial(&buf, "dashboard", "job_list", jobsData{}); err != nil {
			t.Fatalf("render empty job list: %v", err)
		}
	})

	t.Run("blueprint_grid", func(t *testing.T) {
		var buf bytes.Buffer
		data := createData{
			baseData:     sampleBase(),
			Images:       incus.DefaultCatalog(),
			DefaultImage: "ubuntu-24.04",
			Form:         createForm{Image: "debian-13"},
		}
		if err := renderer.ExecutePartial(&buf, "create", "blueprint_grid", data); err != nil {
			t.Fatalf("render blueprints: %v", err)
		}
		if !strings.Contains(buf.String(), "debian-13") {
			t.Error("selected image missing from blueprint grid")
		}
	})
}

func TestRenderLoginPage(t *testing.T) {
	renderer := newTestRenderer(t)

	var buf bytes.Buffer
	if err := renderer.ExecuteStandalone(&buf, "templates/login.html", "login_page", loginData{
		Title: "Sign in", CSRF: "token", Error: "Incorrect username or password.",
	}); err != nil {
		t.Fatalf("render login: %v", err)
	}
	if !strings.Contains(buf.String(), "</html>") {
		t.Error("login page is incomplete")
	}
}
