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

## Choose your tunnel first

Two ways to do this, and they change the rest of the document.

| | WireGuard | Tailscale |
|---|---|---|
| NAT traversal | you arrange it | automatic |
| Behind CGNAT | needs a public IP or a VPS | just works |
| Port forwarding | UDP 51820 | none |
| Dynamic DNS | required | none |
| Runs on the Mango | **yes** (kernel module, tiny) | **no** (see below) |
| Setup effort | an evening | twenty minutes |

**Tailscale is the better choice if you have somewhere to run it.** It is
WireGuard underneath with the hard part — getting two machines behind NAT to find
each other — done for you. Skip to the Tailscale section.

### The Mango cannot run Tailscale

GL.iNet lists the GL-MT300N-V2 as an unsupported model. Tailscale's MIPS build
is two binaries totalling about 24 MB and the Mango has 16 MB of NOR flash, so it
does not fit. People have run it from USB storage, but that is unofficial, does
not survive a reboot cleanly, and **the Mango has one USB port that you need for
the phone tether.** You cannot have both.

So pick one of these:

| Option | What it costs | What you get |
|---|---|---|
| Tailscale on a Raspberry Pi in the car, Mango does Wi-Fi only | a Pi you may already want for `cached` | clean, fast, no CGNAT worries |
| Replace the Mango with a Beryl AX (GL-MT3000) | ~$110 | Tailscale supported, ~200 Mbps encrypted |
| Keep the Mango, use plain WireGuard | nothing | works, but you handle NAT yourself |

The first is the best value if you were going to run the content cache anyway,
since the same Pi does both jobs.

---

## Step 0 — find out whether you are behind CGNAT

**Skip this entirely if you are using Tailscale.** Its whole point is that the
answer stops mattering.


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

## Tailscale path

### At home

On whatever is always on — the router, a Pi, a NAS:

```sh
curl -fsSL https://tailscale.com/install.sh | sh

# Forwarding has to be on, or the exit node accepts traffic and drops it.
printf 'net.ipv4.ip_forward = 1\nnet.ipv6.conf.all.forwarding = 1\n' \
  | sudo tee /etc/sysctl.d/99-tailscale.conf
sudo sysctl -p /etc/sysctl.d/99-tailscale.conf

sudo tailscale up --advertise-exit-node
```

Then **approve it in the admin console**: Machines → your home node → Edit route
settings → *Use as exit node*. Advertising is only an offer; without approval the
car will connect and route nothing, which looks exactly like a broken tunnel.

### In the car, on the Pi

```sh
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --exit-node=<home-hostname> --exit-node-allow-lan-access
```

`--exit-node-allow-lan-access` keeps the Pi reachable from the car's own network
while everything else goes home. Without it you lose the ability to administer
the thing you are sitting next to.

### Share it to the rest of the car

The Pi now has a tunnel; the Mango's clients need to use it. On the Pi:

```sh
sudo sysctl -w net.ipv4.ip_forward=1
sudo iptables -t nat -A POSTROUTING -o tailscale0 -j MASQUERADE
sudo iptables -A FORWARD -i eth0 -o tailscale0 -j ACCEPT
sudo iptables -A FORWARD -i tailscale0 -o eth0 \
  -m state --state RELATED,ESTABLISHED -j ACCEPT
```

Persist those with `iptables-persistent`, or they vanish at the next reboot.

Then on the Mango, point the default gateway at the Pi's address. The Mango keeps
doing what it is good at — Wi-Fi, DHCP, the USB tether — and the Pi carries the
tunnel.

### Check it

From a device on the Mango's Wi-Fi:

```sh
curl ifconfig.me          # should print your home IP
tailscale status          # on the Pi: shows the peer and whether it is direct
```

`tailscale status` is worth reading carefully. If it says **`relay`** rather than
a direct connection, traffic is going through Tailscale's DERP servers: it works,
but it is slower and adds latency. `tailscale netcheck` will say why.

### What differs from WireGuard

No port forward. No dynamic DNS. No CGNAT workaround. In exchange you depend on
Tailscale's coordination service to broker connections, and you accept a little
more overhead per packet. For this use that is a good trade.

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

Tailscale has its own short list:

| Symptom | Usual cause |
|---|---|
| Peers connect, nothing routes | Exit node advertised but never approved in the admin console |
| Slow, high latency | `tailscale status` says `relay`; NAT traversal failed, traffic is going via DERP |
| Exit node works, car LAN does not | Forwarding or the NAT rule missing on the Pi |
| Cannot reach the Pi once connected | `--exit-node-allow-lan-access` not set |

---

## Step 5 — expectations

Whatever carries the tunnel sets the ceiling, and it is rarely the link:

| Carrying the tunnel | Roughly |
|---|---|
| Mango, WireGuard | 20 Mbps — 580 MHz MIPS, no crypto acceleration |
| Raspberry Pi 4 or 5, Tailscale | 200–400 Mbps; cellular becomes the limit again |
| Beryl AX, either | ~200 Mbps |

Twenty megabits is fine for maps, browsing, music and video calls. It is the
Mango's processor, not your 5G and not your fibre.

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

For the Tailscale version, add a Raspberry Pi. If you were going to run `cached`
in the car anyway, that Pi does both jobs and the tunnel stops being the
bottleneck.

Build that first. Everything above it is optional and the cheap version answers
the only question that matters, which is whether 20 Mbps of your home connection
in the car is actually useful to you.
