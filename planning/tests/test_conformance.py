"""The Python side of the link budget conformance check.

Go and Python must agree exactly. A planner that disagrees with the live
scheduler produces confident capacity figures the network will not honour, and
nobody finds out until the subscribers do.
"""

from __future__ import annotations

import json
import math
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from bsplan.capacity import DemandProfile, range_for_rate_km, sector_capacity  # noqa: E402
from bsplan.geo import Point, angle_diff_deg  # noqa: E402
from bsplan.inventory import Sector  # noqa: E402
from bsplan.linkbudget import LinkBudget, airtime_fraction, select_mcs  # noqa: E402

FIXTURE = Path(__file__).resolve().parents[2] / "schema" / "fixtures" / "linkbudget-cases.json"


class TestLinkBudgetConformance(unittest.TestCase):
    def setUp(self) -> None:
        self.doc = json.loads(FIXTURE.read_text())
        self.tol = self.doc.get("tolerance", 1e-6)

    def test_every_case_matches(self) -> None:
        self.assertTrue(self.doc["cases"], "fixture holds no cases")

        for case in self.doc["cases"]:
            with self.subTest(case["name"]):
                budget = LinkBudget(**case["input"])
                expect = case["expect"]

                self.assertAlmostEqual(budget.eirp_dbm(), expect["eirp_dbm"], delta=self.tol)
                self.assertAlmostEqual(budget.rx_power_dbm(), expect["rx_power_dbm"], delta=self.tol)
                self.assertAlmostEqual(budget.sinr_db(), expect["sinr_db"], delta=self.tol)
                self.assertAlmostEqual(
                    budget.achievable_mbps(), expect["achievable_mbps"], delta=self.tol
                )

                mcs = select_mcs(budget.sinr_db())
                self.assertEqual(mcs.name if mcs else "", expect["mcs"])


class TestAirtime(unittest.TestCase):
    def test_slower_link_costs_more_airtime(self) -> None:
        fast = airtime_fraction(10, 200)
        slow = airtime_fraction(10, 25)
        self.assertGreater(slow, fast)
        self.assertAlmostEqual(slow / fast, 8.0, places=9)

    def test_dead_link_costs_infinite_airtime(self) -> None:
        self.assertEqual(airtime_fraction(10, 0), math.inf)

    def test_no_demand_costs_nothing(self) -> None:
        self.assertEqual(airtime_fraction(0, 100), 0.0)


class TestGeo(unittest.TestCase):
    def test_destination_round_trip(self) -> None:
        start = Point(44.9412, -93.0998, 42)
        for bearing in (0, 45, 90, 180, 270, 359):
            for dist in (0.5, 5.0, 50.0):
                end = start.destination(bearing, dist)
                self.assertAlmostEqual(start.distance_km(end), dist, places=6)
                self.assertLess(angle_diff_deg(start.bearing_deg(end), bearing), 1e-6)

    def test_elevation_is_negative_looking_down(self) -> None:
        tower = Point(44.94, -93.10, 40)
        house = tower.destination(0, 1.0)
        house = Point(house.lat, house.lon, 6)
        self.assertLess(tower.elevation_angle_deg(house), 0)
        self.assertGreater(house.elevation_angle_deg(tower), 0)


class TestCapacityModel(unittest.TestCase):
    def _sector(self) -> Sector:
        return Sector(
            id="s", site_id="x", azimuth_deg=0, beamwidth_deg=90,
            freq_mhz=5775, channel_width_mhz=40, tx_power_dbm=25,
            antenna_gain_dbi=17, feeder_loss_db=1.5, clutter_loss_db=8,
            fade_margin_db=10, interference_dbm=-92, airtime_budget=0.85,
            protocol_efficiency=0.75, dl_ul_split=0.75,
        )

    def test_design_range_is_shorter_than_link_range(self) -> None:
        """The distinction the capacity model depends on.

        A link closes well past the point where it can still carry a plan. Plan
        coverage to the link-closing radius and the model fills the sector with
        edge subscribers on the slowest modulation.
        """
        sector = self._sector()
        link = range_for_rate_km(sector, 0.0)
        design = range_for_rate_km(sector, 100.0)
        self.assertGreater(link, design)
        self.assertGreater(design, 0)

    def test_higher_contention_supports_more_subscribers(self) -> None:
        sector = self._sector()
        tight = sector_capacity(sector, DemandProfile(100, contention_ratio=10))
        loose = sector_capacity(sector, DemandProfile(100, contention_ratio=40))
        self.assertGreater(loose.subscribers, tight.subscribers)

    def test_offered_load_never_exceeds_what_the_sector_delivers(self) -> None:
        """Internal consistency: you cannot sell more than the airtime carries."""
        sector = self._sector()
        cap = sector_capacity(sector, DemandProfile(100, contention_ratio=20))
        self.assertLessEqual(cap.offered_mbps, cap.aggregate_mbps + 1e-6)

    def test_airtime_budget_is_respected(self) -> None:
        sector = self._sector()
        cap = sector_capacity(sector, DemandProfile(100, contention_ratio=20))
        used = cap.subscribers * cap.mean_airtime_per_subscriber
        self.assertLessEqual(used, cap.downlink_airtime + 1e-9)

    def test_unreachable_sector_supports_nobody(self) -> None:
        sector = self._sector()
        sector.clutter_loss_db = 80  # buried
        cap = sector_capacity(sector, DemandProfile(100))
        self.assertEqual(cap.subscribers, 0)


if __name__ == "__main__":
    unittest.main(verbosity=2)
