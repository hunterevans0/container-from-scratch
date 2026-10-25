//go:build linux

package runtime

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

// Tests for dns_linux.go: the DNS server's answers, without the parts that
// need a container (its sockets in the container's namespace). The server
// is given a pretend list of containers, and pretend upstream servers on
// local ports. For how Go tests work, see "Tests" in doc.go.

// dnsQuery makes a query asking for records of kind for name, laid out as
// in the notes at the top of dns_linux.go: the header, then each label of
// the name after its length, a zero byte, the type, and the class.
func dnsQuery(id uint16, name string, kind uint16) []byte {
	query := binary.BigEndian.AppendUint16(nil, id)
	// RD, and one question.
	query = append(query, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0)
	for _, label := range strings.Split(name, ".") {
		query = append(query, byte(len(label)))
		query = append(query, label...)
	}
	query = append(query, 0)
	query = binary.BigEndian.AppendUint16(query, kind)
	return binary.BigEndian.AppendUint16(query, dnsClassIN)
}

// dnsResult is what the tests check of a reply.
type dnsResult struct {
	id      uint16
	rcode   int
	answers int
	// address is the last answer's address, if any.
	address string
}

// parseReply reads a reply's ID, response code, number of answers, and the
// last answer's address, which is the reply's last 4 bytes.
func parseReply(t *testing.T, reply []byte) dnsResult {
	t.Helper()
	if len(reply) < 12 || reply[2]&0x80 == 0 {
		t.Fatalf("not a DNS reply: %x", reply)
	}
	result := dnsResult{
		id:      binary.BigEndian.Uint16(reply),
		rcode:   int(reply[3] & 0x0f),
		answers: int(binary.BigEndian.Uint16(reply[6:])),
	}
	if result.answers > 0 {
		result.address = net.IP(reply[len(reply)-4:]).String()
	}
	return result
}

// TestParseDNSQuestion checks reading a query's question, and refusing
// messages that are short or aren't queries.
func TestParseDNSQuestion(t *testing.T) {
	question, err := parseDNSQuestion(dnsQuery(7, "Web.MyNet", dnsTypeA))
	if err != nil {
		t.Fatal(err)
	}
	if question.name != "web.mynet" || question.kind != dnsTypeA || question.class != dnsClassIN {
		t.Errorf("got %+v; want web.mynet, type A, class IN", question)
	}
	for _, query := range [][]byte{
		nil,
		dnsQuery(7, "web", dnsTypeA)[:15],
		// A reply, not a query.
		append([]byte{0, 7, 0x80}, dnsQuery(7, "web", dnsTypeA)[3:]...),
	} {
		if question, err := parseDNSQuestion(query); err == nil {
			t.Errorf("parseDNSQuestion(%x) = %+v; want an error", query, question)
		}
	}
}

// testDNSServer is a DNS server that knows one container, web at
// 10.89.0.2, on the network mynet, and passes other questions on to
// upstreams.
func testDNSServer(upstreams ...string) *dnsServer {
	return &dnsServer{
		network: "mynet",
		lookup: func(name string) net.IP {
			if name == "web" {
				return net.IP{10, 89, 0, 2}
			}
			return nil
		},
		upstreams: upstreams,
	}
}

// TestDNSAnswersContainerNames checks the server's own answers: a
// container's address, in any case and with the network's name after it,
// no answers for other types of record, and SERVFAIL when a question it
// can't answer has nowhere to go.
func TestDNSAnswersContainerNames(t *testing.T) {
	s := testDNSServer()
	for _, name := range []string{"web", "WEB", "web.mynet"} {
		// Structs can be compared with == when all their fields can; the
		// parentheses around the literal are needed in an if.
		got := parseReply(t, s.answer(dnsQuery(42, name, dnsTypeA), false))
		if got != (dnsResult{id: 42, answers: 1, address: "10.89.0.2"}) {
			t.Errorf("A for %s: got %+v; want 10.89.0.2", name, got)
		}
	}
	// The name exists, but has no IPv6 address.
	const typeAAAA = 28
	if got := parseReply(t, s.answer(dnsQuery(43, "web", typeAAAA), false)); got != (dnsResult{id: 43}) {
		t.Errorf("AAAA for web: got %+v; want success with no answers", got)
	}
	// Anything else would be passed on, but there are no servers to ask.
	if got := parseReply(t, s.answer(dnsQuery(44, "example.com", dnsTypeA), false)); got.rcode != dnsRcodeServFail {
		t.Errorf("A for example.com, with no servers to ask: got %+v; want SERVFAIL", got)
	}
	if reply := s.answer([]byte{1, 2, 3}, false); reply != nil {
		t.Errorf("reply to a malformed query: %x; want none", reply)
	}
}

// upstreamReply is what the test upstream servers answer: 192.0.2.1 for
// any question.
func upstreamReply(query []byte) []byte {
	question, err := parseDNSQuestion(query)
	if err != nil {
		return nil
	}
	return dnsReply(query, question, 0, []net.IP{{192, 0, 2, 1}})
}

// TestDNSForwardsOverUDP checks that a question about anything else goes to
// the host's servers, the next one if the first doesn't answer, and their
// reply comes back. The pretend upstream server listens on a free port
// (":0" lets the system pick) and answers in a goroutine.
func TestDNSForwardsOverUDP(t *testing.T) {
	upstream, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		buffer := make([]byte, 512)
		for {
			n, client, err := upstream.ReadFrom(buffer)
			if err != nil {
				return
			}
			upstream.WriteTo(upstreamReply(buffer[:n]), client)
		}
	}()
	// The first server doesn't answer: nothing listens there.
	s := testDNSServer("127.0.0.1:1", upstream.LocalAddr().String())
	got := parseReply(t, s.answer(dnsQuery(9, "example.com", dnsTypeA), false))
	if got != (dnsResult{id: 9, answers: 1, address: "192.0.2.1"}) {
		t.Errorf("got %+v; want the upstream's answer, 192.0.2.1", got)
	}
}

// TestDNSForwardsOverTCP checks the same over TCP, with its length before
// each message.
func TestDNSForwardsOverTCP(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if query, err := readDNSMessage(conn); err == nil {
			writeDNSMessage(conn, upstreamReply(query))
		}
	}()
	s := testDNSServer(upstream.Addr().String())
	got := parseReply(t, s.answer(dnsQuery(10, "example.com", dnsTypeA), true))
	if got != (dnsResult{id: 10, answers: 1, address: "192.0.2.1"}) {
		t.Errorf("got %+v; want the upstream's answer, 192.0.2.1", got)
	}
}
