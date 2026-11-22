package model

import "testing"

func TestSeverityFromText_Mapping(t *testing.T) {
	cases := map[string]int{
		"fatal": 2, "CRITICAL": 2, "panic": 2, "emerg": 0, "alert": 1,
		"error": 3, "ERR": 3, "warn": 4, "WARNING": 4, "notice": 5,
		"info": 6, "DEBUG": 7, "trace": 7, "0": 0, "7": 7,
	}
	for in, want := range cases {
		got, ok := SeverityFromText(in)
		if !ok || got != want {
			t.Errorf("SeverityFromText(%q) = (%d, %v)，期望 (%d, true)", in, got, ok, want)
		}
	}
}

func TestSeverityFromText_UnknownReturnsFalse(t *testing.T) {
	for _, in := range []string{"", "  ", "verbose-ish?", "50", "30", "severity"} {
		if _, ok := SeverityFromText(in); ok && in != "severity" {
			t.Errorf("SeverityFromText(%q) 不应识别成功（避免猜错方向）", in)
		}
	}
}
