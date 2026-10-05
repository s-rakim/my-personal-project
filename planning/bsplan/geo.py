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


#: Effective-earth factor. Atmospheric refraction bends radio waves slightly
#: downward, so they follow a path flatter than the true curvature. Treating the
#: earth as 4/3 its real radius accounts for it, and is the standard assumption
#: in temperate climates. Over water or in ducting conditions it varies, which is
#: one more reason to keep a fade margin.
K_FACTOR = 4.0 / 3.0


def earth_bulge_m(distance_km: float, position: float = 0.5) -> float:
    """Height the earth rises above the straight chord between two points.

    This is the thing that ends long links, and it is invisible on a map. Two
    antennas can be in clear line of sight on paper and still have a hill of
    water in the way: over 20 km of flat ground the earth bulges about 31 m at
    the midpoint, so both masts must clear that before any Fresnel clearance is
    even considered.

    position is where along the path to measure, 0 to 1; the bulge is largest at
    the midpoint.
    """
    if distance_km <= 0:
        return 0.0
    d1 = distance_km * position
    d2 = distance_km * (1 - position)
    # d1*d2 / (2 * k * R), with distances in km and the result in metres.
    return (d1 * d2 * 1000.0) / (2 * K_FACTOR * EARTH_RADIUS_KM)


def radio_horizon_km(height_a_m: float, height_b_m: float) -> float:
    """Furthest the two antennas can see each other over a smooth earth.

    No radio reaches past this, whatever its link budget says, because the planet
    is in the way. A 30 km path needs masts tall enough to see 30 km, and that is
    usually the binding constraint rather than transmit power.
    """
    a = max(0.0, height_a_m)
    b = max(0.0, height_b_m)
    # sqrt(2 * k * R * h), reduced to the familiar 4.12*sqrt(h) for k = 4/3.
    return 4.12 * (math.sqrt(a) + math.sqrt(b))


def required_mast_height_m(distance_km: float, freq_mhz: float,
                           obstacle_height_m: float = 0.0,
                           fraction: float = 0.6) -> float:
    """Mast height both ends need for a clear path over flat ground.

    Adds the earth's bulge at the midpoint to the Fresnel radius there, plus
    anything standing in the way. Assumes a symmetric link over level terrain,
    which is the pessimistic case: real terrain with a rise in the middle needs
    more, and a path across a valley needs much less.

    The result is usually the number that decides whether a link is possible at
    all. A 10 km hop over flat ground needs roughly 15 m at each end before a
    single tree is accounted for.
    """
    if distance_km <= 0:
        return max(0.0, obstacle_height_m)
    return (earth_bulge_m(distance_km)
            + fresnel_clearance_m(distance_km, freq_mhz, fraction)
            + max(0.0, obstacle_height_m))


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
