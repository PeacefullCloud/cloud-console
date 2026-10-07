package web

import (
	"fmt"
	"os"
	"path/filepath"
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

// TestWriteDesignPreview renders every page to static HTML so the layout can be
// inspected in a browser without a running Incus daemon.
//
// It only writes files when CONSOLE_PREVIEW_DIR is set, so a normal `go test`
// run stays side-effect free:
//
//	CONSOLE_PREVIEW_DIR=/tmp/console-preview go test ./internal/web -run TestWriteDesignPreview
func TestWriteDesignPreview(t *testing.T) {
	outDir := os.Getenv("CONSOLE_PREVIEW_DIR")
	if outDir == "" {
		t.Skip("set CONSOLE_PREVIEW_DIR to write HTML previews")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("create preview dir: %v", err)
	}

	css, err := os.ReadFile(filepath.Join("..", "..", "static", "style.css"))
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "style.css"), css, 0o644); err != nil {
		t.Fatalf("write stylesheet: %v", err)
	}

	renderer := newTestRenderer(t)
	pages := previewPages()

	for _, preview := range pages {
		var sb strings.Builder

		var renderErr error
		if preview.file == "login" {
			renderErr = renderer.ExecuteStandalone(&sb, "templates/login.html", "login_page", preview.data)
		} else {
			renderErr = renderer.Execute(&sb, preview.page, preview.data)
		}
		if renderErr != nil {
			t.Fatalf("render %s: %v", preview.file, renderErr)
		}

		// Point at the sibling stylesheet instead of the /static/ route.
		html := strings.ReplaceAll(sb.String(), `href="/static/style.css"`, `href="style.css"`)
		html = strings.ReplaceAll(html, `src="/static/htmx.min.js"`, `src="htmx.min.js"`)
		html = strings.ReplaceAll(html, `src="/static/app.js"`, `src="app.js"`)

		path := filepath.Join(outDir, preview.file+".html")
		if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %s", path)
	}
}

// previewPage is one HTML file to write.
type previewPage struct {
	file string
	page string
	data any
}

