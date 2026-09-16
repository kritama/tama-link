package catalog

import "testing"

func TestParseCountValueExponentForm(t *testing.T) {
	for _, tc := range []struct {
		name    string
		literal string
		want    int
		wantErr bool
	}{
		{name: "positive exponent", literal: "1e2", want: 100},
		{name: "fraction shifted to integer", literal: "1.5e1", want: 15},
		{name: "fractional spelling", literal: "15.0", want: 15},
		{name: "zero at exponent boundary", literal: "0e100000", want: 0},
		{name: "negative zero", literal: "-0e-100000", want: 0},
		{name: "fractional value", literal: "1e-100000", wantErr: true},
		{name: "negative value", literal: "-1", wantErr: true},
		{name: "above count bound", literal: "1e7", wantErr: true},
		{name: "outside exponent range", literal: "1e100001", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCountValue([]byte(tc.literal))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCountValue(%q) = %d, want error", tc.literal, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCountValue(%q): %v", tc.literal, err)
			}
			if got != tc.want {
				t.Fatalf("parseCountValue(%q) = %d, want %d", tc.literal, got, tc.want)
			}
		})
	}
}

func TestIsIntegerLiteralDoesNotAllocateByExponent(t *testing.T) {
	shortExponent := testing.AllocsPerRun(100, func() {
		_ = isIntegerLiteral([]byte("1e-1"))
	})
	boundaryExponent := testing.AllocsPerRun(100, func() {
		_ = isIntegerLiteral([]byte("1e-100000"))
	})
	if boundaryExponent > shortExponent+1 {
		t.Fatalf("boundary exponent allocated %.0f times, short exponent %.0f; allocation count must not scale with the exponent", boundaryExponent, shortExponent)
	}
}
