# Setting up: home internet in the car

End state: devices join the Mango's Wi-Fi in the car, their traffic rides a 5G
link into a tunnel back to your house, and exits your home fibre with your home
IP address.

Budget about two hours the first time. The tunnel is the fiddly part; everything
after it is configuration pages.

```
devices ──wifi──▶ Mango ──5G──▶ carrier ──internet──▶ home router ──▶ fibre
                       └──────── WireGuard tunnel ────────┘
```

---

## Step 0 — find out whether you are behind CGNAT

Do this first. It decides which of two setups you need, and finding out later
means redoing the work.

On your home router, find the WAN IP address (often under Status or WAN). Then
from a browser on your home network, visit `whatismyip.com`.

| Result | What it means | Go to |
|---|---|---|
| The two match | You have a public IP | Step 1 |
| They differ | You are behind carrier-grade NAT; nothing can connect inbound | Step 1b |

If your WAN IP starts with `100.64.` through `100.127.`, that is CGNAT space and
the answer is definitely the second row.

---

## Step 1 — WireGuard server at home

Run this on whatever is always on: the router itself if it supports WireGuard
(OpenWrt, MikroTik, GL.iNet, most Ubiquiti), otherwise a Raspberry Pi on the LAN.

### Generate keys

```sh
sudo apt install wireguard
umask 077
wg genkey | sudo tee /etc/wireguard/server.key | wg pubkey | sudo tee /etc/wireguard/server.pub
wg genkey | tee car.key | wg pubkey > car.pub
```

Four files. The `.key` ones are secret; the `.pub` ones get pasted into the other
end's config.

### Server config

`/etc/wireguard/wg0.conf`, replacing `eth0` with your actual WAN interface
(`ip route get 1.1.1.1` will name it):

```ini
[Interface]
Address    = 10.8.0.1/24
ListenPort = 51820
PrivateKey = <contents of server.key>

# Let tunnel traffic out to the internet and NAT it behind the home IP.
PostUp   = sysctl -w net.ipv4.ip_forward=1; iptables -A FORWARD -i wg0 -j ACCEPT; iptables -t nat -A POSTROUTING -o eth0 -j MASQUERADE
PostDown = iptables -D FORWARD -i wg0 -j ACCEPT; iptables -t nat -D POSTROUTING -o eth0 -j MASQUERADE

[Peer]
# the car
PublicKey  = <contents of car.pub>
AllowedIPs = 10.8.0.2/32
```

Start it:

```sh
sudo systemctl enable --now wg-quick@wg0
sudo wg show
```

### Port forward

On your home router, forward **UDP 51820** to the machine running WireGuard.
UDP, not TCP; forwarding TCP is the single most common reason this step fails.

### Dynamic DNS

Residential IPs change. Set up a hostname so the car does not need to be
reconfigured each time: DuckDNS is free, and GL.iNet and most routers have DDNS
built in. You will use this hostname as the tunnel endpoint.

---

## Step 1b — if you are behind CGNAT

Inbound connections cannot reach you, so the car and your house both have to dial
*out* to a meeting point. Cheapest options, in order:

**Ask your ISP for a public IP.** Often free, usually trivial on a business plan.
Try this before anything else; it turns Step 1b back into Step 1.

**Otherwise, a $5 VPS as a rendezvous.** WireGuard runs on the VPS, and both your
home router and the car connect out to it.

VPS `/etc/wireguard/wg0.conf`:

```ini
[Interface]
Address    = 10.8.0.1/24
ListenPort = 51820
PrivateKey = <vps.key>
PostUp   = sysctl -w net.ipv4.ip_forward=1; iptables -A FORWARD -i wg0 -j ACCEPT
PostDown = iptables -D FORWARD -i wg0 -j ACCEPT

[Peer]
# home
PublicKey  = <home.pub>
AllowedIPs = 10.8.0.2/32, 192.168.1.0/24

[Peer]
# car
PublicKey  = <car.pub>
AllowedIPs = 10.8.0.3/32
```

