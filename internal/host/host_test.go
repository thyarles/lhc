package host

import "testing"

// Written from a real incident: three RKE2 nodes (k8s-node01/02/03) sit
// behind the shared name cluster-vip.example.com, and a resolver-derived name
// made all three report as that one name.

func resolver(name string) func() string { return func() string { return name } }

func TestLabel(t *testing.T) {
	cases := []struct {
		name, override, kernel, resolved, want string
	}{
		{"a shared VIP name never replaces the machine name", "", "web-node03", "cluster-vip.example.com", "web-node03"},
		{"node 1 behind one VIP keeps its name", "", "k8s-node01", "cluster-vip.example.com", "k8s-node01"},
		{"node 2 behind one VIP keeps its name", "", "k8s-node02", "cluster-vip.example.com", "k8s-node02"},
		{"node 3 behind one VIP keeps its name", "", "web-node03", "cluster-vip.example.com", "web-node03"},
		{"a matching FQDN is kept because it only adds a domain", "", "web01", "web01.example.com", "web01.example.com"},
		{"the match is case-insensitive", "", "web01", "WEB01.example.com", "WEB01.example.com"},
		{"an already-qualified kernel name is used verbatim", "", "web-node03.example.com", "WEB-NODE03.example.com", "web-node03.example.com"},
		{"a qualified kernel name ignores the resolver", "", "web-node03.example.com", "cluster-vip.example.com", "web-node03.example.com"},
		{"an unresolvable host falls back to the kernel name", "", "isolated-box", "isolated-box", "isolated-box"},
		{"an empty kernel name falls back to the resolved one", "", "", "somehow.example.com", "somehow.example.com"},
		{"the config override wins", "kpm03.prod.example.com", "web-node03", "cluster-vip.example.com", "kpm03.prod.example.com"},
		{"a blank override is no override", "   ", "web01", "web01.example.com", "web01.example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Label(c.override, c.kernel, resolver(c.resolved)); got != c.want {
				t.Fatalf("Label = %q, want %q", got, c.want)
			}
		})
	}
}

func TestQualifiedKernelNameNeverConsultsTheResolver(t *testing.T) {
	called := false
	Label("", "web-node03.example.com", func() string { called = true; return "" })
	if called {
		t.Fatal("resolver consulted for an already-qualified kernel name")
	}
}

func TestTheSameHostReportsTheSameNameWhoeverHoldsTheVIP(t *testing.T) {
	a := Label("", "web-node03.example.com", resolver("cluster-vip.example.com"))
	b := Label("", "web-node03.example.com", resolver("web-node03.example.com"))
	if a != b {
		t.Fatalf("identity drifted with the VIP: %q vs %q", a, b)
	}
}

func TestMailDomain(t *testing.T) {
	cases := []struct{ label, resolved, want string }{
		{"web-node03", "cluster-vip.example.com", "web-node03.example.com"},
		{"web01.example.com", "web01.example.com", "web01.example.com"},
		{"isolated-box", "isolated-box", "isolated-box"},
	}
	for _, c := range cases {
		if got := MailDomain(c.label, resolver(c.resolved)); got != c.want {
			t.Errorf("MailDomain(%q) = %q, want %q", c.label, got, c.want)
		}
	}
}

func TestParseOSRelease(t *testing.T) {
	got := ParseOSRelease([]byte("NAME=\"Rocky Linux\"\nVERSION_ID='9.4'\n# comment\nID=rocky\n"))
	if got["NAME"] != "Rocky Linux" || got["VERSION_ID"] != "9.4" || got["ID"] != "rocky" {
		t.Fatalf("got %v", got)
	}
}

func TestPkgManagerPreference(t *testing.T) {
	have := func(names ...string) func(string) (string, bool) {
		return func(n string) (string, bool) {
			for _, h := range names {
				if h == n {
					return "/usr/bin/" + n, true
				}
			}
			return "", false
		}
	}
	cases := []struct {
		have []string
		want string
	}{
		{[]string{"yum", "dnf"}, "dnf"},
		{[]string{"yum"}, "yum"},
		{[]string{"apt-get"}, "apt-get"},
		{[]string{"zypper"}, "zypper"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := PkgManager(have(c.have...)); got != c.want {
			t.Errorf("PkgManager(%v) = %q, want %q", c.have, got, c.want)
		}
	}
}
