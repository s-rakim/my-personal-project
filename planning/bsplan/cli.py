"""Command line interface.

    python -m bsplan capacity --plan 100 --contention 20
    python -m bsplan coverage --sector ridge-n
    python -m bsplan link --site pop-ridge --to 44.95,-93.09
    python -m bsplan check
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from . import __version__
from .capacity import DemandProfile, network_capacity, range_for_rate_km, sector_capacity
from .geo import Point, angle_diff_deg, fresnel_clearance_m
from .inventory import Inventory, Sector, load
from .linkbudget import LinkBudget, select_mcs

DEFAULT_INVENTORY = "var/inventory.json"


def _load(path: str) -> Inventory:
    p = Path(path)
    if not p.exists():
        raise SystemExit(
            f"bsplan: no inventory at {p}\n"
            f"        run `basestationd -config var/config.json seed` first, "
            f"or pass --inventory"
        )
    return load(p)


def cmd_capacity(args: argparse.Namespace) -> int:
    inv = _load(args.inventory)
    demand = DemandProfile(
        plan_down_mbps=args.plan,
        contention_ratio=args.contention,
        min_service_mbps=args.min_service,
    )
    net = network_capacity(inv, demand)

    print(f"Capacity for a {args.plan:.0f} Mbps plan at {args.contention:.0f}:1 contention")
    print(f"  sustained load per subscriber: {net.busy_hour_per_subscriber_mbps:.2f} Mbps")
    print(f"  service floor: {demand.service_floor_mbps:.0f} Mbps "
          f"(only sell where the link delivers at least this)")
    print()
    print(f"  {'sector':<12} {'site':<13} {'link':>7} {'design':>7} "
          f"{'area':>8} {'aggregate':>10} {'subs':>6}")
    print(f"  {'-'*12} {'-'*13} {'-'*7} {'-'*7} {'-'*8} {'-'*10} {'-'*6}")
    for c in net.sectors:
        flag = " (capped)" if c.limited_by_max_terminals else ""
        print(f"  {c.sector_id:<12} {c.site_id:<13} {c.usable_range_km:>6.2f}k "
              f"{c.design_range_km:>6.2f}k {c.coverage_km2:>7.1f}k² "
              f"{c.aggregate_mbps:>7.0f}Mb {c.subscribers:>6}{flag}")
    print()
    print(f"  airtime supports      {net.airtime_subscribers:>6} subscribers")
    print(f"  backhaul supports     {net.backhaul_subscribers:>6} subscribers "
          f"({net.total_backhaul_mbps:.0f} Mbps of transit)")
    print(f"  you can sell          {net.subscribers:>6} subscribers")
    print()
    print(f"  Binding constraint: {net.binding_constraint}")
    print(f"  {net.headroom_note}")

    if net.binding_constraint == "airtime" and net.total_backhaul_mbps > 0:
        used = net.subscribers * net.busy_hour_per_subscriber_mbps
        pct = 100 * used / net.total_backhaul_mbps
        print(f"  At that count you would use {used:.0f} of "
              f"{net.total_backhaul_mbps:.0f} Mbps ({pct:.0f}%) of your transit.")
        if pct < 50:
            per_sector = net.airtime_subscribers / max(1, len(net.sectors))
            if per_sector > 0:
                need = int(net.backhaul_subscribers / per_sector)
                print(f"  To consume the transit you have, you need about "
                      f"{need} sectors; you have {len(net.sectors)}.")
    return 0


def cmd_coverage(args: argparse.Namespace) -> int:
    inv = _load(args.inventory)
    sector = inv.sectors.get(args.sector)
    if sector is None:
        print(f"bsplan: no sector {args.sector!r}. Known: "
              f"{', '.join(sorted(inv.sectors)) or '(none)'}", file=sys.stderr)
        return 1
    site = inv.sites.get(sector.site_id)
    if site is None:
        print(f"bsplan: sector {sector.id} references unknown site {sector.site_id}",
              file=sys.stderr)
        return 1

    print(f"Sector {sector.id} on {site.id} ({site.name})")
    print(f"  {sector.freq_mhz:.0f} MHz, {sector.channel_width_mhz:.0f} MHz channel, "
          f"azimuth {sector.azimuth_deg:.0f}°, beamwidth {sector.beamwidth_deg:.0f}°, "
          f"downtilt {sector.downtilt_deg:.0f}°")

    budget = LinkBudget(
        tx_power_dbm=sector.tx_power_dbm, tx_gain_dbi=sector.antenna_gain_dbi,
        rx_gain_dbi=args.terminal_gain, freq_mhz=sector.freq_mhz,
        channel_width_mhz=sector.channel_width_mhz, distance_km=1.0,
        feeder_loss_db=sector.feeder_loss_db, clutter_loss_db=sector.clutter_loss_db,
        fade_margin_db=sector.fade_margin_db, interference_dbm=sector.interference_dbm,
    )
    print(f"  EIRP {budget.eirp_dbm():.1f} dBm  "
          f"(check this against your band's limit before transmitting)")
    print()
    print(f"  {'range':>8} {'Rx power':>10} {'SINR':>7} {'modulation':<14} {'rate':>9} "
          f"{'Fresnel':>9}")
    print(f"  {'-'*8} {'-'*10} {'-'*7} {'-'*14} {'-'*9} {'-'*9}")

    for d in args.distances:
        budget.distance_km = d
        sinr = budget.sinr_db()
        mcs = select_mcs(sinr)
        rate = budget.achievable_mbps(sector.protocol_efficiency)
        name = mcs.name if mcs else "-- no link --"
        clearance = fresnel_clearance_m(d, sector.freq_mhz)
        print(f"  {d:>7.2f}k {budget.rx_power_dbm():>9.1f} {sinr:>6.1f} "
              f"{name:<14} {rate:>7.0f}Mb {clearance:>7.1f}m")

    print()
    for rate_target in (25, 50, 100, 200):
        reach = range_for_rate_km(sector, rate_target, args.terminal_gain)
        if reach > 0:
            print(f"  {rate_target:>4} Mbps deliverable out to {reach:.2f} km")
        else:
            print(f"  {rate_target:>4} Mbps not deliverable anywhere on this sector")
    return 0


def cmd_link(args: argparse.Namespace) -> int:
    inv = _load(args.inventory)
    site = inv.sites.get(args.site)
    if site is None:
        print(f"bsplan: no site {args.site!r}. Known: {', '.join(sorted(inv.sites))}",
              file=sys.stderr)
        return 1

    try:
        lat_s, lon_s = args.to.split(",")
        target = Point(lat=float(lat_s), lon=float(lon_s), height_m=args.height)
    except ValueError:
        print("bsplan: --to must be lat,lon (e.g. 44.95,-93.09)", file=sys.stderr)
        return 1

    distance = site.position.distance_km(target)
    bearing = site.position.bearing_deg(target)
    elevation = site.position.elevation_angle_deg(target)

    print(f"From {site.id} ({site.position.lat:.5f}, {site.position.lon:.5f}, "
          f"{site.position.height_m:.0f} m)")
    print(f"To   {target.lat:.5f}, {target.lon:.5f}, {target.height_m:.0f} m")
    print(f"  distance {distance:.2f} km, bearing {bearing:.1f}°, "
          f"elevation {elevation:+.2f}°")
    print(f"  Fresnel clearance needed at midpoint: "
          f"{fresnel_clearance_m(distance, 5775):.1f} m (at 5.8 GHz)")
    print()

    sectors = inv.sectors_of(site.id)
    if not sectors:
        print("  this site has no sectors")
        return 0

    print(f"  {'sector':<12} {'in beam':<9} {'SINR':>7} {'modulation':<14} {'rate':>9}")
    print(f"  {'-'*12} {'-'*9} {'-'*7} {'-'*14} {'-'*9}")

    served = False
    for sector in sorted(sectors, key=lambda s: s.id):
        reasons = _beam_check(sector, bearing, elevation)
        budget = LinkBudget(
            tx_power_dbm=sector.tx_power_dbm, tx_gain_dbi=sector.antenna_gain_dbi,
            rx_gain_dbi=args.terminal_gain, freq_mhz=sector.freq_mhz,
            channel_width_mhz=sector.channel_width_mhz, distance_km=distance,
            feeder_loss_db=sector.feeder_loss_db, clutter_loss_db=sector.clutter_loss_db,
            fade_margin_db=sector.fade_margin_db, interference_dbm=sector.interference_dbm,
        )
        sinr = budget.sinr_db()
        mcs = select_mcs(sinr)
        rate = budget.achievable_mbps(sector.protocol_efficiency)
        verdict = "yes" if not reasons else "no"
        name = mcs.name if mcs else "-- no link --"
        print(f"  {sector.id:<12} {verdict:<9} {sinr:>6.1f} {name:<14} {rate:>7.0f}Mb")
        if reasons:
            for r in reasons:
                print(f"               {r}")
        elif rate > 0:
            served = True

    print()
    print("  SERVICEABLE" if served else
          "  NOT SERVICEABLE from this site: no sector covers this location")
    return 0


def _beam_check(sector: Sector, bearing: float, elevation: float) -> list[str]:
    """Why a location falls outside a sector's beam, if it does."""
    reasons: list[str] = []

    if 0 < sector.beamwidth_deg < 360:
        off = angle_diff_deg(bearing, sector.azimuth_deg)
        if off > sector.beamwidth_deg / 2:
            reasons.append(
                f"{off:.1f}° off azimuth {sector.azimuth_deg:.0f}°, "
                f"outside the {sector.beamwidth_deg:.0f}° beam"
            )
    if sector.v_beamwidth_deg > 0:
        off_v = angle_diff_deg(elevation, -sector.downtilt_deg)
        if off_v > sector.v_beamwidth_deg / 2:
            reasons.append(
                f"{off_v:.1f}° off the {sector.downtilt_deg:.0f}° downtilt, "
                f"outside the {sector.v_beamwidth_deg:.0f}° vertical beam "
                f"(too close to the tower, or too high)"
            )
    return reasons


