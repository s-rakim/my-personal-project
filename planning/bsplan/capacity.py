"""How many subscribers will actually fit.

Two independent limits, and which one binds is the whole question:

**Airtime.** A sector has one second per second. A subscriber at the cell edge
running QPSK 1/2 spends several times the airtime of a close one running 256QAM
to move the same bytes, so a sector's subscriber count depends on *where* the
subscribers are, not merely how many there are. This module integrates over the
coverage area rather than assuming an average distance, because the airtime cost
of distance is strongly non-linear and an average under-counts it badly.

**Backhaul.** Your fibre is a hard ceiling on the whole network, and no amount of
spectrum changes it.

The usual mistake is to plan against only one of the two. Build out sectors
against a fibre limit and you oversubscribe the air; buy more fibre against an
airtime limit and you have bought nothing.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

from .inventory import Inventory, Sector
from .linkbudget import LinkBudget, airtime_fraction

#: Steps used when integrating over a sector's radius. 240 is well past the point
#: where the answer stops moving.
_INTEGRATION_STEPS = 240

#: Minimum serviceable range. Nobody installs a subscriber antenna 50 m from the
#: tower, and the vertical beam excludes them anyway.
_MIN_RANGE_KM = 0.15


@dataclass
class DemandProfile:
    """What one subscriber is assumed to offer at the busy hour.

    ``contention_ratio`` is the WISP convention: 20 means twenty subscribers
    share one subscriber's worth of sustained capacity, because they are not all
    downloading at once. Residential networks run 20:1 to 40:1 and feel fine;
    business customers who actually use what they bought want 2:1 to 5:1.

    ``min_service_mbps`` is the design decision that makes capacity planning
    meaningful, and leaving it out is the classic way to get a nonsense answer.
    A sector's link *closes* far beyond the point where it can still deliver a
    plan: at 40 MHz the lowest modulation yields about 15 Mbps, so a 10 km link
    may technically work while being useless for a 100 Mbps product. If you plan
    coverage out to where the link merely closes, your model fills the sector
    with edge subscribers on the slowest modulation, each consuming many times
    the airtime, and tells you a sector holds two customers.

    Real operators pick a minimum deliverable rate and only sell inside it. That
    radius, not the link-closing radius, is the one to plan against. Defaults to
    the plan speed, so by default you sell only where a subscriber can actually
    receive what they bought on an idle sector.
    """

    plan_down_mbps: float
    contention_ratio: float = 20.0
    min_service_mbps: float = 0.0

    @property
    def busy_hour_mbps(self) -> float:
        """Sustained per-subscriber load the network must actually carry."""
        if self.contention_ratio <= 0:
            return self.plan_down_mbps
        return self.plan_down_mbps / self.contention_ratio

    @property
    def service_floor_mbps(self) -> float:
        """Minimum link rate required to sell service at a location."""
        return self.min_service_mbps if self.min_service_mbps > 0 else self.plan_down_mbps


@dataclass
class SectorCapacity:
    """One sector's carrying capacity."""

    sector_id: str
    site_id: str

    #: Furthest distance at which the link still closes, after fade margin.
    #: Interesting for understanding the radio, not for planning coverage.
    usable_range_km: float

    #: Furthest distance at which the plan speed can still be delivered. This is
    #: the number to draw on a map and quote to a prospective subscriber.
    design_range_km: float

    #: Peak rate achievable, at the inner edge of coverage.
    best_mbps: float

    #: Rate at the usable range limit.
    edge_mbps: float

    #: Area-weighted mean rate across the coverage wedge.
    mean_mbps: float

    coverage_km2: float
    downlink_airtime: float

    #: Mean airtime one subscriber consumes at the busy hour.
    mean_airtime_per_subscriber: float

    #: Subscribers the airtime supports.
    subscribers: int

    #: Sustained megabits those subscribers offer, which the backhaul must carry.
    offered_mbps: float

    #: Aggregate throughput the sector delivers when full. Sanity-check the rest
    #: against this: offered_mbps must not exceed it.
    aggregate_mbps: float

    #: Cap from inventory, when one is set and it binds first.
    limited_by_max_terminals: bool


