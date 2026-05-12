package service

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaybeLiftMaxTokensFloor_RaisesLowValue(t *testing.T) {
	t.Setenv("SUB2API_KIRO_MAX_TOKENS_FLOOR", "")
	require.Equal(t, defaultKiroMaxTokensFloor, maybeLiftMaxTokensFloor(8000))
}

func TestMaybeLiftMaxTokensFloor_KeepsLargeValue(t *testing.T) {
	t.Setenv("SUB2API_KIRO_MAX_TOKENS_FLOOR", "")
	require.Equal(t, 64000, maybeLiftMaxTokensFloor(64000))
}

func TestMaybeLiftMaxTokensFloor_ZeroInputUntouched(t *testing.T) {
	// 0 / negative means "no limit" to some callers; we don't invent one.
	t.Setenv("SUB2API_KIRO_MAX_TOKENS_FLOOR", "")
	require.Equal(t, 0, maybeLiftMaxTokensFloor(0))
	require.Equal(t, -5, maybeLiftMaxTokensFloor(-5))
}

func TestMaybeLiftMaxTokensFloor_EnvOverride(t *testing.T) {
	t.Setenv("SUB2API_KIRO_MAX_TOKENS_FLOOR", "16000")
	require.Equal(t, 16000, maybeLiftMaxTokensFloor(8000))
	require.Equal(t, 20000, maybeLiftMaxTokensFloor(20000))
}

func TestMaybeLiftMaxTokensFloor_EnvZeroDisables(t *testing.T) {
	t.Setenv("SUB2API_KIRO_MAX_TOKENS_FLOOR", "0")
	require.Equal(t, 8000, maybeLiftMaxTokensFloor(8000))
}

func TestMaybeLiftMaxTokensFloor_EnvInvalidFallsBack(t *testing.T) {
	t.Setenv("SUB2API_KIRO_MAX_TOKENS_FLOOR", "garbage")
	require.Equal(t, defaultKiroMaxTokensFloor, maybeLiftMaxTokensFloor(8000))
}

func TestParseNonNegativeInt(t *testing.T) {
	cases := map[string]struct {
		want int
		err  bool
	}{
		"":      {0, true},
		"0":     {0, false},
		"8000":  {8000, false},
		"32000": {32000, false},
		"-1":    {0, true},
		"1.5":   {0, true},
		"abc":   {0, true},
	}
	for in, tc := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := parseNonNegativeInt(in)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	// Sanity: env var path uses os.Setenv-compatible reads.
	_ = os.Getenv
}
