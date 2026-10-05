"""Read the control plane's inventory file.

The same ``var/inventory.json`` the Go daemon writes, so planning runs against
the network as it is actually configured rather than against a separate
spreadsheet that drifted from it months ago.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .geo import Point


@dataclass
class Sector:
    id: str
    site_id: str
    name: str = ""
    azimuth_deg: float = 0.0
    beamwidth_deg: float = 90.0
    v_beamwidth_deg: float = 0.0
    downtilt_deg: float = 0.0
    freq_mhz: float = 5775.0
    channel_width_mhz: float = 40.0
    tx_power_dbm: float = 25.0
    antenna_gain_dbi: float = 17.0
    feeder_loss_db: float = 1.5
    clutter_loss_db: float = 0.0
    fade_margin_db: float = 10.0
    interference_dbm: float = 0.0
    noise_figure_db: float = 6.0
    airtime_budget: float = 0.85
    protocol_efficiency: float = 0.75
    dl_ul_split: float = 0.75
    max_terminals: int = 0
    max_range_km: float = 0.0
    enabled: bool = True

    @property
    def downlink_airtime(self) -> float:
        """Airtime available to the downlink."""
        split = self.dl_ul_split if self.dl_ul_split > 0 else 0.75
        return self.airtime_budget * split


@dataclass
class Site:
    id: str
    name: str = ""
    position: Point = field(default_factory=lambda: Point(0, 0, 0))
    parent_site_id: str = ""
    backhaul_kind: str = "fiber"
    backhaul_mbps: float = 0.0
    backhaul_sector_id: str = ""
    monthly_cap_gb: float = 0.0
    enabled: bool = True
    notes: str = ""


@dataclass
class PlanTier:
    name: str
    down_mbps: float = 0.0
    up_mbps: float = 0.0
    committed_down_mbps: float = 0.0
    committed_up_mbps: float = 0.0
    priority: int = 1
    burst_mbps: float = 0.0
    burst_seconds: float = 0.0


@dataclass
class Terminal:
    id: str
    subscriber_id: str = ""
    position: Point = field(default_factory=lambda: Point(0, 0, 0))
    antenna_gain_dbi: float = 19.0
    noise_figure_db: float = 6.0
    feeder_loss_db: float = 0.5
    tx_power_dbm: float = 23.0
    pinned_sector_id: str = ""
    enabled: bool = True


@dataclass
class Subscriber:
    id: str
    name: str = ""
    plan: str = ""
    suspended: bool = False


@dataclass
class Inventory:
    plans: dict[str, PlanTier] = field(default_factory=dict)
    sites: dict[str, Site] = field(default_factory=dict)
    sectors: dict[str, Sector] = field(default_factory=dict)
    subscribers: dict[str, Subscriber] = field(default_factory=dict)
    terminals: dict[str, Terminal] = field(default_factory=dict)

    def sectors_of(self, site_id: str) -> list[Sector]:
        return [s for s in self.sectors.values() if s.site_id == site_id]

    def site_of(self, sector_id: str) -> Site | None:
        sector = self.sectors.get(sector_id)
        return self.sites.get(sector.site_id) if sector else None


def _point(raw: dict[str, Any] | None) -> Point:
    raw = raw or {}
    return Point(
        lat=float(raw.get("lat", 0.0)),
        lon=float(raw.get("lon", 0.0)),
        height_m=float(raw.get("height_m", 0.0)),
    )


def _build(cls, raw: dict[str, Any], **overrides):
    """Construct a dataclass from JSON, ignoring fields it does not declare.

    Unknown keys are dropped rather than raising, matching the additive-change
    rule in ``schema/README.md``: a newer daemon may write fields this tool has
    never heard of, and that must not stop you planning.
    """
    known = {f.name for f in cls.__dataclass_fields__.values()}
    kwargs = {k: v for k, v in raw.items() if k in known and k not in overrides}
    kwargs.update(overrides)
    return cls(**kwargs)


def load(path: str | Path) -> Inventory:
    """Load an inventory file."""
    raw = json.loads(Path(path).read_text())

    inv = Inventory()
    for name, p in (raw.get("plans") or {}).items():
        inv.plans[name] = _build(PlanTier, p)
    for sid, s in (raw.get("sites") or {}).items():
        inv.sites[sid] = _build(Site, s, position=_point(s.get("position")))
    for sid, s in (raw.get("sectors") or {}).items():
        inv.sectors[sid] = _build(Sector, s)
    for sid, s in (raw.get("subscribers") or {}).items():
        inv.subscribers[sid] = _build(Subscriber, s)
    for tid, t in (raw.get("terminals") or {}).items():
        inv.terminals[tid] = _build(Terminal, t, position=_point(t.get("position")))
    return inv