def _budget_at(sector: Sector, distance_km: float,
               terminal_gain_dbi: float, terminal_noise_figure_db: float,
               terminal_feeder_loss_db: float) -> LinkBudget:
    return LinkBudget(
        tx_power_dbm=sector.tx_power_dbm,
        tx_gain_dbi=sector.antenna_gain_dbi,
        rx_gain_dbi=terminal_gain_dbi,
        freq_mhz=sector.freq_mhz,
        channel_width_mhz=sector.channel_width_mhz,
        distance_km=distance_km,
        noise_figure_db=terminal_noise_figure_db,
        feeder_loss_db=sector.feeder_loss_db + terminal_feeder_loss_db,
        clutter_loss_db=sector.clutter_loss_db,
        fade_margin_db=sector.fade_margin_db,
        interference_dbm=sector.interference_dbm,
    )


def range_for_rate_km(sector: Sector, min_rate_mbps: float,
                      terminal_gain_dbi: float = 19.0,
                      terminal_noise_figure_db: float = 6.0,
                      terminal_feeder_loss_db: float = 0.5,
                      ceiling_km: float = 60.0) -> float:
    """Furthest distance at which this sector still delivers min_rate_mbps.

    Found by bisection. Rate falls monotonically with distance, so bisection is
    valid; it steps down the modulation ladder rather than falling smoothly, so
    the answer sits on an MCS boundary.

    Pass 0 for min_rate_mbps to get the link-closing range instead.
    """

    def good(d: float) -> bool:
        rate = _budget_at(sector, d, terminal_gain_dbi, terminal_noise_figure_db,
                          terminal_feeder_loss_db).achievable_mbps(
                              sector.protocol_efficiency)
        return rate > 0 and rate >= min_rate_mbps

    hard_cap = ceiling_km
    if sector.max_range_km > 0:
        hard_cap = min(hard_cap, sector.max_range_km)

    if good(hard_cap):
        return hard_cap
    if not good(_MIN_RANGE_KM):
        return 0.0

    lo, hi = _MIN_RANGE_KM, hard_cap
    for _ in range(50):
        mid = (lo + hi) / 2
        if good(mid):
            lo = mid
        else:
            hi = mid
    return lo


def sector_capacity(sector: Sector, demand: DemandProfile,
                    terminal_gain_dbi: float = 19.0,
                    terminal_noise_figure_db: float = 6.0,
                    terminal_feeder_loss_db: float = 0.5) -> SectorCapacity:
    """Compute one sector's subscriber capacity.

    Subscribers are assumed uniformly distributed by *area* across the coverage
    wedge, which weights the far ring more heavily than the near one because it
    is larger. That matters: the outer half of a sector's radius holds three
    quarters of its area and costs the most airtime per subscriber, so assuming an
    average distance overstates capacity considerably.
    """
    link_reach = range_for_rate_km(sector, 0.0, terminal_gain_dbi,
                                   terminal_noise_figure_db, terminal_feeder_loss_db)
    design_reach = range_for_rate_km(sector, demand.service_floor_mbps, terminal_gain_dbi,
                                     terminal_noise_figure_db, terminal_feeder_loss_db)

    def rate_at(d: float) -> float:
        return _budget_at(sector, d, terminal_gain_dbi, terminal_noise_figure_db,
                          terminal_feeder_loss_db).achievable_mbps(
                              sector.protocol_efficiency)

    if design_reach <= _MIN_RANGE_KM:
        return SectorCapacity(
            sector_id=sector.id, site_id=sector.site_id,
            usable_range_km=link_reach, design_range_km=design_reach,
            best_mbps=rate_at(_MIN_RANGE_KM), edge_mbps=0.0, mean_mbps=0.0,
            coverage_km2=0.0, downlink_airtime=sector.downlink_airtime,
            mean_airtime_per_subscriber=math.inf, subscribers=0,
            offered_mbps=0.0, aggregate_mbps=0.0, limited_by_max_terminals=False,
        )

    # Area-weighted integration over the radius. Weight 2r/(r_out^2 - r_in^2) is
    # the density of a uniform-by-area distribution.
    r_in, r_out = _MIN_RANGE_KM, design_reach
    span = r_out**2 - r_in**2
    step = (r_out - r_in) / _INTEGRATION_STEPS

    weighted_airtime = 0.0
    weighted_rate = 0.0
    for i in range(_INTEGRATION_STEPS):
        r = r_in + (i + 0.5) * step
        weight = (2 * r * step) / span
        rate = rate_at(r)
        weighted_rate += weight * rate
        weighted_airtime += weight * airtime_fraction(demand.busy_hour_mbps, rate)

    coverage = (sector.beamwidth_deg / 360.0) * math.pi * span

    subscribers = 0
    if weighted_airtime > 0 and math.isfinite(weighted_airtime):
        subscribers = int(sector.downlink_airtime / weighted_airtime)

    capped = False
    if sector.max_terminals > 0 and subscribers > sector.max_terminals:
        subscribers = sector.max_terminals
        capped = True

    # The harmonic-weighted rate, which is what the sector actually delivers when
    # every subscriber is busy. Always below the arithmetic mean rate, and that
    # gap is exactly the cost of distance: averaging rates instead of airtime is
    # the arithmetic error that makes optimistic capacity models.
    effective_mbps = (
        demand.busy_hour_mbps / weighted_airtime
        if weighted_airtime > 0 and math.isfinite(weighted_airtime)
        else 0.0
    )

    return SectorCapacity(
        sector_id=sector.id,
        site_id=sector.site_id,
        usable_range_km=link_reach,
        design_range_km=design_reach,
        best_mbps=rate_at(r_in),
        edge_mbps=rate_at(r_out * 0.999),
        mean_mbps=weighted_rate,
        coverage_km2=coverage,
        downlink_airtime=sector.downlink_airtime,
        mean_airtime_per_subscriber=weighted_airtime,
        subscribers=subscribers,
        offered_mbps=subscribers * demand.busy_hour_mbps,
        aggregate_mbps=effective_mbps * sector.downlink_airtime,
        limited_by_max_terminals=capped,
    )


