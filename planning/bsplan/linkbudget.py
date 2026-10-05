"""Link budget and airtime arithmetic.

Mirrors ``controlplane/internal/radio/radio.go``. Any change here needs the same
change there, and ``tests/test_parity.py`` fails if they drift.
"""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import NamedTuple, Optional

#: Thermal noise power density at 290 K, dBm/Hz.
THERMAL_NOISE_DBM_PER_HZ = -174.0

#: InterferenceDBm value meaning "nothing measured".
NO_INTERFERENCE = -200.0

#: Fraction of raw airtime that carries payload after preambles, guard
#: intervals, block acknowledgements and time-division turnaround.
PROTOCOL_EFFICIENCY = 0.75


class MCS(NamedTuple):
    """One modulation and coding scheme."""

    name: str
    index: int
    min_sinr_db: float
    bits_per_hz: float


#: A generic OFDM ladder. Replace with your vendor's published table and every
#: capacity figure downstream sharpens, because they all derive from this.
MCS_TABLE: tuple[MCS, ...] = (
    MCS("BPSK 1/2", 0, 2.0, 0.50),
    MCS("QPSK 1/2", 1, 5.0, 1.00),
    MCS("QPSK 3/4", 2, 8.0, 1.50),
    MCS("16QAM 1/2", 3, 11.0, 2.00),
    MCS("16QAM 3/4", 4, 15.0, 3.00),
    MCS("64QAM 2/3", 5, 18.0, 4.00),
    MCS("64QAM 3/4", 6, 20.0, 4.50),
    MCS("64QAM 5/6", 7, 23.0, 5.00),
    MCS("256QAM 3/4", 8, 26.0, 6.00),
    MCS("256QAM 5/6", 9, 28.0, 6.67),
    MCS("1024QAM 3/4", 10, 31.0, 7.50),
    MCS("1024QAM 5/6", 11, 34.0, 8.33),
)


def select_mcs(sinr_db: float) -> Optional[MCS]:
    """Fastest modulation the given SINR supports, or None if the link fails."""
    for mcs in reversed(MCS_TABLE):
        if sinr_db >= mcs.min_sinr_db:
            return mcs
    return None


def minimum_viable_sinr_db() -> float:
    """SINR below which no modulation closes the link."""
    return MCS_TABLE[0].min_sinr_db


def free_space_path_loss_db(distance_km: float, freq_mhz: float) -> float:
    """ITU free-space loss."""
    if distance_km <= 0 or freq_mhz <= 0:
        return 0.0
    return 32.44 + 20 * math.log10(distance_km) + 20 * math.log10(freq_mhz)


def noise_floor_dbm(channel_width_mhz: float, noise_figure_db: float) -> float:
    """Thermal noise in the channel plus the receiver's own noise."""
    if channel_width_mhz <= 0:
        return THERMAL_NOISE_DBM_PER_HZ + noise_figure_db
    return (
        THERMAL_NOISE_DBM_PER_HZ
        + 10 * math.log10(channel_width_mhz * 1e6)
        + noise_figure_db
    )


@dataclass
class LinkBudget:
    """Everything needed to predict one path's SINR."""

    tx_power_dbm: float
    tx_gain_dbi: float
    rx_gain_dbi: float
    freq_mhz: float
    channel_width_mhz: float
    distance_km: float
    noise_figure_db: float = 6.0
    feeder_loss_db: float = 0.0

    #: Excess loss over free space: foliage, buildings, Fresnel intrusion, rain.
    #: Zero means a fully cleared line of sight, which is rarer than planning
    #: spreadsheets assume.
    clutter_loss_db: float = 0.0

    #: Held back so the link survives weather rather than running at the edge of
    #: its modulation. Raise it above 11 GHz where rain fade dominates.
    fade_margin_db: float = 10.0

    #: Aggregate co-channel power. In unlicensed spectrum this, not distance, is
    #: usually what sets your capacity.
    interference_dbm: float = NO_INTERFERENCE

    def eirp_dbm(self) -> float:
        """Effective radiated power. This is the figure a regulator caps."""
        return self.tx_power_dbm + self.tx_gain_dbi - self.feeder_loss_db / 2

    def rx_power_dbm(self) -> float:
        """Received signal power."""
        return (
            self.tx_power_dbm
            + self.tx_gain_dbi
            + self.rx_gain_dbi
            - free_space_path_loss_db(self.distance_km, self.freq_mhz)
            - self.feeder_loss_db
            - self.clutter_loss_db
        )

    def sinr_db(self) -> float:
        """Signal to interference plus noise, with the fade margin deducted."""
        noise = noise_floor_dbm(self.channel_width_mhz, self.noise_figure_db)
        interference = self.interference_dbm if self.interference_dbm != 0 else NO_INTERFERENCE
        # Noise and interference add as powers, not as decibels.
        total = 10 * math.log10(10 ** (noise / 10) + 10 ** (interference / 10))
        return self.rx_power_dbm() - total - self.fade_margin_db

    def achievable_mbps(self, protocol_efficiency: float = PROTOCOL_EFFICIENCY) -> float:
        """Usable throughput, or 0 when the link does not close."""
        mcs = select_mcs(self.sinr_db())
        if mcs is None or self.channel_width_mhz <= 0:
            return 0.0
        return mcs.bits_per_hz * self.channel_width_mhz * protocol_efficiency


def airtime_fraction(demand_mbps: float, achievable_mbps: float) -> float:
    """Share of a sector's airtime consumed by carrying demand over this link.

    The function the whole capacity model turns on. Sum it across a sector's
    subscribers and the total must stay under the airtime budget; a sector has no
    bandwidth to divide, only one second per second.
    """
    if demand_mbps <= 0:
        return 0.0
    if achievable_mbps <= 0:
        return math.inf
    return demand_mbps / achievable_mbps
