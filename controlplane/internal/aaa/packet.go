// Package aaa implements the RADIUS server subscribers authenticate against.
//
// RADIUS rather than something modern because it is what the gear speaks. Every
// MikroTik, Ubiquiti, Cambium and OpenWrt box can be pointed at a RADIUS server
// and will then ask it who may connect and at what rate. That makes this the one
// integration point that works across all of them, and it is why a WISP's AAA
// server is the real centre of the network.
//
// RFC 2865 (authentication) and RFC 2866 (accounting) are implemented here
// directly. The protocol is small, and a hand-rolled implementation of it is a
// smaller liability than a dependency in the path of every subscriber login.
package aaa

import (
	// MD5 is not a choice. RADIUS specifies it for the response authenticator
	// and for password hiding, so interoperating means using it. This is why the
	// shared secret must be long and random, and why RADIUS belongs on a
	// management VLAN rather than a routed path.
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
)

// Code is a RADIUS packet code.
type Code byte

// Packet codes used here.
const (
	CodeAccessRequest      Code = 1
	CodeAccessAccept       Code = 2
	CodeAccessReject       Code = 3
	CodeAccountingRequest  Code = 4
	CodeAccountingResponse Code = 5
	CodeDisconnectRequest  Code = 40
	CodeCoARequest         Code = 43
)

func (c Code) String() string {
	switch c {
	case CodeAccessRequest:
		return "Access-Request"
	case CodeAccessAccept:
		return "Access-Accept"
	case CodeAccessReject:
		return "Access-Reject"
	case CodeAccountingRequest:
		return "Accounting-Request"
	case CodeAccountingResponse:
		return "Accounting-Response"
	case CodeDisconnectRequest:
		return "Disconnect-Request"
	case CodeCoARequest:
		return "CoA-Request"
	}
	return fmt.Sprintf("code(%d)", byte(c))
}

// AttrType is a RADIUS attribute type.
type AttrType byte

// Attributes used here, from the IANA RADIUS registry.
const (
	AttrUserName            AttrType = 1
	AttrUserPassword        AttrType = 2
	AttrCHAPPassword        AttrType = 3
	AttrNASIPAddress        AttrType = 4
	AttrNASPort             AttrType = 5
	AttrFramedIPAddress     AttrType = 8
	AttrFramedIPNetmask     AttrType = 9
	AttrReplyMessage        AttrType = 18
	AttrClass               AttrType = 25
	AttrVendorSpecific      AttrType = 26
	AttrSessionTimeout      AttrType = 27
	AttrIdleTimeout         AttrType = 28
	AttrCalledStationID     AttrType = 30
	AttrCallingStationID    AttrType = 31
	AttrNASIdentifier       AttrType = 32
	AttrAcctStatusType      AttrType = 40
	AttrAcctDelayTime       AttrType = 41
	AttrAcctInputOctets     AttrType = 42
	AttrAcctOutputOctets    AttrType = 43
	AttrAcctSessionID       AttrType = 44
	AttrAcctSessionTime     AttrType = 46
	AttrAcctInputPackets    AttrType = 47
	AttrAcctOutputPackets   AttrType = 48
	AttrAcctTerminateCause  AttrType = 49
	AttrAcctInputGigawords  AttrType = 52
	AttrAcctOutputGigawords AttrType = 53
	AttrAcctInterimInterval AttrType = 85
	AttrFramedIPv6Prefix    AttrType = 97
	AttrDelegatedIPv6Prefix AttrType = 123
)

// Acct-Status-Type values.
const (
	AcctStatusStart         uint32 = 1
	AcctStatusStop          uint32 = 2
	AcctStatusInterimUpdate uint32 = 3
	AcctStatusAccountingOn  uint32 = 7
	AcctStatusAccountingOff uint32 = 8
)

// VendorMikrotik is MikroTik's enterprise number. Its Rate-Limit attribute is
// how RouterOS learns a subscriber's speed from RADIUS, and RouterOS is the most
// common thing a small WISP puts at the edge.
const VendorMikrotik uint32 = 14988

// MikrotikRateLimit is Mikrotik-Rate-Limit.
const MikrotikRateLimit byte = 8

// Attribute is one type-length-value.
type Attribute struct {
	Type  AttrType
	Value []byte
}

// Packet is a RADIUS packet.
type Packet struct {
	Code          Code
	Identifier    byte
	Authenticator [16]byte
	Attributes    []Attribute
}

const (
	headerLen = 20
	// minLen and maxLen are from RFC 2865 section 3. Enforcing them keeps a
	// malformed or hostile datagram from becoming a large allocation.
	minLen = 20
	maxLen = 4096
)

