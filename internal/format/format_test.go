package format

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestFormats(t *testing.T) {
	d := decimal.RequireFromString
	cases := []struct{ got, want string }{
		{USD(d("8063")), "$8,063.00"},
		{USD(d("-196")), "-$196.00"},
		{USD(d("87.255")), "$87.26"},
		{USD(d("-0.001")), "$0.00"},
		{USD(d("999.995")), "$1,000.00"},
		{USD0(d("-13681.4")), "-$13,681"},
		{USD0(d("0.5")), "$1"},
		{Signed(d("591")), "+$591"},
		{Signed(d("-118.2")), "-$118"},
		{Signed(d("0.2")), "$0"},
		{Pct(d("0.53094")), "53.1%"},
		{Price(d("0.305")), "$0.31"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %s, want %s", c.got, c.want)
		}
	}
}

func TestSignedPriceAndPlural(t *testing.T) {
	d := decimal.RequireFromString
	for in, want := range map[string]string{"0.32": "+$0.32", "-3.005": "-$3.01", "0.001": "$0.00"} {
		if got := SignedPrice(d(in)); got != want {
			t.Errorf("SignedPrice(%s) = %s, want %s", in, got, want)
		}
	}
	if Plural(1, "alert") != "1 alert" || Plural(4, "defect") != "4 defects" {
		t.Error("plural")
	}
}
