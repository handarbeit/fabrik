package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeDNS is an in-process DNS tree behind funnelDNSExchange. Every request is
// checked for RD=0 and a known destination (AC6, behaviourally): the walk asks
// only the root hints and addresses it was referred to.
type fakeDNS struct {
	t        *testing.T
	mu       sync.Mutex
	servers  map[netip.Addr]func(q dnsmessage.Message) dnsmessage.Message
	asked    []netip.Addr
	allowed  map[netip.Addr]bool // root hints + every address handed out in a referral
	failures map[netip.Addr]error
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func newFakeDNS(t *testing.T, roots ...string) *fakeDNS {
	t.Helper()
	f := &fakeDNS{
		t:        t,
		servers:  map[netip.Addr]func(dnsmessage.Message) dnsmessage.Message{},
		allowed:  map[netip.Addr]bool{},
		failures: map[netip.Addr]error{},
	}
	var hints []netip.Addr
	for _, r := range roots {
		hints = append(hints, addr(r))
		f.allowed[addr(r)] = true
	}
	origHints, origEx := funnelDNSRootHints, funnelDNSExchange
	funnelDNSRootHints = hints
	funnelDNSExchange = f.exchange
	t.Cleanup(func() { funnelDNSRootHints, funnelDNSExchange = origHints, origEx })
	return f
}

func (f *fakeDNS) serve(server string, h func(q dnsmessage.Message) dnsmessage.Message) {
	f.servers[addr(server)] = h
}

func (f *fakeDNS) exchange(_ context.Context, server netip.Addr, req []byte) ([]byte, error) {
	var q dnsmessage.Message
	if err := q.Unpack(req); err != nil {
		f.t.Errorf("unparseable request: %v", err)
		return nil, err
	}
	f.mu.Lock()
	f.asked = append(f.asked, server)
	allowed := f.allowed[server]
	fail := f.failures[server]
	h := f.servers[server]
	f.mu.Unlock()
	if q.RecursionDesired {
		f.t.Errorf("query to %s has RD set: a recursive query", server)
	}
	if !allowed {
		f.t.Errorf("query to %s, which is neither a root hint nor a referred address", server)
	}
	if fail != nil {
		return nil, fail
	}
	if h == nil {
		return nil, errors.New("connection refused")
	}
	resp := h(q)
	resp.ID = q.ID
	resp.Response = true
	resp.Questions = q.Questions
	// Everything a server hands out in a referral becomes a legitimate target.
	f.mu.Lock()
	for _, rr := range append(append([]dnsmessage.Resource(nil), resp.Additionals...), resp.Answers...) {
		switch b := rr.Body.(type) {
		case *dnsmessage.AResource:
			f.allowed[netip.AddrFrom4(b.A)] = true
		case *dnsmessage.AAAAResource:
			f.allowed[netip.AddrFrom16(b.AAAA)] = true
		}
	}
	f.mu.Unlock()
	return resp.Pack()
}

func (f *fakeDNS) askedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.asked) }

func dname(s string) dnsmessage.Name { return dnsmessage.MustNewName(s) }

func rrHeader(name string, t dnsmessage.Type) dnsmessage.ResourceHeader {
	return dnsmessage.ResourceHeader{Name: dname(name), Type: t, Class: dnsmessage.ClassINET, TTL: 60}
}

func nsRR(zone, ns string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(zone, dnsmessage.TypeNS), Body: &dnsmessage.NSResource{NS: dname(ns)}}
}

func aRR(name, ip string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypeA), Body: &dnsmessage.AResource{A: addr(ip).As4()}}
}

func aaaaRR(name, ip string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypeAAAA), Body: &dnsmessage.AAAAResource{AAAA: addr(ip).As16()}}
}

func cnameRR(name, target string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: rrHeader(name, dnsmessage.TypeCNAME), Body: &dnsmessage.CNAMEResource{CNAME: dname(target)}}
}

// referralTo answers with a delegation of zone to ns (glue: ns → ip when glueIP != "").
func referralTo(zone, ns, glueIP string) func(dnsmessage.Message) dnsmessage.Message {
	return func(dnsmessage.Message) dnsmessage.Message {
		m := dnsmessage.Message{Authorities: []dnsmessage.Resource{nsRR(zone, ns)}}
		if glueIP != "" {
			m.Additionals = []dnsmessage.Resource{aRR(ns, glueIP)}
		}
		return m
	}
}