// Parse decodes a RADIUS packet.
func Parse(buf []byte) (*Packet, error) {
	if len(buf) < minLen {
		return nil, fmt.Errorf("radius: packet is %d bytes, minimum is %d", len(buf), minLen)
	}
	declared := int(binary.BigEndian.Uint16(buf[2:4]))
	if declared < minLen || declared > maxLen {
		return nil, fmt.Errorf("radius: declared length %d is outside [%d, %d]",
			declared, minLen, maxLen)
	}
	if declared > len(buf) {
		return nil, fmt.Errorf("radius: declared length %d exceeds the %d bytes received",
			declared, len(buf))
	}
	// Trailing bytes past the declared length are ignored, as the RFC requires.
	buf = buf[:declared]

	p := &Packet{
		Code:       Code(buf[0]),
		Identifier: buf[1],
	}
	copy(p.Authenticator[:], buf[4:20])

	for pos := headerLen; pos < len(buf); {
		if pos+2 > len(buf) {
			return nil, fmt.Errorf("radius: truncated attribute header at offset %d", pos)
		}
		attrType := AttrType(buf[pos])
		attrLen := int(buf[pos+1])
		if attrLen < 2 {
			return nil, fmt.Errorf("radius: attribute %d at offset %d declares length %d",
				attrType, pos, attrLen)
		}
		if pos+attrLen > len(buf) {
			return nil, fmt.Errorf("radius: attribute %d at offset %d overruns the packet",
				attrType, pos)
		}
		value := make([]byte, attrLen-2)
		copy(value, buf[pos+2:pos+attrLen])
		p.Attributes = append(p.Attributes, Attribute{Type: attrType, Value: value})
		pos += attrLen
	}
	return p, nil
}

// Get returns the first value for an attribute type.
func (p *Packet) Get(t AttrType) ([]byte, bool) {
	for _, a := range p.Attributes {
		if a.Type == t {
			return a.Value, true
		}
	}
	return nil, false
}

// GetString returns an attribute as text.
func (p *Packet) GetString(t AttrType) string {
	v, ok := p.Get(t)
	if !ok {
		return ""
	}
	return string(v)
}

// GetUint32 returns a 4-byte integer attribute.
func (p *Packet) GetUint32(t AttrType) (uint32, bool) {
	v, ok := p.Get(t)
	if !ok || len(v) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(v), true
}

// GetUint64Octets combines an octet counter with its Gigawords companion.
//
// RADIUS counters are 32 bits, which wraps after 4 GiB. The Gigawords attribute
// carries the overflow, and ignoring it is a classic billing bug: a subscriber
// who transfers 5 GiB appears to have transferred 1.
func (p *Packet) GetUint64Octets(octets, gigawords AttrType) uint64 {
	low, _ := p.GetUint32(octets)
	high, _ := p.GetUint32(gigawords)
	return uint64(high)<<32 | uint64(low)
}

// Add appends an attribute.
func (p *Packet) Add(t AttrType, value []byte) {
	p.Attributes = append(p.Attributes, Attribute{Type: t, Value: value})
}

// AddString appends a text attribute, truncating to the 253-byte limit a single
// attribute can carry.
func (p *Packet) AddString(t AttrType, s string) {
	if len(s) > 253 {
		s = s[:253]
	}
	p.Add(t, []byte(s))
}

// AddUint32 appends an integer attribute.
func (p *Packet) AddUint32(t AttrType, v uint32) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	p.Add(t, b)
}

// AddIPv4 appends an IPv4 address attribute.
func (p *Packet) AddIPv4(t AttrType, addr netip.Addr) error {
	if !addr.Is4() {
		return fmt.Errorf("radius: attribute %d needs an IPv4 address, got %s", t, addr)
	}
	a := addr.As4()
	p.Add(t, a[:])
	return nil
}

// AddIPv6Prefix appends a prefix in the RFC 3162 / RFC 6911 encoding: a reserved
// byte, the prefix length, then only as many address bytes as the prefix needs.
func (p *Packet) AddIPv6Prefix(t AttrType, prefix netip.Prefix) error {
	if !prefix.Addr().Is6() {
		return fmt.Errorf("radius: attribute %d needs an IPv6 prefix, got %s", t, prefix)
	}
	bits := prefix.Bits()
	if bits < 0 || bits > 128 {
		return fmt.Errorf("radius: invalid prefix length %d", bits)
	}
	full := prefix.Addr().As16()
	nbytes := (bits + 7) / 8
	value := make([]byte, 2+nbytes)
	value[0] = 0 // reserved
	value[1] = byte(bits)
	copy(value[2:], full[:nbytes])
	p.Add(t, value)
	return nil
}

// AddVendor appends a Vendor-Specific attribute.
func (p *Packet) AddVendor(vendor uint32, vendorType byte, value []byte) {
	if len(value) > 247 {
		value = value[:247]
	}
	buf := make([]byte, 6+len(value))
	binary.BigEndian.PutUint32(buf[0:4], vendor)
	buf[4] = vendorType
	buf[5] = byte(2 + len(value))
	copy(buf[6:], value)
	p.Add(AttrVendorSpecific, buf)
}