def cmd_check(args: argparse.Namespace) -> int:
    """Check the inventory against physics and against its own geometry."""
    inv = _load(args.inventory)
    problems: list[str] = []
    notes: list[str] = []

    for sector in sorted(inv.sectors.values(), key=lambda s: s.id):
        budget = LinkBudget(
            tx_power_dbm=sector.tx_power_dbm, tx_gain_dbi=sector.antenna_gain_dbi,
            rx_gain_dbi=19.0, freq_mhz=sector.freq_mhz,
            channel_width_mhz=sector.channel_width_mhz, distance_km=1.0,
            feeder_loss_db=sector.feeder_loss_db,
        )
        eirp = budget.eirp_dbm()
        # 36 dBm EIRP is the common US limit for 5 GHz point-to-multipoint. Your
        # band and jurisdiction differ; this is a prompt to check, not a ruling.
        if eirp > 36:
            notes.append(
                f"sector {sector.id} radiates {eirp:.1f} dBm EIRP, above the 36 dBm "
                f"that often applies to 5 GHz point-to-multipoint. Confirm your band's "
                f"limit before transmitting."
            )
        if sector.interference_dbm == 0:
            notes.append(
                f"sector {sector.id} has no measured interference floor. In unlicensed "
                f"spectrum that is usually the real capacity limit, so scan before you "
                f"trust any capacity figure for it."
            )

    # Every terminal must be inside some sector's beam, or it has no service and
    # the inventory does not say so anywhere.
    for term in sorted(inv.terminals.values(), key=lambda t: t.id):
        if not term.enabled:
            continue
        reachable = []
        for sector in inv.sectors.values():
            if not sector.enabled:
                continue
            site = inv.sites.get(sector.site_id)
            if site is None or not site.enabled:
                continue
            distance = site.position.distance_km(term.position)
            if sector.max_range_km > 0 and distance > sector.max_range_km:
                continue
            bearing = site.position.bearing_deg(term.position)
            elevation = site.position.elevation_angle_deg(term.position)
            if _beam_check(sector, bearing, elevation):
                continue
            budget = LinkBudget(
                tx_power_dbm=sector.tx_power_dbm, tx_gain_dbi=sector.antenna_gain_dbi,
                rx_gain_dbi=term.antenna_gain_dbi, freq_mhz=sector.freq_mhz,
                channel_width_mhz=sector.channel_width_mhz, distance_km=distance,
                noise_figure_db=term.noise_figure_db,
                feeder_loss_db=sector.feeder_loss_db + term.feeder_loss_db,
                clutter_loss_db=sector.clutter_loss_db,
                fade_margin_db=sector.fade_margin_db,
                interference_dbm=sector.interference_dbm,
            )
            if budget.achievable_mbps(sector.protocol_efficiency) > 0:
                reachable.append((sector.id, budget.achievable_mbps(sector.protocol_efficiency)))

        if not reachable:
            problems.append(
                f"terminal {term.id} cannot reach any sector. It will be provisioned, "
                f"authenticate, and then receive no allocation."
            )
        elif len(reachable) == 1:
            notes.append(
                f"terminal {term.id} can only reach {reachable[0][0]}. It has no "
                f"fallback if that sector fails."
            )

    # A relay must sit inside the beam it relays through.
    for site in sorted(inv.sites.values(), key=lambda s: s.id):
        if not site.backhaul_sector_id:
            continue
        parent_sector = inv.sectors.get(site.backhaul_sector_id)
        if parent_sector is None:
            problems.append(f"site {site.id} relays over unknown sector "
                            f"{site.backhaul_sector_id}")
            continue
        parent_site = inv.sites.get(parent_sector.site_id)
        if parent_site is None:
            continue
        bearing = parent_site.position.bearing_deg(site.position)
        elevation = parent_site.position.elevation_angle_deg(site.position)
        reasons = _beam_check(parent_sector, bearing, elevation)
        if reasons:
            problems.append(
                f"site {site.id} relays over {parent_sector.id} but sits outside its "
                f"beam: {reasons[0]}"
            )

    for p in problems:
        print(f"  PROBLEM  {p}")
    for n in notes:
        print(f"  note     {n}")
    if not problems and not notes:
        print("  inventory looks consistent")
    print()
    print(f"  {len(problems)} problem(s), {len(notes)} note(s)")
    return 1 if problems else 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="bsplan",
        description="RF planning and capacity forecasting for a fixed wireless network",
    )
    parser.add_argument("--version", action="version", version=f"bsplan {__version__}")
    parser.add_argument("--inventory", default=DEFAULT_INVENTORY,
                        help=f"inventory file (default: {DEFAULT_INVENTORY})")
    parser.add_argument("--terminal-gain", type=float, default=19.0,
                        help="subscriber antenna gain in dBi (default: 19)")

    sub = parser.add_subparsers(dest="command", required=True)

    cap = sub.add_parser("capacity", help="how many subscribers will fit")
    cap.add_argument("--plan", type=float, default=100.0, help="plan speed in Mbps")
    cap.add_argument("--contention", type=float, default=20.0,
                     help="contention ratio, e.g. 20 for 20:1 (default: 20)")
    cap.add_argument("--min-service", type=float, default=0.0,
                     help="minimum deliverable rate to sell at (default: the plan speed)")
    cap.set_defaults(func=cmd_capacity)

    cov = sub.add_parser("coverage", help="rate against distance for one sector")
    cov.add_argument("--sector", required=True)
    cov.add_argument("--distances", type=float, nargs="+",
                     default=[0.25, 0.5, 1, 2, 3, 5, 8, 12],
                     help="distances in km to tabulate")
    cov.set_defaults(func=cmd_coverage)

    lnk = sub.add_parser("link", help="is one location serviceable from a site")
    lnk.add_argument("--site", required=True)
    lnk.add_argument("--to", required=True, metavar="LAT,LON")
    lnk.add_argument("--height", type=float, default=6.0,
                     help="subscriber antenna height in metres (default: 6)")
    lnk.set_defaults(func=cmd_link)

    chk = sub.add_parser("check", help="validate inventory against physics and geometry")
    chk.set_defaults(func=cmd_check)

    args = parser.parse_args(argv)
    return args.func(args)
