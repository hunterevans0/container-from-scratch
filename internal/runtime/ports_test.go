package runtime

// Tests for ports.go: parsing -p and showing published ports. For how Go
// tests work, see "Tests" in doc.go.

import "testing"

// TestParsePortMapping checks each form -p takes, and that malformed ones
// are refused.
func TestParsePortMapping(t *testing.T) {
	for _, test := range []struct {
		spec string
		want PortMapping
	}{
		{"8080:80", PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		{"127.0.0.1:8080:80", PortMapping{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		{"53:53/udp", PortMapping{HostPort: 53, ContainerPort: 53, Protocol: "udp"}},
		{"80", PortMapping{ContainerPort: 80, Protocol: "tcp"}},
		{"127.0.0.1::80/tcp", PortMapping{HostIP: "127.0.0.1", ContainerPort: 80, Protocol: "tcp"}},
		// 0.0.0.0 is every address, as no address is.
		{"0.0.0.0:8080:80", PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
	} {
		got, err := ParsePortMapping(test.spec)
		if err != nil {
			t.Errorf("ParsePortMapping(%q): %v", test.spec, err)
		} else if got != test.want {
			t.Errorf("ParsePortMapping(%q) = %+v; want %+v", test.spec, got, test.want)
		}
	}
	for _, spec := range []string{"", "80/sctp", "0:80", "8080:0", "8080:65536", "8000-8010:80", "web:80", "::1:8080:80", "a:b:c:d", "1.2.3.4:8080"} {
		if got, err := ParsePortMapping(spec); err == nil {
			t.Errorf("ParsePortMapping(%q) = %+v; want an error", spec, got)
		}
	}
}

// TestPortMappingString checks that ports show as docker ps shows them.
func TestPortMappingString(t *testing.T) {
	for _, test := range []struct {
		mapping PortMapping
		want    string
	}{
		{PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, "0.0.0.0:8080->80/tcp"},
		{PortMapping{HostIP: "127.0.0.1", HostPort: 5353, ContainerPort: 53, Protocol: "udp"}, "127.0.0.1:5353->53/udp"},
	} {
		if got := test.mapping.String(); got != test.want {
			t.Errorf("%+v.String() = %q; want %q", test.mapping, got, test.want)
		}
	}
}