// authZone is an authoritative server whose records are given per (name, type).
func authZone(rcode dnsmessage.RCode, records map[string][]dnsmessage.Resource) func(dnsmessage.Message) dnsmessage.Message {
	return func(q dnsmessage.Message) dnsmessage.Message {
		qq := q.Questions[0]
		m := dnsmessage.Message{Header: dnsmessage.Header{Authoritative: true, RCode: rcode}}
		// Like a real server: the records for the name, then the chain of CNAMEs.
		name := qq.Name.String()
		for hops := 0; hops < 4; hops++ {
			next := ""
			for _, rr := range records[name] {
				if rr.Header.Type == qq.Type || rr.Header.Type == dnsmessage.TypeCNAME {
					m.Answers = append(m.Answers, rr)
				}
				if c, ok := rr.Body.(*dnsmessage.CNAMEResource); ok {
					next = c.CNAME.String()
				}
			}
			if next == "" {
				break
			}
			name = next
		}
		return m
	}
}

const funnelName = "fabrik-models.tail1234.ts.net."

// tsTree builds root → net. → ts.net. (authoritative, serving recs).
func tsTree(t *testing.T, recs map[string][]dnsmessage.Resource) *fakeDNS {
	f := newFakeDNS(t, "10.0.0.1")
	f.serve("10.0.0.1", referralTo("net.", "a.gtld.test.", "10.0.0.2"))
	f.serve("10.0.0.2", referralTo("ts.net.", "ns1.dnsimple.test.", "10.0.0.3"))
	f.serve("10.0.0.3", authZone(dnsmessage.RCodeSuccess, recs))
	return f
}

func resolveOnce(t *testing.T, name string) (bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return newAuthoritativeResolver().Resolve(ctx, name)
}