// previewPages builds representative data for every screen.
func previewPages() []previewPage {
	base := sampleBase()
	list := previewInstances()
	inst := list[0]
	cpuSeries, memSeries := sampleSeries()

	host := &monitoring.Host{
		CPUTotal: 16, MemoryTotal: 64 << 30, MemoryUsed: 21 << 30,
		StorageName: "default", StorageUsed: 214 << 30, StorageTotal: 1000 << 30,
		ServerName: "incus-01", Version: "6.23", Driver: "lxc | qemu",
		Pools: []monitoring.Pool{
			{Name: "default", Driver: "zfs", Used: 214 << 30, Total: 1000 << 30},
		},
	}

	pool := poolView{Name: "default", Driver: "zfs", Used: 214 << 30, Total: 1000 << 30, Free: 786 << 30}
	pool.Instances = list

	activity := []models.Activity{
		{ID: 4, TS: time.Now().Add(-4 * time.Minute), Username: "admin", Action: "Reboot instance", Target: "wp-example", Status: "ok"},
		{ID: 3, TS: time.Now().Add(-46 * time.Minute), Username: "admin", Action: "Create snapshot", Target: "wp-example", Detail: "snap-20261007-1200", Status: "ok"},
		{ID: 2, TS: time.Now().Add(-3 * time.Hour), Username: "admin", Action: "Add domain", Target: "api-demo", Detail: "api.example.com", Status: "ok"},
		{ID: 1, TS: time.Now().Add(-26 * time.Hour), Username: "admin", Action: "Sign in", Target: "", Detail: "from 203.0.113.4", Status: "ok"},
	}

	instancePage := instanceData{
		baseData:          base,
		Inst:              &inst,
		Tab:               "overview",
		Tabs:              instanceTabs(),
		Window:            "hour",
		Windows:           windowOptions("hour"),
		WindowLabel:       "hour",
		SampleInterval:    60,
		CPUSeries:         cpuSeries,
		MemSeries:         memSeries,
		Images:            incus.DefaultCatalog(),
		Sizes:             DefaultSizes(),
		S3Enabled:         true,
		S3Bucket:          "incus-backups",
		CaddyEnabled:      true,
		SuggestedSnapshot: "snap-20261007-1730",
		SuggestedBackup:   "wp-example-20261007-1730",
		Snapshots: []snapshots.Snapshot{
			{Name: "snap-20261007-1200", InstanceName: inst.Name, CreatedAt: time.Now().Add(-5 * time.Hour), Note: "Before plugin update", CreatedBy: "admin", Tracked: true},
			{Name: "snap-20261006-0900", InstanceName: inst.Name, CreatedAt: time.Now().Add(-32 * time.Hour), Note: "Clean install", CreatedBy: "admin", Tracked: true},
		},
		Backups: []backups.Backup{
			{Name: "wp-example-20261007-0200", InstanceName: inst.Name, Target: "s3", Status: "complete", SizeBytes: 512 << 20, S3Key: "peaceful-cloud/wp-example/x.tar.gz", CreatedAt: time.Now().Add(-15 * time.Hour), CreatedBy: "admin", LocalPath: "/var/lib/peaceful-cloud/backups/x.tar.gz", LocalExists: true, InS3: true},
			{Name: "wp-example-20261005-0200", InstanceName: inst.Name, Target: "s3", Status: "complete", SizeBytes: 498 << 20, CreatedAt: time.Now().Add(-63 * time.Hour), CreatedBy: "admin", LocalPath: "/var/lib/peaceful-cloud/backups/y.tar.gz", LocalExists: true, InS3: true},
		},
		Domains: []domains.Record{
			{ID: 1, Domain: "example.com", InstanceName: inst.Name, Port: 80, Upstream: "10.10.0.21:80", Reachable: true, CreatedAt: time.Now().Add(-72 * time.Hour)},
			{ID: 2, Domain: "www.example.com", InstanceName: inst.Name, Port: 80, Upstream: "10.10.0.21:80", Reachable: true, CreatedAt: time.Now().Add(-70 * time.Hour)},
		},
		Activity: activity,
		ConsoleLog: []string{
			"… 128 earlier lines omitted",
			"systemd[1]: Started MariaDB 10.11 database server.",
			"systemd[1]: Started php8.3-fpm.service - The PHP 8.3 FastCGI Process Manager.",
			"caddy[142]: {\"level\":\"info\",\"msg\":\"serving initial configuration\"}",
			"systemd[1]: Reached target Multi-User System.",
		},
		Disks: []diskView{
			{Name: "data", Type: "disk", Pool: "default", Source: "wp-uploads", Path: "/var/www/uploads"},
		},
		Networks: []networkView{
			{Name: "eth0", Type: "nic", Network: "incusbr0", MAC: "00:16:3e:aa:bb:cc", State: "up"},
		},
	}

	caddyPreview := "example.com {\n\treverse_proxy 10.10.0.21:80\n}\n\napi.example.com {\n\treverse_proxy 10.10.0.22:80\n}\n"

	// One page per instance tab, so every panel can be inspected.
	tabs := make([]previewPage, 0, len(instanceTabs()))
	for _, tab := range instanceTabs() {
		data := instancePage
		data.Tab = tab.ID
		if tab.ID == "history" {
			data.Activity = activity
		}
		tabs = append(tabs, previewPage{
			file: "instance-" + tab.ID,
			page: "instance",
			data: data,
		})
	}

	pages := []previewPage{
		{file: "login", page: "login", data: loginData{Title: "Sign in", CSRF: "preview", Notice: "Sign in to manage Incus instances."}},
		{file: "dashboard", page: "dashboard", data: dashboardData{
			baseData:  base,
			Instances: list,
			Totals: instances.Totals{
				Instances: 3, Running: 2, Stopped: 1, VCPUs: 6,
				MemoryBytes: 10 << 30, DiskBytes: 130 << 30,
				Domains: 3, Snapshots: 4, Backups: 3, BackupBytes: 1500 << 20,
			},
			Host:      host,
			Activity:  activity,
			S3Enabled: true,
		}},
		{file: "instances", page: "instances", data: instancesData{
			baseData:  base,
			Instances: list,
			Groups:    []poolGroup{{Name: "default", Driver: "zfs", Instances: list}},
			Running:   2,
			Stopped:   1,
		}},
		{file: "create", page: "create", data: createData{
			baseData:     base,
			Platforms:    platformInfos(incus.DefaultCatalog()),
			Images:       incus.DefaultCatalog(),
			Sizes:        DefaultSizes(),
			Pools:        []poolInfo{{Name: "default", Driver: "zfs"}},
			DefaultImage: "ubuntu-24.04",
			Form: createForm{
				Platform: "os", Image: "ubuntu-24.04", Size: DefaultSizes()[2].Value(),
				StoragePool: "default", CPU: 2, MemoryMB: 2048, DiskGB: 40,
			},
		}},
		{file: "snapshots", page: "snapshots", data: snapshotsData{
			baseData: base,
			Snapshots: []snapshots.Snapshot{
				{Name: "snap-20261007-1200", InstanceName: "wp-example", CreatedAt: time.Now().Add(-5 * time.Hour), Note: "Before plugin update"},
				{Name: "snap-20261006-0900", InstanceName: "wp-example", CreatedAt: time.Now().Add(-32 * time.Hour), Note: "Clean install"},
				{Name: "snap-20261006-0830", InstanceName: "api-demo", CreatedAt: time.Now().Add(-33 * time.Hour), Note: "Before deploy"},
			},
		}},
		{file: "backups", page: "backups", data: backupsData{
			baseData:   base,
			S3Enabled:  true,
			S3Bucket:   "incus-backups",
			TotalBytes: 1500 << 20,
			Backups: []backups.Backup{
				{Name: "wp-example-20261007-0200", InstanceName: "wp-example", Status: "complete", SizeBytes: 512 << 20, CreatedAt: time.Now().Add(-15 * time.Hour), CreatedBy: "admin", LocalExists: true, InS3: true},
				{Name: "api-demo-20261006-0200", InstanceName: "api-demo", Status: "complete", SizeBytes: 220 << 20, CreatedAt: time.Now().Add(-39 * time.Hour), CreatedBy: "admin", LocalExists: true, InS3: true},
				{Name: "wp-example-20261005-0200", InstanceName: "wp-example", Status: "failed", SizeBytes: 0, CreatedAt: time.Now().Add(-63 * time.Hour), CreatedBy: "admin", Error: "upload failed: connection reset by peer"},
			},
		}},
		{file: "domains", page: "domains", data: domainsData{
			baseData: base,
			Domains: []domains.Record{
				{ID: 1, Domain: "example.com", InstanceName: "wp-example", Port: 80, Upstream: "10.10.0.21:80", Reachable: true, CreatedAt: time.Now().Add(-72 * time.Hour)},
				{ID: 2, Domain: "www.example.com", InstanceName: "wp-example", Port: 80, Upstream: "10.10.0.21:80", Reachable: true, CreatedAt: time.Now().Add(-70 * time.Hour)},
				{ID: 3, Domain: "api.example.com", InstanceName: "api-demo", Port: 80, Upstream: "10.10.0.22:80", Reachable: true, CreatedAt: time.Now().Add(-3 * time.Hour)},
			},
			CaddyPreview: caddyPreview,
			CaddyEnabled: true,
			PublicIP:     "203.0.113.10",
		}},
		{file: "storage", page: "storage", data: storageData{
			baseData: base,
			Host:     &hostView{Host: host, Pools: []poolView{pool}},
		}},
		{file: "networking", page: "networking", data: networkingData{
			baseData: base,
			Networks: []networkInfo{
				{Name: "incusbr0", Type: "bridge", Managed: "yes", Address: "10.10.0.1/24", UsedBy: 3},
				{Name: "lo", Type: "loopback", Managed: "no", Address: "127.0.0.1/8", UsedBy: 0},
			},
			Instances:    list,
			CaddyEnabled: true,
			CaddyPath:    "/etc/caddy/Caddyfile",
		}},
		{file: "activity", page: "activity", data: activityData{
			baseData: base,
			Activity: activity,
			Total:    4,
			Page:     1,
			Pages:    1,
			Limit:    50,
			Jobs: []models.Job{
				{ID: "9f2c", Kind: "instance.create", Target: "blog-demo", Status: "running", Progress: 55, Message: "Starting instance", CreatedBy: "admin", CreatedAt: time.Now().Add(-2 * time.Minute)},
				{ID: "7a11", Kind: "backup.create", Target: "wp-example", Status: "done", Progress: 100, Message: "Completed", CreatedBy: "admin", CreatedAt: time.Now().Add(-15 * time.Hour)},
			},
		}},
		{file: "settings", page: "settings", data: settingsData{
			baseData: base,
			Host:     host,
			Users: []models.User{
				{ID: 1, Username: "admin", Role: "admin"},
				{ID: 2, Username: "ops", Role: "operator"},
			},
			S3Configured:      true,
			S3Bucket:          "incus-backups",
			S3Endpoint:        "s3.eu-west-1.amazonaws.com",
			S3Region:          "eu-west-1",
			S3Prefix:          "peaceful-cloud",
			S3AccessKeyMasked: "AKI••••••••23",
			CaddyEnabled:      true,
			CaddyPath:         "/etc/caddy/Caddyfile",
			CaddyReloadCmd:    "caddy reload --config /etc/caddy/Caddyfile",
			CaddyPreview:      caddyPreview,
			SessionCount:      2,
			SessionTTL:        168,
			MetricsInterval:   60,
			MetricsRetention:  168,
			JobWorkers:        3,
		}},
	}

	return append(tabs, pages...)
}

