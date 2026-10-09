package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseLimits(t *testing.T) {
	post := func(v url.Values) (cpu, mem, disk *int, detail string, err error) {
		r := httptest.NewRequest("POST", "/instances/x/limits", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		lim, detail, err := parseLimits(r)
		return lim.CPU, lim.MemoryMB, lim.DiskGB, detail, err
	}

	cpu, mem, disk, detail, err := post(url.Values{"cpu": {"4"}, "memory_mb": {""}, "disk_gb": {"0"}})
	if err != nil {
		t.Fatal(err)
	}
	if cpu == nil || *cpu != 4 {
		t.Errorf("cpu = %v", cpu)
	}
	if mem != nil {
		t.Errorf("blank memory became %d: it must mean keep", *mem)
	}
	if disk == nil || *disk != 0 {
		t.Errorf("explicit 0 disk lost: %v", disk)
	}
	if !strings.Contains(detail, "cpu=4") || strings.Contains(detail, "memory") {
		t.Errorf("detail = %q", detail)
	}

	if _, _, _, _, err := post(url.Values{"cpu": {""}, "memory_mb": {" "}}); err == nil {
		t.Error("an entirely blank form was accepted")
	}
	if _, _, _, _, err := post(url.Values{"cpu": {"four"}}); err == nil {
		t.Error("non-numeric input was accepted")
	}
}
