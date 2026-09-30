package provider

import "testing"

func TestMaskKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"normal key", "sk-abc123def456", "sk-a****f456"},
		{"ends with digits", "sk-abc123456789", "sk-a****6789"},
		{"exactly 12 chars", "abcdefghijkl", "abcd****ijkl"},
		{"short key", "sk-abc", "****"},
		{"short key 11 chars", "abcdefghijk", "****"},
		{"empty", "", ""},
		{"spaces only", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MaskKey(c.in); got != c.want {
				t.Errorf("MaskKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
