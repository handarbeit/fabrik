package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The production funnelDNSResolver (#2425, ADR 2425). It answers one question:
// does this name have a usable address in the zone's own, authoritative DNS
// servers? It must not ask anything else, because every convenient resolver
// gives a wrong or delayed answer here:
//
//   - the system resolver on a tailnet host answers with the tailnet IP
//     (MagicDNS), which says nothing about the public name;
//   - a public recursive resolver caches a negative answer, so asking it can
//     delay its own success and keep a published name looking missing.
//
// So it walks the delegation itself: from the root servers it follows referrals
// (NS records and their glue) down to the zone's servers, with recursion
// desired off, and takes the answer only from a server that says it is
// authoritative. This file therefore uses no net.Lookup*, no net.Resolver and
// no recursive resolver address; funnelDNSExchange is the only network I/O and
// tailnet_dns_auth_test.go scans this file for those names.

const (
	funnelDNSQueryTimeout = 3 * time.Second
	funnelDNSMaxReferrals = 16 // delegation steps for one name
	funnelDNSMaxGlueless  = 2  // name servers resolved by a nested walk, per referral
	funnelDNSMaxDepth     = 3  // nested walks (for glueless name servers)
	funnelDNSMaxServers   = 6  // servers tried per step
)

// funnelDNSPort is the DNS port; a var so a test can serve on a free one.
var funnelDNSPort = "53"

// funnelDNSRootHints are the root servers' addresses (IANA root hints). The
// only constants the walk starts from; everything below the root is discovered.
var funnelDNSRootHints = []netip.Addr{
	netip.MustParseAddr("198.41.0.4"),     // a
	netip.MustParseAddr("170.247.170.2"),  // b
	netip.MustParseAddr("192.33.4.12"),    // c
	netip.MustParseAddr("199.7.91.13"),    // d
	netip.MustParseAddr("192.203.230.10"), // e
	netip.MustParseAddr("192.5.5.241"),    // f
	netip.MustParseAddr("192.112.36.4"),   // g
	netip.MustParseAddr("198.97.190.53"),  // h
	netip.MustParseAddr("192.36.148.17"),  // i
	netip.MustParseAddr("192.58.128.30"),  // j
	netip.MustParseAddr("193.0.14.129"),   // k
	netip.MustParseAddr("199.7.83.42"),    // l
	netip.MustParseAddr("202.12.27.33"),   // m
	netip.MustParseAddr("2001:503:ba3e::2:30"),
	netip.MustParseAddr("2001:500:2::c"),
	netip.MustParseAddr("2001:500:2f::f"),
	netip.MustParseAddr("2001:7fd::1"),
}

// funnelDNSExchange sends one DNS request to server and returns the raw reply.
// The production version speaks UDP and retries over TCP when the reply is
// truncated. Tests replace it with an in-process fake DNS tree.
var funnelDNSExchange = exchangeDNS

func exchangeDNS(ctx context.Context, server netip.Addr, req []byte) ([]byte, error) {
	addr := net.JoinHostPort(server.String(), funnelDNSPort)
	d := net.Dialer{Timeout: funnelDNSQueryTimeout}
	ctx, cancel := context.WithTimeout(ctx, funnelDNSQueryTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	_ = conn.Close()
	if err != nil {
		return nil, err
	}
	reply := buf[:n]
	if len(reply) < 12 || reply[2]&0x02 == 0 { // TC bit: the reply was truncated
		return reply, nil
	}

	tc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer tc.Close()
	_ = tc.SetDeadline(deadline)
	framed := make([]byte, 2+len(req))
	binary.BigEndian.PutUint16(framed, uint16(len(req)))
	copy(framed[2:], req)
	if _, err := tc.Write(framed); err != nil {
		return nil, err
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(tc, lenBuf[:]); err != nil {
		return nil, err
	}
	out := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
	if _, err := io.ReadFull(tc, out); err != nil {
		return nil, err
	}
	return out, nil
}

type authoritativeResolver struct{}

func newAuthoritativeResolver() *authoritativeResolver { return &authoritativeResolver{} }

// replyKind classifies one server's reply.
type replyKind int

const (
	replyAnswer   replyKind = iota // authoritative answer (possibly with no addresses)
	replyReferral                  // delegation to a deeper zone
)

type dnsReply struct {
	kind  replyKind
	addrs []netip.Addr // replyAnswer: the addresses of the queried type
	zone  string       // replyReferral: the delegated zone, lower-case FQDN
	ns    []string     // replyReferral: name servers, lower-case FQDNs
	glue  map[string][]netip.Addr
}

// Resolve implements funnelDNSResolver.
func (r *authoritativeResolver) Resolve(ctx context.Context, name string) (bool, error) {
	name = fqdn(name)
	if name == "." {
		return false, errors.New("empty name")
	}
	v4, servers, zone, err := r.walk(ctx, name, dnsmessage.TypeA, 0)
	if err != nil {
		return false, err
	}
	if anyPublicAddr(v4) {
		return true, nil
	}
	// The zone's servers are known; ask them for AAAA directly.
	rep, err := r.queryServers(ctx, servers, zone, name, dnsmessage.TypeAAAA)
	if err != nil {
		return false, err
	}
	if rep.kind != replyAnswer {
		return false, errors.New("authoritative servers answered an AAAA query with a referral")
	}
	return anyPublicAddr(rep.addrs), nil
}

// walk follows the delegation from the root to the zone that answers
// authoritatively for name, returning that answer's addresses, the servers that
// gave it and the zone they serve.
func (r *authoritativeResolver) walk(ctx context.Context, name string, qtype dnsmessage.Type, depth int) ([]netip.Addr, []netip.Addr, string, error) {
	servers := funnelDNSRootHints
	zone := "."
	for step := 0; step < funnelDNSMaxReferrals; step++ {
		rep, err := r.queryServers(ctx, servers, zone, name, qtype)
		if err != nil {
			return nil, nil, "", err
		}
		if rep.kind == replyAnswer {
			return rep.addrs, servers, zone, nil
		}
		next, err := r.referralServers(ctx, rep, depth)
		if err != nil {
			return nil, nil, "", err
		}
		servers, zone = next, rep.zone
	}
	return nil, nil, "", fmt.Errorf("more than %d delegation steps resolving %s", funnelDNSMaxReferrals, name)
}

// referralServers returns the addresses of a referral's name servers: glue
// first, then (up to funnelDNSMaxGlueless) a nested walk for a name server with
// no glue.
func (r *authoritativeResolver) referralServers(ctx context.Context, rep dnsReply, depth int) ([]netip.Addr, error) {
	var out []netip.Addr
	var glueless []string
	for _, ns := range rep.ns {
		if addrs := rep.glue[ns]; len(addrs) > 0 {
			out = append(out, addrs...)
		} else {
			glueless = append(glueless, ns)
		}
	}
	if len(out) == 0 {
		if depth >= funnelDNSMaxDepth {
			return nil, fmt.Errorf("zone %s: name servers have no glue and the lookup is too deep", rep.zone)
		}
		for i, ns := range glueless {
			if i >= funnelDNSMaxGlueless {
				break
			}
			addrs, _, _, err := r.walk(ctx, ns, dnsmessage.TypeA, depth+1)
			if err != nil {
				continue
			}
			out = append(out, addrs...)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("zone %s: no name server address found", rep.zone)
	}
	return out, nil
}

// queryServers asks servers, in turn, until one gives a valid reply. zone is the
// zone they serve (the bailiwick a reply must stay within).
func (r *authoritativeResolver) queryServers(ctx context.Context, servers []netip.Addr, zone, name string, qtype dnsmessage.Type) (dnsReply, error) {
	var lastErr error
	tried := 0
	for _, s := range servers {
		if tried >= funnelDNSMaxServers {
			break
		}
		if ctx.Err() != nil {
			return dnsReply{}, ctx.Err()
		}
		tried++
		rep, err := queryOne(ctx, s, zone, name, qtype)
		if err == nil {
			return rep, nil
		}
		lastErr = fmt.Errorf("%s: %w", s, err)
	}
	if lastErr == nil {
		lastErr = errors.New("no name servers to ask")
	}
	return dnsReply{}, fmt.Errorf("no usable reply from the servers of zone %s for %s: %w", zone, name, lastErr)
}

// queryOne sends one non-recursive question and validates the reply.
func queryOne(ctx context.Context, server netip.Addr, zone, name string, qtype dnsmessage.Type) (dnsReply, error) {
	qname, err := dnsmessage.NewName(name)
	if err != nil {
		return dnsReply{}, fmt.Errorf("bad name %q: %w", name, err)
	}
	id := uint16(rand.Uint32())
	req := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: false},
		Questions: []dnsmessage.Question{{Name: qname, Type: qtype, Class: dnsmessage.ClassINET}},
	}
	wire, err := req.Pack()
	if err != nil {
		return dnsReply{}, err
	}
	raw, err := funnelDNSExchange(ctx, server, wire)
	if err != nil {
		return dnsReply{}, err
	}
	var m dnsmessage.Message
	if err := m.Unpack(raw); err != nil {
		return dnsReply{}, fmt.Errorf("unparseable reply: %w", err)
	}
	return classifyReply(m, id, zone, name, qtype)
}

// classifyReply accepts a reply only if it is what an authoritative server (or
// the parent referring to one) would send. In particular a reply with the
// recursion bits set and no AA bit is a recursive resolver or an interceptor
// answering for a name it does not own, and is refused.
func classifyReply(m dnsmessage.Message, id uint16, zone, name string, qtype dnsmessage.Type) (dnsReply, error) {
	if !m.Response || m.ID != id {
		return dnsReply{}, errors.New("reply does not match the query")
	}
	if len(m.Questions) != 1 || !strings.EqualFold(m.Questions[0].Name.String(), name) || m.Questions[0].Type != qtype {
		return dnsReply{}, errors.New("reply question does not match the query")
	}
	if m.RCode != dnsmessage.RCodeSuccess && m.RCode != dnsmessage.RCodeNameError {
		return dnsReply{}, fmt.Errorf("rcode %s", m.RCode)
	}
	if m.Authoritative {
		return authoritativeAnswer(m, zone, name, qtype)
	}
	if m.RecursionAvailable {
		return dnsReply{}, errors.New("recursive-looking reply without the authoritative bit (not an authoritative server)")
	}
	return referral(m, zone, name)
}

// authoritativeAnswer reads the addresses of qtype for name from an AA reply,
// following a CNAME chain only while it stays inside zone.
func authoritativeAnswer(m dnsmessage.Message, zone, name string, qtype dnsmessage.Type) (dnsReply, error) {
	owner := name
	for hops := 0; hops < 4; hops++ {
		var next string
		var addrs []netip.Addr
		for _, rr := range m.Answers {
			if !strings.EqualFold(rr.Header.Name.String(), owner) {
				continue
			}
			switch b := rr.Body.(type) {
			case *dnsmessage.AResource:
				if qtype == dnsmessage.TypeA {
					addrs = append(addrs, netip.AddrFrom4(b.A))
				}
			case *dnsmessage.AAAAResource:
				if qtype == dnsmessage.TypeAAAA {
					addrs = append(addrs, netip.AddrFrom16(b.AAAA))
				}
			case *dnsmessage.CNAMEResource:
				next = strings.ToLower(b.CNAME.String())
			}
		}
		if len(addrs) > 0 || next == "" {
			return dnsReply{kind: replyAnswer, addrs: addrs}, nil
		}
		if !inZone(next, zone) {
			return dnsReply{}, fmt.Errorf("CNAME to %s leaves zone %s", next, zone)
		}
		owner = next
	}
	return dnsReply{}, errors.New("CNAME chain too long")
}

// referral reads a delegation from the authority section of a non-AA reply.
func referral(m dnsmessage.Message, zone, name string) (dnsReply, error) {
	if len(m.Answers) > 0 {
		return dnsReply{}, errors.New("non-authoritative reply carries answers")
	}
	rep := dnsReply{kind: replyReferral, glue: map[string][]netip.Addr{}}
	for _, rr := range m.Authorities {
		ns, ok := rr.Body.(*dnsmessage.NSResource)
		if !ok {
			continue
		}
		owner := strings.ToLower(rr.Header.Name.String())
		if !inZone(name, owner) || !strictlyDeeper(owner, zone) {
			return dnsReply{}, fmt.Errorf("referral to %s does not lead toward %s from %s", owner, name, zone)
		}
		if rep.zone != "" && rep.zone != owner {
			return dnsReply{}, errors.New("referral names more than one zone")
		}
		rep.zone = owner
		rep.ns = append(rep.ns, strings.ToLower(ns.NS.String()))
	}
	if len(rep.ns) == 0 {
		return dnsReply{}, errors.New("reply is neither an answer nor a referral")
	}
	isNS := map[string]bool{}
	for _, n := range rep.ns {
		isNS[n] = true
	}
	for _, rr := range m.Additionals {
		o := strings.ToLower(rr.Header.Name.String())
		if !isNS[o] {
			continue
		}
		switch b := rr.Body.(type) {
		case *dnsmessage.AResource:
			rep.glue[o] = append(rep.glue[o], netip.AddrFrom4(b.A))
		case *dnsmessage.AAAAResource:
			rep.glue[o] = append(rep.glue[o], netip.AddrFrom16(b.AAAA))
		}
	}
	return rep, nil
}

// fqdn lower-cases name and gives it a trailing dot.
func fqdn(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	return name
}

// inZone reports whether name is zone or a name below it (both lower-case FQDNs).
func inZone(name, zone string) bool {
	if zone == "." {
		return true
	}
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// strictlyDeeper reports whether zone is a proper descendant of parent.
func strictlyDeeper(zone, parent string) bool {
	return zone != parent && inZone(zone, parent)
}
