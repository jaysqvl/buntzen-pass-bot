package twilio

import "testing"

func TestProductionEndpointCannotBeOverridden(t *testing.T) {
	valid := Config{AccountSID: testAccountSID, AuthToken: testAuthToken, ToNumber: testToNumber}
	if _, err := New(valid); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"https://unapproved.example", "http://127.0.0.1:1234", "https://api.twilio.com:444"} {
		candidate := valid
		candidate.BaseURL = endpoint
		if _, err := New(candidate); err == nil {
			t.Errorf("production admitted endpoint override %q", endpoint)
		}
	}
}
