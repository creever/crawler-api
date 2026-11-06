package handlers

import "testing"

func TestArgInt(t *testing.T) {
	args := map[string]interface{}{"days": float64(7), "bad": "x", "zero": float64(0)}
	if got := argInt(args, "days", 28); got != 7 {
		t.Errorf("argInt(days) = %d, want 7", got)
	}
	if got := argInt(args, "missing", 28); got != 28 {
		t.Errorf("argInt(missing) = %d, want default 28", got)
	}
	if got := argInt(args, "bad", 28); got != 28 {
		t.Errorf("argInt(bad) = %d, want default 28", got)
	}
	if got := argInt(args, "zero", 28); got != 28 {
		t.Errorf("argInt(zero) = %d, want default 28", got)
	}
}

func TestParsePositiveInt(t *testing.T) {
	if n, err := parsePositiveInt("10"); err != nil || n != 10 {
		t.Errorf("parsePositiveInt(10) = %d, %v", n, err)
	}
	if _, err := parsePositiveInt("0"); err == nil {
		t.Error("parsePositiveInt(0) should fail")
	}
	if _, err := parsePositiveInt("abc"); err == nil {
		t.Error("parsePositiveInt(abc) should fail")
	}
}
