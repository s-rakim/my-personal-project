"""Geodesic helpers.

Spherical earth, which is accurate to about 0.3% over the distances a
fixed-wireless link spans. At 10 km that is 30 m, well inside the error in your
antenna bearing.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

EARTH_RADIUS_KM = 6371.0088


@dataclass(frozen=True)
class Point:
    """A position plus height above ground level."""

    lat: float
    lon: float
    height_m: float = 0.0

    def distance_km(self, other: "Point") -> float:
        """Great-circle distance, ignoring height."""
        lat1, lat2 = math.radians(self.lat), math.radians(other.lat)
        dlat = lat2 - lat1
        dlon = math.radians(other.lon - self.lon)

        h = (
            math.sin(dlat / 2) ** 2
            + math.cos(lat1) * math.cos(lat2) * math.sin(dlon / 2) ** 2
        )
        return 2 * EARTH_RADIUS_KM * math.asin(math.sqrt(min(1.0, h)))

    def bearing_deg(self, other: "Point") -> float:
        """Initial compass bearing to another point, in [0, 360)."""
        lat1, lat2 = math.radians(self.lat), math.radians(other.lat)
        dlon = math.radians(other.lon - self.lon)

        y = math.sin(dlon) * math.cos(lat2)
        x = math.cos(lat1) * math.sin(lat2) - math.sin(lat1) * math.cos(lat2) * math.cos(dlon)
        return math.degrees(math.atan2(y, x)) % 360

    def destination(self, bearing_deg: float, distance_km: float) -> "Point":
        """The point reached by travelling distance_km along bearing_deg."""
        lat1, lon1 = math.radians(self.lat), math.radians(self.lon)
        theta = math.radians(bearing_deg)
        delta = distance_km / EARTH_RADIUS_KM

        sin_lat2 = math.sin(lat1) * math.cos(delta) + math.cos(lat1) * math.sin(delta) * math.cos(theta)
        lat2 = math.asin(max(-1.0, min(1.0, sin_lat2)))
        lon2 = lon1 + math.atan2(
            math.sin(theta) * math.sin(delta) * math.cos(lat1),
            math.cos(delta) - math.sin(lat1) * sin_lat2,
        )
        return Point(
            lat=math.degrees(lat2),
            lon=(math.degrees(lon2) + 540) % 360 - 180,
            height_m=self.height_m,
        )

    def elevation_angle_deg(self, other: "Point") -> float:
        """Angle up to another point, positive when it is higher.

        The check people forget. A subscriber directly beneath a downtilted
        sector is outside its vertical beam no matter how strong the signal
        would otherwise be.
        """
        d = self.distance_km(other) * 1000
        if d < 0.1:
            return 90.0 if other.height_m > self.height_m else -90.0
        return math.degrees(math.atan2(other.height_m - self.height_m, d))


def angle_diff_deg(a: float, b: float) -> float:
    """Absolute difference between two bearings, in [0, 180]."""
    d = abs(a - b) % 360
    return 360 - d if d > 180 else d


def fresnel_clearance_m(distance_km: float, freq_mhz: float, fraction: float = 0.6) -> float:
    """Radius of the Fresnel zone at the midpoint of a path, in metres.

    A link needs this much clearance above every obstacle along its path, not
    merely a visible line of sight. Sixty percent of the first zone is the usual
    engineering minimum; below that the obstruction starts costing real signal
    even though you can see straight through.

    This is why a link that looks fine on a map fails in practice: at 5.8 GHz
    over 5 km the midpoint zone is about 5.5 m, so a hedge at the halfway point
    matters even when both ends are plainly visible to each other.
    """
    if distance_km <= 0 or freq_mhz <= 0:
        return 0.0
    wavelength_m = 299.792458 / freq_mhz
    d_m = distance_km * 1000
    # Radius at the midpoint, where the zone is widest.
    return fraction * math.sqrt(wavelength_m * d_m / 4)
