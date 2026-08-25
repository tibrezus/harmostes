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
