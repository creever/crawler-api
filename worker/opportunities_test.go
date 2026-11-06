package worker

import "testing"

func TestExpectedCTR(t *testing.T) {
	cases := []struct {
		pos      float64
		expected float64
	}{
		{1, 0.08},
		{3, 0.08},
		{4, 0.05},
		{5, 0.05},
		{6, 0.02},
		{10, 0.02},
		{11, 0.01},
		{20, 0.01},
	}
	for _, c := range cases {
		if got := expectedCTR(c.pos); got != c.expected {
			t.Errorf("expectedCTR(%v) = %v, want %v", c.pos, got, c.expected)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		expected string
	}{
		{"plain", `{"a": 1}`, `{"a": 1}`},
		{"fenced", "```json\n{\"a\": 1}\n```", `{"a": 1}`},
		{"fenced no lang", "```\n{\"a\": 1}\n```", `{"a": 1}`},
		{"with prose", "Here you go:\n{\"a\": 1}\nHope that helps", `{"a": 1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractJSON(c.raw); got != c.expected {
				t.Errorf("extractJSON(%q) = %q, want %q", c.raw, got, c.expected)
			}
		})
	}
}

func TestParseFloat(t *testing.T) {
	if got := parseFloat("12.5"); got != 12.5 {
		t.Errorf("parseFloat(12.5) = %v", got)
	}
	if got := parseFloat("0"); got != 0 {
		t.Errorf("parseFloat(0) = %v", got)
	}
	if got := parseFloat("not-a-number"); got != 0 {
		t.Errorf("parseFloat(garbage) = %v, want 0", got)
	}
}

func TestStripHTML(t *testing.T) {
	html := `<html><head><style>body{color:red}</style></head><body>
		<h1>Batz cipők</h1>
		<p>Kategória   leírás <a href="/batz/cipok">cipők</a> és <a href="/batz/kiegeszitok">kiegészítők</a>.</p>
		<script>alert(1)</script>
	</body></html>`
	got := stripHTML(html)
	want := "Batz cipők Kategória leírás cipők és kiegészítők."
	if got != want {
		t.Errorf("stripHTML = %q, want %q", got, want)
	}
}

func TestTruncateText(t *testing.T) {
	s := "aaaaabbbbb"
	if got := truncateText(s, 5); got != "aaaaa …" {
		t.Errorf("truncateText = %q", got)
	}
	if got := truncateText("abc", 10); got != "abc" {
		t.Errorf("truncateText short = %q", got)
	}
}
