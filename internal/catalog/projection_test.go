package catalog

import "testing"

func TestClip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"short unchanged", "abc", 5, "abc"},
		{"exact length unchanged", "abc...", 6, "abc..."},
		{"ascii cut", "abcdefgh", 5, "ab..."},
		{"two-byte rune dropped", "héllo", 5, "h..."},
		{"three-byte rune dropped", "€uro", 5, "..."},
		{"four-byte rune dropped", "𝄞x", 4, "..."},
		{"four-byte rune unchanged at bound", "𝄞x", 5, "𝄞x"},
		{"ascii before three-byte rune", "ab€", 4, "a..."},
		{"ascii at rune boundary", "ab€", 5, "ab€"},
		{"malformed input shrunk", "a\xffb", 2, ".."},
		{"marker length", "abcdef", 3, "..."},
		{"below marker", "abcdef", 2, ".."},
		{"one byte", "abcdef", 1, "."},
		{"zero bytes", "abcdef", 0, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := clip(test.s, test.n)
			if got != test.want {
				t.Fatalf("clip(%q, %d) = %q, want %q", test.s, test.n, got, test.want)
			}
			if len(got) > test.n {
				t.Fatalf("clip(%q, %d) = %q exceeds bound %d", test.s, test.n, got, test.n)
			}
		})
	}
}
