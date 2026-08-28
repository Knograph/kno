package main

import (
	"math/big"
	"testing"
)

// TestNormalizationUnitsMatch pins the normalization contract: the same price
// expressed as a USD-per-token decimal string (OpenRouter), a USD-per-MTok
// page cell (the provider pages), and the schema's micro-USD-per-MTok must be
// THE SAME number. This is the test that keeps the three sources comparable.
func TestNormalizationUnitsMatch(t *testing.T) {
	t.Parallel()
	perToken, err := microsPerMTokFromUSDPerToken("0.000004")
	if err != nil {
		t.Fatalf("USD/token parse: %v", err)
	}
	perMTok, err := microsPerMTokFromUSDPerMTok("$4 / MTok")
	if err != nil {
		t.Fatalf("USD/MTok parse: %v", err)
	}
	want := big.NewRat(4_000_000, 1)
	if perToken.Cmp(want) != 0 {
		t.Errorf("USD/token 0.000004 = %s, want %s", perToken, want)
	}
	if perMTok.Cmp(want) != 0 {
		t.Errorf("USD/MTok $4/MTok = %s, want %s", perMTok, want)
	}
}

func TestMicrosPerMTokFromUSDPerToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int64 // micro-USD per MTok
	}{
		{"0.000004", 4_000_000},
		{"0.000002", 2_000_000},
		{"0.000000834", 834_000},
		{"2", 2_000_000_000_000},
		{"0", 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := microsPerMTokFromUSDPerToken(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Cmp(big.NewRat(tt.want, 1)) != 0 {
				t.Errorf("got %s, want %d", got, tt.want)
			}
		})
	}
	for _, bad := range []string{"", "abc", "$4"} {
		t.Run("bad "+bad, func(t *testing.T) {
			if _, err := microsPerMTokFromUSDPerToken(bad); err == nil {
				t.Errorf("parse of %q should fail", bad)
			}
		})
	}
}

func TestMicrosPerMTokFromUSDPerMTok(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int64
	}{
		{"$4 / MTok", 4_000_000},
		{"$12.50 / MTok", 12_500_000},
		{"$4.00", 4_000_000},
		{"5", 5_000_000},
		{"$1,000 / MTok", 1_000_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := microsPerMTokFromUSDPerMTok(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Cmp(big.NewRat(tt.want, 1)) != 0 {
				t.Errorf("got %s, want %d", got, tt.want)
			}
		})
	}
	for _, bad := range []string{"", "  ", "N/A", "—", "/ MTok", "$", "abc"} {
		t.Run("bad "+bad, func(t *testing.T) {
			if _, err := microsPerMTokFromUSDPerMTok(bad); err == nil {
				t.Errorf("parse of %q should fail", bad)
			}
		})
	}
}

func TestFormatUSDPerMTok(t *testing.T) {
	t.Parallel()
	tests := []struct {
		micros int64
		want   string
	}{
		{5_000_000, "$5.00"},
		{12_500_000, "$12.50"},
		{400_000, "$0.40"},
		{834_000, "$0.83"}, // display-only; never compared through this
	}
	for _, tt := range tests {
		if got := formatUSDPerMTok(big.NewRat(tt.micros, 1)); got != tt.want {
			t.Errorf("format(%d) = %q, want %q", tt.micros, got, tt.want)
		}
	}
}

func TestRatMul(t *testing.T) {
	t.Parallel()
	got := ratMul(big.NewRat(5_000_000, 1), big.NewRat(125, 100))
	if got.Cmp(big.NewRat(6_250_000, 1)) != 0 {
		t.Errorf("5M * 1.25 = %s, want 6.25M", got)
	}
}
