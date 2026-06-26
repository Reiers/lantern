package main

import "testing"

// TestValidateGatewayScheme guards the #55 plain-http gateway protection.
func TestValidateGatewayScheme(t *testing.T) {
	cases := []struct {
		gw       string
		insecure bool
		wantErr  bool
	}{
		{"https://gateway.lantern.reiers.io", false, false}, // default, fine
		{"https://example.com/x", false, false},
		{"http://gateway.lantern.reiers.io", false, true}, // public http: refused
		{"http://gateway.lantern.reiers.io", true, false}, // explicit opt-in
		{"http://localhost:8080", false, false},           // loopback ok
		{"http://127.0.0.1:8080", false, false},           // loopback ok
		{"http://[::1]:8080", false, false},               // loopback ok
		{"ftp://nope.example.com", false, true},           // bad scheme
		{"://broken", false, true},                        // unparseable/bad scheme
	}
	for _, c := range cases {
		err := validateGatewayScheme(c.gw, c.insecure)
		if (err != nil) != c.wantErr {
			t.Errorf("validateGatewayScheme(%q, insecure=%v) err=%v, wantErr=%v",
				c.gw, c.insecure, err, c.wantErr)
		}
	}
}