@dataclass
class NetworkCapacity:
    """The whole network's capacity and which limit binds."""

    sectors: list[SectorCapacity]
    airtime_subscribers: int
    backhaul_subscribers: int
    total_backhaul_mbps: float
    busy_hour_per_subscriber_mbps: float

    @property
    def subscribers(self) -> int:
        """What you can actually sell: the lower of the two limits."""
        return min(self.airtime_subscribers, self.backhaul_subscribers)

    @property
    def binding_constraint(self) -> str:
        if self.airtime_subscribers < self.backhaul_subscribers:
            return "airtime"
        if self.backhaul_subscribers < self.airtime_subscribers:
            return "backhaul"
        return "both"

    @property
    def headroom_note(self) -> str:
        """What to do about the binding limit, in one line."""
        if self.binding_constraint == "airtime":
            return (
                "Spectrum is the limit. More fibre buys nothing. Add sectors, "
                "narrow the existing beams, or add channels."
            )
        if self.binding_constraint == "backhaul":
            return (
                "Backhaul is the limit. More radios buy nothing. Buy more "
                "transit, or raise the contention ratio if the traffic supports it."
            )
        return "Both limits bind at the same point, which is a well-balanced build."


def network_capacity(inv: Inventory, demand: DemandProfile) -> NetworkCapacity:
    """Compute capacity across every enabled sector.

    Only sites that reach the internet on their own circuit contribute backhaul.
    A relay site's capacity arrives through its parent and counting it again
    would be double-counting the same megabits.
    """
    caps: list[SectorCapacity] = []
    for sector in sorted(inv.sectors.values(), key=lambda s: s.id):
        if not sector.enabled:
            continue
        site = inv.sites.get(sector.site_id)
        if site is None or not site.enabled:
            continue

        # Use the real terminal parameters when the inventory has terminals,
        # so planning reflects the antennas actually being installed.
        gains = [t.antenna_gain_dbi for t in inv.terminals.values() if t.enabled]
        gain = sum(gains) / len(gains) if gains else 19.0
        caps.append(sector_capacity(sector, demand, terminal_gain_dbi=gain))

    airtime_subs = sum(c.subscribers for c in caps)

    total_backhaul = sum(
        s.backhaul_mbps
        for s in inv.sites.values()
        if s.enabled and not s.backhaul_sector_id
    )

    per_sub = demand.busy_hour_mbps
    backhaul_subs = int(total_backhaul / per_sub) if per_sub > 0 else 0

    return NetworkCapacity(
        sectors=caps,
        airtime_subscribers=airtime_subs,
        backhaul_subscribers=backhaul_subs,
        total_backhaul_mbps=total_backhaul,
        busy_hour_per_subscriber_mbps=per_sub,
    )
