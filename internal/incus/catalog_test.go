package incus

import "testing"

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"512MB", 512 * 1_000_000},
		{"512MiB", 512 * (1 << 20)},
		{"4GB", 4_000_000_000},
		{"4GiB", 4 << 30},
		{"1TB", 1_000_000_000_000},
		{"2048", 2048},
		{"50%", 0}, // percentages depend on the host
		{"bogus", 0},
	}

	for _, tc := range cases {
		if got := ParseBytes(tc.in); got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{4 << 30, "4.0 GiB"},
		{200 << 30, "200 GiB"},
	}

	for _, tc := range cases {
		if got := FormatBytes(tc.in); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatGB(t *testing.T) {
	if got := FormatGB(4 << 30); got != "4 GB" {
		t.Errorf("FormatGB(4GiB) = %q, want \"4 GB\"", got)
	}
	if got := FormatGB(0); got != "0 GB" {
		t.Errorf("FormatGB(0) = %q, want \"0 GB\"", got)
	}
	if got := FormatGB(512 << 20); got != "512 MiB" {
		t.Errorf("FormatGB(512MiB) = %q, want \"512 MiB\"", got)
	}
}

func TestParseCPUs(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"2", 2},
		{"16", 16},
		{"0-3", 4},
		{"0,1", 2},
		{"0,1,2", 3},
		{"bogus", 0},
	}

	for _, tc := range cases {
		if got := ParseCPUs(tc.in); got != tc.want {
			t.Errorf("ParseCPUs(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLookupImage(t *testing.T) {
	if _, ok := LookupImage("ubuntu-24.04"); !ok {
		t.Error("expected the ubuntu-24.04 blueprint to exist")
	}
	if _, ok := LookupImage("ubuntu/24.04"); !ok {
		t.Error("expected lookup by image alias to work")
	}
	if _, ok := LookupImage("nope"); ok {
		t.Error("expected an unknown blueprint to be rejected")
	}
}

func TestImageSource(t *testing.T) {
	ubuntu, _ := LookupImage("ubuntu-24.04")
	src := ubuntu.Source()

	if src.Type != "image" {
		t.Errorf("source type = %q, want \"image\"", src.Type)
	}
	if src.Alias != "ubuntu/24.04" {
		t.Errorf("alias = %q", src.Alias)
	}
	if src.Protocol != "simplestreams" {
		t.Errorf("protocol = %q, want simplestreams for a remote image", src.Protocol)
	}

	// A locally built application image must not specify a remote.
	wordpress, _ := LookupImage("wordpress")
	if wpSrc := wordpress.Source(); wpSrc.Server != "" {
		t.Errorf("local image should have no remote server, got %q", wpSrc.Server)
	}
}

func TestDefaultCatalogIsConsistent(t *testing.T) {
	seen := map[string]bool{}

	for _, img := range DefaultCatalog() {
		if img.ID == "" || img.Label == "" || img.Alias == "" {
			t.Errorf("image %+v is missing a required field", img)
		}
		if seen[img.ID] {
			t.Errorf("duplicate image id %q", img.ID)
		}
		seen[img.ID] = true

		if img.Kind != "container" && img.Kind != "virtual-machine" {
			t.Errorf("image %s has an invalid kind %q", img.ID, img.Kind)
		}
		if img.Category != "os" && img.Category != "app" {
			t.Errorf("image %s has an invalid category %q", img.ID, img.Category)
		}
	}
}
