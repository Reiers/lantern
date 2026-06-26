package main

import "testing"

// TestIsLoopbackListen guards the #56 non-loopback RPC bind protection.
func TestIsLoopbackListen(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1234", true},
		{"localhost:1234", true},
		{"[::1]:1234", true},
		{"0.0.0.0:1234", false},
		{":1234", false},
		{"192.168.1.10:1234", false},
		{"10.0.0.5:1234", false},
		{"example.com:1234", false}, // non-literal hostname: fail safe
		{"[::]:1234", false},
	}
	for _, c := range cases {
		if got := isLoopbackListen(c.addr); got != c.want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
