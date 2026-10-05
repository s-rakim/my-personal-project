package aaa

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/s-rakim/my-personal-project/controlplane/internal/config"
	"github.com/s-rakim/my-personal-project/controlplane/internal/ipam"
	"github.com/s-rakim/my-personal-project/controlplane/internal/registry"
	"github.com/s-rakim/my-personal-project/controlplane/internal/secret"
	"github.com/s-rakim/my-personal-project/controlplane/internal/session"
	"github.com/s-rakim/my-personal-project/controlplane/internal/telemetry"
)

// Metric names exported by the AAA server.
const (
	MetricAuthRequests = "bswisp_radius_auth_requests_total"
	MetricAuthRejects  = "bswisp_radius_auth_rejects_total"
	MetricAcctRequests = "bswisp_radius_acct_requests_total"
	MetricMalformed    = "bswisp_radius_malformed_total"
	MetricUnknownNAS   = "bswisp_radius_unknown_nas_total"
)

type client struct {
	prefix netip.Prefix
	secret string
	name   string
}

// Server answers RADIUS authentication and accounting.
type Server struct {
	cfg      config.RADIUSConfig
	nodeID   string
	reg      *registry.Store
	leases   *ipam.Allocator
	sessions *session.Manager
	metrics  *telemetry.Registry
	log      *slog.Logger

	clients []client

	mu    sync.Mutex
	conns []*net.UDPConn
}

// New builds a server. Clients are parsed up front so a bad CIDR is a startup
// error rather than a silently ignored NAS.
func New(cfg config.Config, reg *registry.Store, leases *ipam.Allocator,
	sessions *session.Manager, metrics *telemetry.Registry, log *slog.Logger) (*Server, error) {

	s := &Server{
		cfg:      cfg.RADIUS,
		nodeID:   cfg.NodeID,
		reg:      reg,
		leases:   leases,
		sessions: sessions,
		metrics:  metrics,
		log:      log,
	}
	for _, c := range cfg.RADIUS.Clients {
		prefix, err := netip.ParsePrefix(c.CIDR)
		if err != nil {
			return nil, fmt.Errorf("aaa: client %q: %w", c.CIDR, err)
		}
		name := c.Name
		if name == "" {
			name = c.CIDR
		}
		s.clients = append(s.clients, client{prefix: prefix.Masked(), secret: c.Secret, name: name})
	}

	for _, m := range []struct {
		name, help string
	}{
		{MetricAuthRequests, "RADIUS Access-Request packets received"},
		{MetricAuthRejects, "RADIUS Access-Reject responses sent"},
		{MetricAcctRequests, "RADIUS Accounting-Request packets received"},
		{MetricMalformed, "RADIUS packets rejected as malformed"},
		{MetricUnknownNAS, "RADIUS packets from a source with no configured secret"},
	} {
		metrics.Define(m.name, telemetry.Counter, m.help)
	}
	return s, nil
}

// clientFor resolves a source address to its shared secret.
//
// A packet from an unconfigured source is dropped without a reply. Answering
// would confirm to a scanner that a RADIUS server lives here, and there is no
// legitimate sender that is not already in the config.
func (s *Server) clientFor(addr netip.Addr) (client, bool) {
	addr = addr.Unmap()
	// Most specific match wins, so a per-host entry can override a subnet.
	best := client{}
	found := false
	for _, c := range s.clients {
		if c.prefix.Contains(addr) {
			if !found || c.prefix.Bits() > best.prefix.Bits() {
				best = c
				found = true
			}
		}
	}
	return best, found
}

// ListenAndServe runs the authentication and accounting listeners until the
// context is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	auth, err := net.ListenPacket("udp", s.cfg.AuthListen)
	if err != nil {
		return fmt.Errorf("aaa: listen on %s: %w", s.cfg.AuthListen, err)
	}
	acct, err := net.ListenPacket("udp", s.cfg.AcctListen)
	if err != nil {
		auth.Close()
		return fmt.Errorf("aaa: listen on %s: %w", s.cfg.AcctListen, err)
	}

	authConn := auth.(*net.UDPConn)
	acctConn := acct.(*net.UDPConn)

	s.mu.Lock()
	s.conns = []*net.UDPConn{authConn, acctConn}
	s.mu.Unlock()

	s.log.Info("radius listening",
		"auth", s.cfg.AuthListen, "acct", s.cfg.AcctListen, "clients", len(s.clients))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.serve(ctx, authConn, s.handleAuth) }()
	go func() { defer wg.Done(); s.serve(ctx, acctConn, s.handleAcct) }()

	<-ctx.Done()
	authConn.Close()
	acctConn.Close()
	wg.Wait()
	return nil
}