// AddMikrotikRateLimit sets a subscriber's rate the way RouterOS expects.
//
// The field order is rx-rate/tx-rate, and rx is from the router's point of view,
// which means the first figure is the subscriber's upload and the second their
// download. Getting this backwards is easy and gives every subscriber their
// upload speed as a download limit.
func (p *Packet) AddMikrotikRateLimit(upKbps, downKbps int, burstUpKbps, burstDownKbps, burstSeconds int) {
	v := fmt.Sprintf("%dk/%dk", upKbps, downKbps)
	if burstUpKbps > upKbps && burstDownKbps > downKbps && burstSeconds > 0 {
		// rx-burst-rate/tx-burst-rate rx-burst-threshold/tx-burst-threshold
		// rx-burst-time/tx-burst-time. Threshold is conventionally set to the
		// committed rate, so the bucket refills whenever the subscriber is below it.
		v += fmt.Sprintf(" %dk/%dk %dk/%dk %d/%d",
			burstUpKbps, burstDownKbps, upKbps, downKbps, burstSeconds, burstSeconds)
	}
	p.AddVendor(VendorMikrotik, MikrotikRateLimit, []byte(v))
}

// encodeAttributes serialises the attribute list.
func (p *Packet) encodeAttributes() ([]byte, error) {
	var out []byte
	for _, a := range p.Attributes {
		if len(a.Value) > 253 {
			return nil, fmt.Errorf("radius: attribute %d value is %d bytes, maximum is 253",
				a.Type, len(a.Value))
		}
		out = append(out, byte(a.Type), byte(2+len(a.Value)))
		out = append(out, a.Value...)
	}
	return out, nil
}

// EncodeResponse serialises a reply and computes its Response Authenticator,
// which is MD5 over the response with the request's authenticator spliced in,
// followed by the shared secret (RFC 2865 section 3).
func (p *Packet) EncodeResponse(secret string, requestAuth [16]byte) ([]byte, error) {
	attrs, err := p.encodeAttributes()
	if err != nil {
		return nil, err
	}
	total := headerLen + len(attrs)
	if total > maxLen {
		return nil, fmt.Errorf("radius: response is %d bytes, maximum is %d", total, maxLen)
	}

	buf := make([]byte, headerLen, total)
	buf[0] = byte(p.Code)
	buf[1] = p.Identifier
	binary.BigEndian.PutUint16(buf[2:4], uint16(total))
	copy(buf[4:20], requestAuth[:])
	buf = append(buf, attrs...)

	h := md5.New()
	h.Write(buf)
	h.Write([]byte(secret))
	copy(buf[4:20], h.Sum(nil))
	return buf, nil
}

// VerifyAccountingRequest checks an Accounting-Request's authenticator, which is
// MD5 over the packet with the authenticator field zeroed, plus the secret.
//
// Access-Request packets have no equivalent check: their authenticator is a
// random nonce, so an authentication request cannot be authenticated at all
// before its password is decrypted. That is a genuine weakness of RADIUS and the
// reason to keep it off any path an attacker can reach.
func VerifyAccountingRequest(raw []byte, secret string) bool {
	if len(raw) < headerLen {
		return false
	}
	var received [16]byte
	copy(received[:], raw[4:20])

	scratch := make([]byte, len(raw))
	copy(scratch, raw)
	for i := 4; i < 20; i++ {
		scratch[i] = 0
	}

	h := md5.New()
	h.Write(scratch)
	h.Write([]byte(secret))
	computed := h.Sum(nil)

	var match byte
	for i := 0; i < 16; i++ {
		match |= received[i] ^ computed[i]
	}
	return match == 0
}

// DecryptPassword recovers the User-Password.
//
// RADIUS hides it by XOR against a keystream of chained MD5 digests of the
// secret (RFC 2865 section 5.2). It is obfuscation, not encryption: anyone with
// the secret and the packet recovers the password, and anyone who captures
// enough traffic can attack the secret offline.
func (p *Packet) DecryptPassword(secret string) (string, error) {
	cipher, ok := p.Get(AttrUserPassword)
	if !ok {
		return "", fmt.Errorf("radius: no User-Password attribute")
	}
	if len(cipher) == 0 || len(cipher)%16 != 0 || len(cipher) > 128 {
		return "", fmt.Errorf("radius: User-Password is %d bytes; must be a non-zero "+
			"multiple of 16 and at most 128", len(cipher))
	}

	out := make([]byte, 0, len(cipher))
	prev := p.Authenticator[:]
	for off := 0; off < len(cipher); off += 16 {
		h := md5.New()
		h.Write([]byte(secret))
		h.Write(prev)
		key := h.Sum(nil)

		block := cipher[off : off+16]
		for i := 0; i < 16; i++ {
			out = append(out, block[i]^key[i])
		}
		prev = block
	}
	// The password is null-padded to a multiple of 16.
	return strings.TrimRight(string(out), "\x00"), nil
}
