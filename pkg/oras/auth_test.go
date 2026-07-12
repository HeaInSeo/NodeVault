package oras

import "testing"

func TestRegistryFromRepo(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"harbor.lab.local/library/mytool", "harbor.lab.local"},
		{"harbor.lab.local/library/mytool:latest", "harbor.lab.local"},
		{"harbor.lab.local", "harbor.lab.local"},
		{"10.96.0.1:5000/library/tool", "10.96.0.1:5000"},
	}
	for _, c := range cases {
		if got := registryFromRepo(c.in); got != c.want {
			t.Errorf("registryFromRepo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
