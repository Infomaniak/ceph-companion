package cli

import "testing"

func TestResolveConfigPath(t *testing.T) {
	cases := []struct {
		name                string
		explicit            bool
		flagValue, envValue string
		want                string
	}{
		{"explicit flag wins over env", true, "/explicit/config.yaml", "/env/config.yaml", "/explicit/config.yaml"},
		{"explicit flag wins over default", true, "/explicit/config.yaml", "", "/explicit/config.yaml"},
		{"env beats default", false, "/etc/ceph-companion/config.yaml", "/env/config.yaml", "/env/config.yaml"},
		{"default when nothing else", false, "/etc/ceph-companion/config.yaml", "", "/etc/ceph-companion/config.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveConfigPath(tc.explicit, tc.flagValue, tc.envValue); got != tc.want {
				t.Errorf("resolveConfigPath(%v, %q, %q) = %q, want %q", tc.explicit, tc.flagValue, tc.envValue, got, tc.want)
			}
		})
	}
}
