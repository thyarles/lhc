package paths

import "testing"

func env(root bool, existing ...string) Env {
	return Env{
		Root: root, Home: "/home/u", Executable: "/opt/lhc/lhc",
		Exists: func(p string) bool {
			for _, e := range existing {
				if e == p {
					return true
				}
			}
			return false
		},
	}
}

func TestConfigFileOrder(t *testing.T) {
	cases := []struct {
		name string
		flag string
		e    Env
		want string
	}{
		{"flag wins", "/x.yaml", env(true), "/x.yaml"},
		{"env var next", "", func() Env { e := env(true); e.LHCConfig = "/y.yaml"; return e }(), "/y.yaml"},
		{"root uses /etc", "", env(true), "/etc/lhc/config.yaml"},
		{"user default when nothing exists", "", env(false), "/home/u/.config/lhc/config.yaml"},
		{"user file when it exists", "", env(false, "/home/u/.config/lhc/config.yaml", "/etc/lhc/config.yaml"), "/home/u/.config/lhc/config.yaml"},
		{"beside the binary", "", env(false, "/opt/lhc/config.yaml"), "/opt/lhc/config.yaml"},
		{"user previewing a system install", "", env(false, "/etc/lhc/config.yaml"), "/etc/lhc/config.yaml"},
		{"XDG_CONFIG_HOME honoured", "", func() Env { e := env(false); e.XDGConfig = "/cfg"; return e }(), "/cfg/lhc/config.yaml"},
	}
	for _, c := range cases {
		if got := ConfigFile(c.flag, c.e); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDataPaths(t *testing.T) {
	root := Data(env(true), "", "", "")
	if root.State != "/var/lib/lhc" || root.Reports != "/var/lib/lhc/reports" || root.Log != "/var/log/lhc.log" {
		t.Errorf("root: %+v", root)
	}
	user := Data(env(false), "", "", "")
	if user.State != "/home/u/.local/state/lhc" || user.Log != "/home/u/.local/state/lhc/lhc.log" {
		t.Errorf("user: %+v", user)
	}
	over := Data(env(true), "/srv/lhc", "", "/tmp/l.log")
	if over.State != "/srv/lhc" || over.Reports != "/srv/lhc/reports" || over.Log != "/tmp/l.log" {
		t.Errorf("override: %+v", over)
	}
}
