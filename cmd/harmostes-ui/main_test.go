package main

import "testing"

func TestResolveDaprEndpoint(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"explicit endpoint", map[string]string{"DAPR_HTTP_ENDPOINT": "http://dapr.internal:3500"}, "http://dapr.internal:3500"},
		{"injected port", map[string]string{"DAPR_HTTP_PORT": "3500"}, "http://127.0.0.1:3500"},
		{"neither (conventional default)", nil, "http://127.0.0.1:3500"},
		{"endpoint wins over port", map[string]string{"DAPR_HTTP_ENDPOINT": "http://e:1", "DAPR_HTTP_PORT": "2"}, "http://e:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"DAPR_HTTP_ENDPOINT", "DAPR_HTTP_PORT"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := resolveDaprEndpoint(); got != tc.want {
				t.Fatalf("resolveDaprEndpoint() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFixtureListenAddr(t *testing.T) {
	cases := []struct {
		mode bool
		in   string
		want string
	}{
		{true, ":8083", "127.0.0.1:8083"},            // the wrong default, narrowed
		{true, "127.0.0.1:18099", "127.0.0.1:18099"}, // explicit wins
		{true, "0.0.0.0:8083", "0.0.0.0:8083"},       // explicit all-interfaces is a choice
		{false, ":8083", ":8083"},                    // production untouched
	}
	for _, tc := range cases {
		if got := fixtureListenAddr(tc.mode, tc.in); got != tc.want {
			t.Errorf("fixtureListenAddr(%v, %q) = %q, want %q", tc.mode, tc.in, got, tc.want)
		}
	}
}