type handler func(conn *net.UDPConn, from *net.UDPAddr, raw []byte)

func (s *Server) serve(ctx context.Context, conn *net.UDPConn, h handler) {
	buf := make([]byte, maxLen)
	for {
		if ctx.Err() != nil {
			return
		}
		// A read deadline is what lets this loop notice cancellation; a blocking
		// ReadFromUDP would otherwise hold the goroutine open until a packet
		// happened to arrive.
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("radius read failed", "error", err)
			continue
		}
		raw := make([]byte, n)
		copy(raw, buf[:n])
		h(conn, from, raw)
	}
}

func (s *Server) handleAuth(conn *net.UDPConn, from *net.UDPAddr, raw []byte) {
	s.metrics.Inc(MetricAuthRequests, nil)

	src, ok := netip.AddrFromSlice(from.IP)
	if !ok {
		return
	}
	cl, known := s.clientFor(src)
	if !known {
		s.metrics.Inc(MetricUnknownNAS, nil)
		s.log.Warn("radius auth from unknown NAS; dropped", "from", from.String())
		return
	}

	req, err := Parse(raw)
	if err != nil {
		s.metrics.Inc(MetricMalformed, nil)
		s.log.Warn("radius malformed auth packet", "from", from.String(), "error", err)
		return
	}
	if req.Code != CodeAccessRequest {
		s.log.Warn("radius unexpected code on auth port",
			"from", from.String(), "code", req.Code.String())
		return
	}

	username := req.GetString(AttrUserName)
	logAttrs := []any{"from", from.String(), "nas", cl.name, "username", username}

	reject := func(reason, subscriberFacing string) {
		s.metrics.Inc(MetricAuthRejects, nil)
		// The log gets the real reason; the subscriber's gear gets something
		// deliberately vague, so a Reply-Message cannot be used to enumerate
		// which usernames exist.
		s.log.Info("radius access rejected", append(logAttrs, "reason", reason)...)
		resp := &Packet{Code: CodeAccessReject, Identifier: req.Identifier}
		resp.AddString(AttrReplyMessage, subscriberFacing)
		s.send(conn, from, resp, cl.secret, req.Authenticator)
	}

	if username == "" {
		reject("no User-Name attribute", "Authentication failed")
		return
	}

	sub, found := s.reg.SubscriberByUsername(username)
	if !found {
		reject("no such subscriber", "Authentication failed")
		return
	}

	password, err := req.DecryptPassword(cl.secret)
	if err != nil {
		// CHAP would be the other option. It is not supported because it requires
		// storing recoverable passwords, which is a worse trade than requiring
		// PAP over a trusted management network.
		if _, isCHAP := req.Get(AttrCHAPPassword); isCHAP {
			reject("CHAP is not supported; configure the NAS for PAP",
				"Authentication method not supported")
			return
		}
		reject(fmt.Sprintf("cannot read password: %v", err), "Authentication failed")
		return
	}
	if sub.PasswordHash == "" {
		reject("account has no password set", "Authentication failed")
		return
	}
	okPass, err := secret.Verify(sub.PasswordHash, password)
	if err != nil {
		reject(fmt.Sprintf("stored hash is unreadable: %v", err), "Authentication failed")
		return
	}
	if !okPass {
		reject("wrong password", "Authentication failed")
		return
	}
	if sub.Suspended {
		// A distinct message here is deliberate: the account is real and the
		// subscriber should be told to call billing rather than retype a password.
		reject("account suspended", "Account suspended; please contact support")
		return
	}

	// Pick the terminal. The NAS reports the CPE's MAC in Calling-Station-Id, so
	// a subscriber with several terminals gets the right one; otherwise fall back
	// to their only enabled terminal.
	callingID := req.GetString(AttrCallingStationID)
	term, ok := s.resolveTerminal(sub.ID, callingID)
	if !ok {
		reject("subscriber has no enabled terminal in inventory",
			"No service configured for this account")
		return
	}

	lease, err := s.leases.Lease(term.ID)
	if err != nil {
		// Persisting failed but the lease is live. Serving the subscriber is the
		// right call; the error is logged loudly because a restart would lose it.
		s.log.Error("radius lease problem", append(logAttrs, "error", err)...)
		if !lease.V4.IsValid() {
			reject(fmt.Sprintf("address allocation failed: %v", err),
				"Service temporarily unavailable")
			return
		}
	}

	snap := s.reg.Snapshot()
	plan, planOK := snap.Plans[sub.Plan]
	if !planOK {
		reject(fmt.Sprintf("plan %q is missing from inventory", sub.Plan),
			"No service configured for this account")
		return
	}

	sess := s.sessions.Start(term.ID, sub.ID, lease)

	resp := &Packet{Code: CodeAccessAccept, Identifier: req.Identifier}
	if err := resp.AddIPv4(AttrFramedIPAddress, lease.V4); err != nil {
		s.log.Error("radius cannot encode address", append(logAttrs, "error", err)...)
	}
	// /32 with CGNAT: the CPE's only neighbour is the gateway, so a wider mask
	// would just invite subscribers to try to reach each other.
	_ = resp.AddIPv4(AttrFramedIPNetmask, netip.MustParseAddr("255.255.255.255"))

	if lease.V6.IsValid() {
		// Delegated-IPv6-Prefix is what a CPE asks for by DHCPv6-PD and then
		// subnets behind itself. Framed-IPv6-Prefix is sent alongside it because
		// some gear only looks at one of the two.
		if err := resp.AddIPv6Prefix(AttrDelegatedIPv6Prefix, lease.V6); err != nil {
			s.log.Warn("radius cannot encode delegated prefix", append(logAttrs, "error", err)...)
		}
		_ = resp.AddIPv6Prefix(AttrFramedIPv6Prefix, lease.V6)
	}

	if s.cfg.SessionTimeoutSeconds > 0 {
		// Reauthentication is how a suspension actually takes effect on gear that
		// does not support RADIUS disconnect messages.
		resp.AddUint32(AttrSessionTimeout, uint32(s.cfg.SessionTimeoutSeconds))
	}
	if s.cfg.InterimIntervalSeconds > 0 {
		resp.AddUint32(AttrAcctInterimInterval, uint32(s.cfg.InterimIntervalSeconds))
	}

	// Class is echoed back verbatim in accounting packets, which is how an
	// Accounting-Request gets tied to a terminal without a second lookup.
	resp.AddString(AttrClass, "bswisp:"+s.nodeID+":"+term.ID)

	// The rate handed to the NAS is the live grant when the scheduler has made
	// one, and the plan ceiling before that. Sending the ceiling permanently would
	// let a subscriber ignore the airtime allocation entirely.
	downMbps, upMbps := plan.DownMbps, plan.UpMbps
	if sess.GrantDownMbps > 0 {
		downMbps = sess.GrantDownMbps
	}
	if sess.GrantUpMbps > 0 {
		upMbps = sess.GrantUpMbps
	}
	resp.AddMikrotikRateLimit(
		int(upMbps*1000), int(downMbps*1000),
		int(plan.UpMbps*1000), int(plan.BurstMbps*1000), int(plan.BurstSeconds),
	)

	s.log.Info("radius access accepted",
		append(logAttrs, "terminal", term.ID, "v4", lease.V4.String(),
			"v6", lease.V6.String(), "down_mbps", downMbps, "up_mbps", upMbps)...)
	s.send(conn, from, resp, cl.secret, req.Authenticator)
}