func TestAuthResolver_PublicRecordResolves(t *testing.T) {
	tsTree(t, map[string][]dnsmessage.Resource{funnelName: {aRR(funnelName, "203.0.113.7")}})
	ok, err := resolveOnce(t, "fabrik-models.tail1234.ts.net")
	if err != nil || !ok {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

func TestAuthResolver_OnlyAAAAResolves(t *testing.T) {
	tsTree(t, map[string][]dnsmessage.Resource{funnelName: {aaaaRR(funnelName, "2606:4700::1111")}})
	if ok, err := resolveOnce(t, funnelName); err != nil || !ok {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// The case the issue describes: NODATA from the authoritative server is not an
// error, and not resolved.
func TestAuthResolver_NODATAIsUnresolvedNotAnError(t *testing.T) {
	tsTree(t, map[string][]dnsmessage.Resource{})
	ok, err := resolveOnce(t, funnelName)
	if err != nil || ok {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

func TestAuthResolver_NXDOMAINIsUnresolvedNotAnError(t *testing.T) {
	f := newFakeDNS(t, "10.0.0.1")
	f.serve("10.0.0.1", referralTo("net.", "a.gtld.test.", "10.0.0.2"))
	f.serve("10.0.0.2", referralTo("ts.net.", "ns1.dnsimple.test.", "10.0.0.3"))
	f.serve("10.0.0.3", authZone(dnsmessage.RCodeNameError, nil))
	ok, err := resolveOnce(t, funnelName)
	if err != nil || ok {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// AC2: records that resolve only to tailnet addresses are not resolved.
func TestAuthResolver_TailnetOnlyRecordsAreNotResolved(t *testing.T) {
	tsTree(t, map[string][]dnsmessage.Resource{funnelName: {
		aRR(funnelName, "100.101.102.103"),
		aaaaRR(funnelName, "fd7a:115c:a1e0::1234"),
	}})
	ok, err := resolveOnce(t, funnelName)
	if err != nil || ok {
		t.Fatalf("tailnet-only answer counted as resolved: %v, err = %v", ok, err)
	}
}

// A tailnet record next to a public one counts as resolved.
func TestAuthResolver_MixedRecordsResolve(t *testing.T) {
	tsTree(t, map[string][]dnsmessage.Resource{funnelName: {
		aRR(funnelName, "100.101.102.103"), aRR(funnelName, "198.51.100.9"),
	}})
	if ok, err := resolveOnce(t, funnelName); err != nil || !ok {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// A name server delegated without glue is found by a nested walk.
func TestAuthResolver_GluelessNameServer(t *testing.T) {
	f := newFakeDNS(t, "10.0.0.1")
	f.serve("10.0.0.1", referralTo("net.", "a.gtld.test.", "10.0.0.2"))
	f.serve("10.0.0.2", func(q dnsmessage.Message) dnsmessage.Message {
		qq := q.Questions[0]
		if qq.Name.String() == "ns.net." { // the server is itself authoritative for net.
			return authZone(dnsmessage.RCodeSuccess, map[string][]dnsmessage.Resource{"ns.net.": {aRR("ns.net.", "10.0.0.3")}})(q)
		}
		return referralTo("ts.net.", "ns.net.", "")(q) // no glue
	})
	f.serve("10.0.0.3", authZone(dnsmessage.RCodeSuccess, map[string][]dnsmessage.Resource{funnelName: {aRR(funnelName, "203.0.113.7")}}))
	if ok, err := resolveOnce(t, funnelName); err != nil || !ok {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// The first name server being down is not fatal; the next one is tried.
func TestAuthResolver_FallsThroughToTheNextNameServer(t *testing.T) {
	f := newFakeDNS(t, "10.0.0.1")
	f.serve("10.0.0.1", referralTo("net.", "a.gtld.test.", "10.0.0.2"))
	f.serve("10.0.0.2", func(dnsmessage.Message) dnsmessage.Message {
		return dnsmessage.Message{
			Authorities: []dnsmessage.Resource{nsRR("ts.net.", "ns1.test."), nsRR("ts.net.", "ns2.test.")},
			Additionals: []dnsmessage.Resource{aRR("ns1.test.", "10.0.0.8"), aRR("ns2.test.", "10.0.0.3")},
		}
	})
	f.serve("10.0.0.3", authZone(dnsmessage.RCodeSuccess, map[string][]dnsmessage.Resource{funnelName: {aRR(funnelName, "203.0.113.7")}}))
	f.failures[addr("10.0.0.8")] = errors.New("i/o timeout")
	if ok, err := resolveOnce(t, funnelName); err != nil || !ok {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// Every name server failing is an error (the probe reports unknown), not
// "unresolved".
func TestAuthResolver_AllNameServersFailingIsAnError(t *testing.T) {
	f := tsTree(t, map[string][]dnsmessage.Resource{})
	f.failures[addr("10.0.0.3")] = errors.New("i/o timeout")
	ok, err := resolveOnce(t, funnelName)
	if err == nil || ok {
		t.Fatalf("resolved = %v, err = %v; want an error", ok, err)
	}
}

func TestAuthResolver_NoRootReachableIsAnError(t *testing.T) {
	f := newFakeDNS(t, "10.0.0.1")
	f.failures[addr("10.0.0.1")] = errors.New("network is unreachable")
	if ok, err := resolveOnce(t, funnelName); err == nil || ok {
		t.Fatalf("resolved = %v, err = %v; want an error", ok, err)
	}
}

// A reply with the recursion bits and no AA bit (a recursive resolver, or a
// network that intercepts port 53) must not be taken as authoritative, even if
// it carries a perfectly good public address.
func TestAuthResolver_RecursiveLookingReplyIsRefused(t *testing.T) {
	f := newFakeDNS(t, "10.0.0.1")
	f.serve("10.0.0.1", func(q dnsmessage.Message) dnsmessage.Message {
		return dnsmessage.Message{
			Header:  dnsmessage.Header{RecursionDesired: true, RecursionAvailable: true},
			Answers: []dnsmessage.Resource{aRR(q.Questions[0].Name.String(), "203.0.113.7")},
		}
	})
	ok, err := resolveOnce(t, funnelName)
	if ok || err == nil {
		t.Fatalf("an intercepted answer was accepted: resolved = %v, err = %v", ok, err)
	}
}

// A non-AA reply carrying answers is not an authoritative answer either.
func TestAuthResolver_NonAuthoritativeAnswerIsRefused(t *testing.T) {
	f := newFakeDNS(t, "10.0.0.1")
	f.serve("10.0.0.1", func(q dnsmessage.Message) dnsmessage.Message {
		return dnsmessage.Message{Answers: []dnsmessage.Resource{aRR(q.Questions[0].Name.String(), "203.0.113.7")}}
	})
	if ok, err := resolveOnce(t, funnelName); ok || err == nil {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// A CNAME that leaves the zone cannot be verified from this server.
func TestAuthResolver_CNAMELeavingTheZoneIsRefused(t *testing.T) {
	tsTree(t, map[string][]dnsmessage.Resource{funnelName: {cnameRR(funnelName, "elsewhere.example.com.")}})
	if ok, err := resolveOnce(t, funnelName); ok || err == nil {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// A CNAME inside the zone is followed.
func TestAuthResolver_CNAMEInsideTheZoneIsFollowed(t *testing.T) {
	tsTree(t, map[string][]dnsmessage.Resource{
		funnelName:              {cnameRR(funnelName, "edge.tail1234.ts.net.")},
		"edge.tail1234.ts.net.": {aRR("edge.tail1234.ts.net.", "203.0.113.7")},
	})
	if ok, err := resolveOnce(t, funnelName); err != nil || !ok {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// A referral that does not lead toward the name (or goes back up) is refused,
// so a hostile or confused server cannot loop or redirect the walk.
func TestAuthResolver_MisdirectedReferralIsRefused(t *testing.T) {
	f := newFakeDNS(t, "10.0.0.1")
	f.serve("10.0.0.1", referralTo("example.org.", "ns.example.org.", "10.0.0.2"))
	if ok, err := resolveOnce(t, funnelName); ok || err == nil {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
}

// A referral loop ends at the step bound.
func TestAuthResolver_ReferralLoopIsBounded(t *testing.T) {
	f := newFakeDNS(t, "10.0.0.1")
	f.serve("10.0.0.1", referralTo("net.", "a.test.", "10.0.0.1"))
	if ok, err := resolveOnce(t, funnelName); ok || err == nil {
		t.Fatalf("resolved = %v, err = %v", ok, err)
	}
	if n := f.askedCount(); n > funnelDNSMaxReferrals+2 {
		t.Fatalf("asked %d times", n)
	}
}

// Real UDP and TCP on loopback: a truncated UDP reply is retried over TCP.
func TestExchangeDNS_TruncationFallsBackToTCP(t *testing.T) {
	uc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback udp: %v", err)
	}
	defer uc.Close()
	port := uc.LocalAddr().(*net.UDPAddr).Port
	tl, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Skipf("tcp not bindable on the udp port: %v", err)
	}
	defer tl.Close()

	full := []byte("FULL-REPLY-OVER-TCP-0123456789")
	go func() {
		buf := make([]byte, 512)
		n, from, err := uc.ReadFrom(buf)
		if err != nil || n < 12 {
			return
		}
		trunc := make([]byte, 12)
		copy(trunc, buf[:12])
		trunc[2] |= 0x82 // QR + TC
		_, _ = uc.WriteTo(trunc, from)
	}()
	go func() {
		c, err := tl.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var lb [2]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return
		}
		req := make([]byte, binary.BigEndian.Uint16(lb[:]))
		if _, err := io.ReadFull(c, req); err != nil {
			return
		}
		out := make([]byte, 2+len(full))
		binary.BigEndian.PutUint16(out, uint16(len(full)))
		copy(out[2:], full)
		_, _ = c.Write(out)
	}()

	origPort := funnelDNSPort
	funnelDNSPort = itoa(port)
	defer func() { funnelDNSPort = origPort }()
	got, err := exchangeDNS(context.Background(), addr("127.0.0.1"), make([]byte, 20))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(full) {
		t.Fatalf("reply = %q, want the TCP reply", got)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// AC6: the production resolver never reaches the system resolver or a
// recursive resolver. The walk starts at the root hints and the only network
// call is funnelDNSExchange (the behavioural half is fakeDNS.exchange, which
// every test above runs through: RD=0 and a known destination).
func TestFunnelDNSFilesNeverUseSystemOrRecursiveResolvers(t *testing.T) {
	forbidden := []string{
		"net.Lookup", "net.DefaultResolver", "net.Resolver", "LookupHost", "LookupIP", "LookupAddr",
		"LookupNS", "LookupCNAME", "LookupTXT", "LookupMX", "LookupSRV", "LookupPort",
		"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4", "9.9.9.9", "9.9.9.10", "149.112.112.112",
		"208.67.222.222", "208.67.220.220", "resolv.conf", "systemd-resolved",
	}
	for _, file := range []string{"tailnet_dns.go", "tailnet_dns_auth.go"} {
		bs, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(bs), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 {
				code = code[:j]
			}
			for _, bad := range forbidden {
				if strings.Contains(code, bad) {
					t.Errorf("%s:%d uses %q: the probe must ask the authoritative servers only", file, i+1, bad)
				}
			}
		}
	}
}

// The production factory builds the authoritative resolver (not a stub) outside
// tests: the init() in tailnet_dns_test.go replaces the variable, so check the
// constructor the variable's default points at.
func TestFunnelDNSResolverFactoryDefault(t *testing.T) {
	if _, ok := funnelDNSResolver(newAuthoritativeResolver()).(*authoritativeResolver); !ok {
		t.Fatal("newAuthoritativeResolver does not build an *authoritativeResolver")
	}
}
