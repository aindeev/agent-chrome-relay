package relay

import "testing"

func TestCardKind(t *testing.T) {
	for _, c := range []struct {
		f    field
		want string
	}{
		{field{AC: "cc-number"}, "number"},
		{field{AC: "cc-csc"}, "cvc"},
		{field{AC: "cc-exp"}, "expiry"},
		{field{AC: "cc-exp-month"}, "expiry-month"},
		{field{AC: "cc-exp-year"}, "expiry-year"},
		{field{AC: "cc-name"}, "name"},
		{field{Hint: "cardnumber card number 1234 1234 1234 1234"}, "number"}, // Stripe's card element
		{field{Hint: "exp-date expiration date mm / yy"}, "expiry"},
		{field{Hint: "cvc cvc"}, "cvc"},
		{field{Hint: "expiry month"}, "expiry-month"},
		{field{Hint: "expiry year yy"}, "expiry-year"},
		{field{Hint: "name on card"}, "name"},
		{field{Hint: "email"}, ""},
		{field{Hint: "phone number"}, ""}, // a number, but not a card's
		{field{AC: "email", Hint: "email"}, ""},
	} {
		if got := cardKind(&c.f); got != c.want {
			t.Errorf("cardKind(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestPaymentHost(t *testing.T) {
	s := &Server{}
	for host, want := range map[string]bool{"js.stripe.com": true, "assets.braintreegateway.com": true, "checkoutshopper-live.adyen.com": true,
		"stripe.com": false, "evil-stripe.com": false, "js.stripe.com.evil.example": false, "example.com": false} {
		if got := s.paymentHost(host); got != want {
			t.Errorf("paymentHost(%q) = %v", host, got)
		}
	}
}
