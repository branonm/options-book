package occ

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
		"AAA   270115C00076000": "AAA 2027-01-15 call 76",
		"AAA270115C00076000":    "AAA 2027-01-15 call 76",
		"bbb   270617c00055000": "BBB 2027-06-17 call 55",
		"CCC   261218P00064500": "CCC 2026-12-18 put 64.5",
		"BRK.B 261218C00450000": "BRK.B 2026-12-18 call 450",
	} {
		c, err := Parse(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := c.Root + " " + c.Expiry + " " + c.Type + " " + c.Strike.String(); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "AAA $76c 2027-01-15", "AAA   271315C00076000", "AAA   270115X00076000", "AAA   270115C0076000", "TOOLONGX270115C00076000", "AAA   270115C00000000"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	c, _ := Parse("AAA270115C00076000")
	if c.String() != "AAA   270115C00076000" {
		t.Errorf("String: %q", c.String())
	}
	c, _ = Parse("CCC261218P00064500")
	if c.String() != "CCC   261218P00064500" {
		t.Errorf("String: %q", c.String())
	}
}