Home side adds `PersistentKeepalive = 25` and points its `Endpoint` at the VPS.
Replace `192.168.1.0/24` with your actual home LAN range.

Note the trade: all car traffic now crosses the VPS, so its bandwidth and its
location become yours. A $5 box with a gigabit port is plenty for one person.

---

## Step 2 — WireGuard client on the Mango

The config the car needs:

```ini
[Interface]
PrivateKey = <contents of car.key>
Address    = 10.8.0.2/32
DNS        = 10.8.0.1

[Peer]
PublicKey  = <contents of server.pub>
Endpoint   = yourhome.duckdns.org:51820
AllowedIPs = 0.0.0.0/0, ::/0

# Mobile networks drop idle NAT bindings aggressively. Without this the tunnel
# works until you stop using it for a minute and then silently stops.
PersistentKeepalive = 25
```

`AllowedIPs = 0.0.0.0/0` sends *everything* through the tunnel, which is what you
want — otherwise only traffic to your home LAN takes it.

On the Mango: **VPN → WireGuard Client → Add Manually**, paste the config, set it
to start on boot.

---

## Step 3 — 5G into the Mango

Two ways, both using the USB port:

**Tether a phone.** Plug it in, enable USB tethering on the phone. The Mango
picks it up as a WAN source automatically. Free, works today, good for testing
whether this whole thing is worth building.

**A USB 5G modem with its own SIM.** Tidier, always on, costs a modem and a data
plan. Do this only after the tethered version has proven itself.

Set the WAN priority so cellular is used when no known Wi-Fi is in range.

---

## Step 4 — check it works

From a device connected to the Mango's Wi-Fi:

```sh
curl ifconfig.me
```

It should print **your home IP address**. If it prints the carrier's, the tunnel
is not carrying your traffic.

On both ends, `sudo wg show` should list the peer with a recent handshake and
non-zero transfer counters in each direction.

### When it does not work

| Symptom | Usual cause |
|---|---|
| No handshake at all | UDP 51820 not forwarded, or forwarded as TCP |
| Handshake, no traffic | `AllowedIPs` on the client is not `0.0.0.0/0` |
| Works, then dies after a minute | `PersistentKeepalive` missing |
| Works at home, not on cellular | You are behind CGNAT; see Step 1b |
| Everything resolves to nothing | `DNS` not set on the client interface |

---

## Step 5 — expectations

**About 20 Mbps.** That is the Mango's 580 MHz MIPS processor doing encryption
without hardware acceleration — not your 5G link and not your fibre. Fine for
maps, browsing, music and video calls. A Beryl AX (~$110) does roughly 200 Mbps
if you need more.

And the thing worth being clear about: the tunnel gives you your home
connection's *identity*, not its *bandwidth*, and not free data. Cellular carries
every byte and your carrier bills for it.

---

## Step 6 — the part that actually cuts the bill

Pre-stage content over your home fibre while parked, then serve it in the car
from local disk with no tunnel and no cellular at all.

Run `cached` on a Raspberry Pi in the car (the Mango's 16 MB of flash cannot hold
a Go binary):

```sh
# while on home wifi
curl -X POST http://127.0.0.1:8078/v1/prestage -d '{
  "name": "maps/region.mbtiles",
  "url":  "https://example.org/region.mbtiles",
  "pin":  true
}'

# later, anywhere, with or without signal
curl http://127.0.0.1:8078/c/maps/region.mbtiles
```

Add `mobilelinkd` alongside it and bulk transfers wait for a free link on their
own, so cellular only ever carries what genuinely has to happen now. See
`mobile/README.md` and `cache/README.md`.

---

## Shopping list for the minimum version

| Item | Cost |
|---|---|
| GL.iNet Mango (you have one) | — |
| A phone you already own, tethered | — |
| WireGuard on existing home hardware | — |
| **Total to find out if this is worth it** | **nothing** |

Build that first. Everything above it is optional and the cheap version answers
the only question that matters, which is whether 20 Mbps of your home connection
in the car is actually useful to you.
