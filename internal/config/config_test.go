package config

import (
	"testing"
	"time"
)

func TestGetEnv(t *testing.T) {
	t.Run("returns value when set", func(t *testing.T) {
		t.Setenv("TEST_STR", "hello")
		if got := getEnv("TEST_STR", "fallback"); got != "hello" {
			t.Errorf("getEnv = %q, want hello", got)
		}
	})
	t.Run("empty value overrides fallback", func(t *testing.T) {
		// LookupEnv distinguishes set-but-empty from unset, so an explicitly
		// empty env var must win over the fallback.
		t.Setenv("TEST_STR", "")
		if got := getEnv("TEST_STR", "fallback"); got != "" {
			t.Errorf("getEnv = %q, want empty string", got)
		}
	})
	t.Run("fallback when unset", func(t *testing.T) {
		if got := getEnv("DEFINITELY_UNSET_VAR_XYZ", "fallback"); got != "fallback" {
			t.Errorf("getEnv = %q, want fallback", got)
		}
	})
}

func TestGetEnvInt(t *testing.T) {
	t.Run("valid int", func(t *testing.T) {
		t.Setenv("TEST_INT", "42")
		if got := getEnvInt("TEST_INT", 7); got != 42 {
			t.Errorf("getEnvInt = %d, want 42", got)
		}
	})
	t.Run("invalid int falls back", func(t *testing.T) {
		t.Setenv("TEST_INT", "notanint")
		if got := getEnvInt("TEST_INT", 7); got != 7 {
			t.Errorf("getEnvInt = %d, want fallback 7", got)
		}
	})
	t.Run("unset falls back", func(t *testing.T) {
		if got := getEnvInt("DEFINITELY_UNSET_INT_XYZ", 7); got != 7 {
			t.Errorf("getEnvInt = %d, want fallback 7", got)
		}
	})
}

func TestGetEnvBool(t *testing.T) {
	tests := []struct {
		val      string
		fallback bool
		want     bool
	}{
		{"true", false, true},
		{"false", true, false},
		{"1", false, true},
		{"0", true, false},
		{"invalid", true, true},   // unparseable -> fallback
		{"invalid", false, false}, // unparseable -> fallback
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("TEST_BOOL", tt.val)
			if got := getEnvBool("TEST_BOOL", tt.fallback); got != tt.want {
				t.Errorf("getEnvBool(%q, %v) = %v, want %v", tt.val, tt.fallback, got, tt.want)
			}
		})
	}

	t.Run("unset falls back", func(t *testing.T) {
		if got := getEnvBool("DEFINITELY_UNSET_BOOL_XYZ", true); got != true {
			t.Errorf("getEnvBool = %v, want fallback true", got)
		}
	})
}

func TestGetEnvDate(t *testing.T) {
	fallback := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("valid date parsed as UTC", func(t *testing.T) {
		t.Setenv("TEST_DATE", "2023-06-15")
		got := getEnvDate("TEST_DATE", fallback)
		want := time.Date(2023, 6, 15, 0, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("getEnvDate = %v, want %v", got, want)
		}
		if got.Location() != time.UTC {
			t.Errorf("getEnvDate location = %v, want UTC", got.Location())
		}
	})

	t.Run("invalid date falls back", func(t *testing.T) {
		t.Setenv("TEST_DATE", "15/06/2023")
		got := getEnvDate("TEST_DATE", fallback)
		if !got.Equal(fallback) {
			t.Errorf("getEnvDate = %v, want fallback %v", got, fallback)
		}
	})

	t.Run("unset falls back", func(t *testing.T) {
		got := getEnvDate("DEFINITELY_UNSET_DATE_XYZ", fallback)
		if !got.Equal(fallback) {
			t.Errorf("getEnvDate = %v, want fallback %v", got, fallback)
		}
	})
}
