package service

import "testing"

func TestValidateAppSecret(t *testing.T) {
	for _, secret := range []string{"", "Panda", defaultJWTSecret, "<ganti_dengan_string_acak_panjang>", "                                "} {
		t.Setenv("JWT_SECRET", secret)
		if err := ValidateAppSecret(); err == nil {
			t.Errorf("unsafe JWT_SECRET must be rejected: %q", secret)
		}
	}
	t.Setenv("JWT_SECRET", "e81750d27fe5c4c6581ac9e5ecbf928d3a06c551b14a687bf2c404aabf19f450")
	if err := ValidateAppSecret(); err != nil {
		t.Fatal(err)
	}
}
