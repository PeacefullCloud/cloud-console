package instances

import "testing"

func TestImageLabelsAreLookedUpOncePerImage(t *testing.T) {
	var c imageLabelCache
	calls := 0
	lookup := func(fp string) string { calls++; return "label-" + fp }

	for i := 0; i < 10; i++ {
		if got := c.get("abc", lookup); got != "label-abc" {
			t.Fatalf("got %q", got)
		}
	}
	if got := c.get("def", lookup); got != "label-def" {
		t.Fatalf("got %q", got)
	}
	if calls != 2 {
		t.Errorf("%d lookups for 2 images", calls)
	}
}