// resolveTerminal picks which CPE a login refers to.
func (s *Server) resolveTerminal(subscriberID, callingStationID string) (registry.Terminal, bool) {
	terms := s.reg.TerminalsOf(subscriberID)

	if callingStationID != "" {
		for _, t := range terms {
			if t.Enabled && (t.ID == callingStationID) {
				return t, true
			}
		}
	}
	// Deterministic fallback: the lowest-numbered enabled terminal, so repeated
	// logins from a multi-terminal account do not oscillate between them.
	var best registry.Terminal
	found := false
	for _, t := range terms {
		if !t.Enabled {
			continue
		}
		if !found || t.ID < best.ID {
			best = t
			found = true
		}
	}
	return best, found
}

func (s *Server) handleAcct(conn *net.UDPConn, from *net.UDPAddr, raw []byte) {
	s.metrics.Inc(MetricAcctRequests, nil)

	src, ok := netip.AddrFromSlice(from.IP)
	if !ok {
		return
	}
	cl, known := s.clientFor(src)
	if !known {
		s.metrics.Inc(MetricUnknownNAS, nil)
		return
	}

	// Unlike Access-Request, an Accounting-Request is authenticated, so verify it
	// before acting. Otherwise anyone who can reach the port can forge usage and
	// move a subscriber past their data cap.
	if !VerifyAccountingRequest(raw, cl.secret) {
		s.metrics.Inc(MetricMalformed, nil)
		s.log.Warn("radius accounting authenticator failed; dropped",
			"from", from.String(), "nas", cl.name)
		return
	}

	req, err := Parse(raw)
	if err != nil {
		s.metrics.Inc(MetricMalformed, nil)
		return
	}
	if req.Code != CodeAccountingRequest {
		return
	}

	status, _ := req.GetUint32(AttrAcctStatusType)
	acctSessionID := req.GetString(AttrAcctSessionID)
	terminalID := terminalFromClass(req.GetString(AttrClass))

	// Gigawords must be folded in or any subscriber past 4 GiB is undercounted.
	in := req.GetUint64Octets(AttrAcctInputOctets, AttrAcctInputGigawords)
	out := req.GetUint64Octets(AttrAcctOutputOctets, AttrAcctOutputGigawords)

	switch status {
	case AcctStatusStart:
		s.log.Info("radius accounting start",
			"terminal", terminalID, "session", acctSessionID, "nas", cl.name)
	case AcctStatusInterimUpdate:
		if terminalID != "" {
			s.sessions.SetAccounting(terminalID, acctSessionID, in, out)
		}
	case AcctStatusStop:
		if terminalID != "" {
			s.sessions.SetAccounting(terminalID, acctSessionID, in, out)
			s.sessions.Stop(terminalID)
		}
		cause, _ := req.GetUint32(AttrAcctTerminateCause)
		s.log.Info("radius accounting stop",
			"terminal", terminalID, "session", acctSessionID,
			"in_octets", in, "out_octets", out, "terminate_cause", cause)
	case AcctStatusAccountingOn:
		// The NAS rebooted. Every session it held is gone, but which ones they
		// were is not knowable from this packet, so sessions are left to time out
		// rather than dropping subscribers who may have already reconnected.
		s.log.Warn("radius NAS reports accounting-on; it restarted",
			"nas", cl.name, "from", from.String())
	case AcctStatusAccountingOff:
		s.log.Warn("radius NAS reports accounting-off; it is shutting down", "nas", cl.name)
	}

	resp := &Packet{Code: CodeAccountingResponse, Identifier: req.Identifier}
	s.send(conn, from, resp, cl.secret, req.Authenticator)
}

// terminalFromClass extracts the terminal ID from the Class attribute this
// server set at authentication time.
func terminalFromClass(class string) string {
	const prefix = "bswisp:"
	if len(class) <= len(prefix) || class[:len(prefix)] != prefix {
		return ""
	}
	rest := class[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ':' {
			return rest[i+1:]
		}
	}
	return ""
}

func (s *Server) send(conn *net.UDPConn, to *net.UDPAddr, p *Packet,
	sharedSecret string, requestAuth [16]byte) {

	raw, err := p.EncodeResponse(sharedSecret, requestAuth)
	if err != nil {
		s.log.Error("radius cannot encode response", "error", err)
		return
	}
	if _, err := conn.WriteToUDP(raw, to); err != nil {
		s.log.Warn("radius send failed", "to", to.String(), "error", err)
	}
}