// previewInstances returns three representative instances.
func previewInstances() []instances.Instance {
	now := time.Now()

	mk := func(name, imageLabel, status, ip, domain string, cpu int, memGB, diskGB int64, pct float64) instances.Instance {
		inst := instances.Instance{
			Name:        name,
			Kind:        "container",
			Status:      status,
			Running:     status == "Running",
			IP:          ip,
			IPs:         []string{ip},
			Image:       "ubuntu/24.04",
			ImageLabel:  imageLabel,
			CPU:         cpu,
			MemoryBytes: memGB << 30,
			DiskBytes:   diskGB << 30,
			StoragePool: "default",
			CreatedAt:   now.Add(-72 * time.Hour),
			Owner:       "admin",
			PrimaryHost: domain,
			CPUPercent:  pct,
			MemUsed:     memGB << 28,
			DiskUsed:    diskGB << 28,
			NetRx:       1 << 20,
			NetTx:       3 << 20,
			Processes:   42,
			Tracked:     true,
		}
		if inst.Running {
			inst.StartedAt = now.Add(-4 * time.Hour)
			inst.Domains = []models.Domain{{ID: 1, InstanceName: name, Domain: domain, Port: 80}}
		}
		return inst
	}

	return []instances.Instance{
		mk("wp-example", "WordPress", "Running", "10.10.0.21", "example.com", 2, 4, 50, 12.5),
		mk("api-demo", "Debian 13", "Running", "10.10.0.22", "api.example.com", 2, 2, 30, 4.1),
		mk("staging-blog", "Ubuntu 24.04 LTS", "Stopped", "", "", 1, 2, 20, 0),
	}
}

var _ = fmt.Sprintf
