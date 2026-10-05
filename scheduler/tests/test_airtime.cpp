// Tests for the airtime solver.
//
// The cases here are the ones that encode the actual thesis of the scheduler:
// that airtime is the scarce resource, that commitments are honoured before
// surplus, that terminals do not flap, and that a relay cannot carry more than
// its trunk. Each one fails loudly if the behaviour regresses.
#include <cmath>
#include <iostream>
#include <string>
#include <vector>

#include "bswisp/airtime.hpp"
#include "bswisp/json.hpp"
#include "bswisp/model.hpp"

namespace {

int g_failures = 0;
std::string g_case;

#define CASE(name)      \
  do {                  \
    g_case = (name);    \
  } while (0)

#define CHECK(cond)                                                            \
  do {                                                                         \
    if (!(cond)) {                                                             \
      std::cerr << "FAIL [" << g_case << "] line " << __LINE__ << ": " << #cond \
                << "\n";                                                       \
      ++g_failures;                                                            \
    }                                                                          \
  } while (0)

#define CHECK_NEAR(actual, expected, tol)                                       \
  do {                                                                          \
    const double a_ = (actual), e_ = (expected);                                 \
    if (std::fabs(a_ - e_) > (tol)) {                                            \
      std::cerr << "FAIL [" << g_case << "] line " << __LINE__ << ": " << #actual \
                << " = " << a_ << ", expected " << e_ << " +/- " << (tol) << "\n";\
      ++g_failures;                                                              \
    }                                                                            \
  } while (0)

using namespace bswisp;

Sector makeSector(const std::string& id, const std::string& siteId, double budget = 1.0) {
  Sector s;
  s.id = id;
  s.siteId = siteId;
  s.airtimeBudget = budget;
  s.dlUlSplit = 1.0;  // treat uplink as an independent pool, keeping tests about downlink
  return s;
}

Site makeSite(const std::string& id, double backhaul = 10000.0) {
  Site s;
  s.id = id;
  s.backhaulMbps = backhaul;
  return s;
}

Terminal makeTerminal(const std::string& id, const std::string& sectorId, double achievable,
                      double demand, double ceiling, double committed = 0.0, int priority = 1) {
  Terminal t;
  t.id = id;
  t.priority = priority;
  t.demandDownMbps = demand;
  t.ceilingDownMbps = ceiling;
  t.committedDownMbps = committed;
  Candidate c;
  c.sectorId = sectorId;
  c.achievableDownMbps = achievable;
  c.achievableUpMbps = achievable;
  t.candidates.push_back(c);
  return t;
}

const Assignment* find(const Plan& p, const std::string& id) {
  for (const Assignment& a : p.assignments) {
    if (a.terminalId == id) return &a;
  }
  return nullptr;
}

const SectorReport* findSector(const Plan& p, const std::string& id) {
  for (const SectorReport& s : p.sectors) {
    if (s.sectorId == id) return &s;
  }
  return nullptr;
}

// ---------------------------------------------------------------------------

void testFillEqualWeights() {
  CASE("max-min fill, equal weights, enough to go round");
  std::vector<double> alloc;
  const double left = WeightedMaxMinFill(alloc, {0.2, 0.3}, {1, 1}, 1.0);
  CHECK_NEAR(alloc[0], 0.2, 1e-9);
  CHECK_NEAR(alloc[1], 0.3, 1e-9);
  CHECK_NEAR(left, 0.5, 1e-9);
}

void testFillScarce() {
  CASE("max-min fill, scarce: small want satisfied, rest shared");
  std::vector<double> alloc;
  // A wants 0.1, B and C want 10 each, only 1.0 available. A is cheap to satisfy
  // outright; the remaining 0.9 splits evenly between B and C.
  const double left = WeightedMaxMinFill(alloc, {0.1, 10.0, 10.0}, {1, 1, 1}, 1.0);
  CHECK_NEAR(alloc[0], 0.1, 1e-9);
  CHECK_NEAR(alloc[1], 0.45, 1e-9);
  CHECK_NEAR(alloc[2], 0.45, 1e-9);
  CHECK_NEAR(left, 0.0, 1e-9);
}

void testFillWeighted() {
  CASE("max-min fill honours weight");
  std::vector<double> alloc;
  WeightedMaxMinFill(alloc, {10.0, 10.0}, {3, 1}, 1.0);
  CHECK_NEAR(alloc[0], 0.75, 1e-9);
  CHECK_NEAR(alloc[1], 0.25, 1e-9);
}

void testFillNothingAvailable() {
  CASE("max-min fill with no budget allocates nothing");
  std::vector<double> alloc;
  const double left = WeightedMaxMinFill(alloc, {1.0, 2.0}, {1, 1}, 0.0);
  CHECK_NEAR(alloc[0], 0.0, 1e-12);
  CHECK_NEAR(alloc[1], 0.0, 1e-12);
  CHECK_NEAR(left, 0.0, 1e-12);
}

// The central claim: a distant terminal gets an equal share of *airtime*, not of
// bandwidth. Bandwidth-fair would hand each 27.5 Mbps, which the slow terminal
// could only deliver by consuming 2.75 seconds of airtime per second.
void testAirtimeFairnessNotBandwidthFairness() {
  CASE("airtime fairness: slow terminal cannot eat the sector");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  p.terminals.push_back(makeTerminal("near", "sec-1", 100.0, 50.0, 100.0));
  p.terminals.push_back(makeTerminal("far", "sec-1", 10.0, 50.0, 100.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* near = find(plan, "near");
  const Assignment* far = find(plan, "far");
  CHECK(near != nullptr && far != nullptr);
  if (near == nullptr || far == nullptr) return;

  CHECK_NEAR(near->airtimeDown, 0.5, 1e-6);
  CHECK_NEAR(far->airtimeDown, 0.5, 1e-6);
  CHECK_NEAR(near->grantDownMbps, 50.0, 1e-6);
  CHECK_NEAR(far->grantDownMbps, 5.0, 1e-6);

  const SectorReport* sec = findSector(plan, "sec-1");
  CHECK(sec != nullptr);
  if (sec != nullptr) {
    CHECK(sec->airtimeUsedDown <= 1.0 + 1e-9);
    CHECK(!sec->oversubscribed);
  }
}

void testCommittedRatesReservedFirst() {
  CASE("committed rates are reserved before surplus is shared");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  // The slow terminal needs 0.8 airtime just for its 8 Mbps commitment. The fast
  // one could absorb the whole sector if allowed to bid first.
  p.terminals.push_back(makeTerminal("slow", "sec-1", 10.0, 10.0, 10.0, 8.0));
  p.terminals.push_back(makeTerminal("fast", "sec-1", 100.0, 100.0, 100.0, 10.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* slow = find(plan, "slow");
  const Assignment* fast = find(plan, "fast");
  CHECK(slow != nullptr && fast != nullptr);
  if (slow == nullptr || fast == nullptr) return;

  CHECK(slow->grantDownMbps >= 8.0 - 1e-6);
  CHECK(fast->grantDownMbps >= 10.0 - 1e-6);
  CHECK(slow->committedMet);
  CHECK(fast->committedMet);
  CHECK_NEAR(plan.objective.committedShortfallMbps, 0.0, 1e-6);
}

// A subscriber idling well below their committed rate is not a breach of it.
// Without this distinction the shortfall metric is non-zero on every healthy
// network and stops meaning anything.
void testIdleSubscriberIsNotABreach() {
  CASE("a subscriber demanding less than their commitment is not a breach");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  // Committed 50 Mbps, but only asking for 2.
  p.terminals.push_back(makeTerminal("idle", "sec-1", 200.0, 2.0, 100.0, 50.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* a = find(plan, "idle");
  CHECK(a != nullptr);
  if (a != nullptr) {
    CHECK_NEAR(a->grantDownMbps, 2.0, 1e-6);
    CHECK(a->committedMet);
    CHECK(a->limitedBy == LimitedBy::Demand);
  }
  CHECK_NEAR(plan.objective.committedShortfallMbps, 0.0, 1e-6);
}

// The converse: wanting the committed rate and not getting it IS a breach, and
// the per-terminal flag and the network total must agree about it.
void testRealBreachIsCountedConsistently() {
  CASE("a real commitment breach is reported by both the flag and the total");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 0.5));
  // Each wants its full 20 Mbps commitment, but 0.5 airtime at 20 Mbps
  // achievable cannot deliver 40 Mbps between them.
  p.terminals.push_back(makeTerminal("a", "sec-1", 20.0, 20.0, 20.0, 20.0));
  p.terminals.push_back(makeTerminal("b", "sec-1", 20.0, 20.0, 20.0, 20.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  CHECK(plan.objective.committedShortfallMbps > 0.0);

  double flagged = 0;
  for (const Assignment& a : plan.assignments) {
    if (!a.committedMet) ++flagged;
  }
  CHECK(flagged > 0);
}

void testOversubscriptionIsReported() {
  CASE("a sector that cannot meet its commitments says so");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 0.5));
  p.terminals.push_back(makeTerminal("a", "sec-1", 10.0, 10.0, 10.0, 8.0));
  p.terminals.push_back(makeTerminal("b", "sec-1", 10.0, 10.0, 10.0, 8.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  const SectorReport* sec = findSector(plan, "sec-1");
  CHECK(sec != nullptr);
  if (sec != nullptr) CHECK(sec->oversubscribed);
  CHECK(plan.objective.committedShortfallMbps > 0.0);
  CHECK(!plan.warnings.empty());

  const Assignment* a = find(plan, "a");
  CHECK(a != nullptr);
  if (a != nullptr) {
    CHECK(!a->committedMet);
    CHECK(a->limitedBy == LimitedBy::Airtime);
  }
}

void testNoFlapForMarginalGain() {
  CASE("hysteresis: a marginally better sector does not win");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  p.sectors.push_back(makeSector("sec-2", "site-1", 1.0));

  Terminal t = makeTerminal("t1", "sec-1", 50.0, 10.0, 100.0);
  Candidate better;
  better.sectorId = "sec-2";
  better.achievableDownMbps = 60.0;
  better.achievableUpMbps = 60.0;
  t.candidates.push_back(better);
  t.currentSectorId = "sec-1";
  t.dwellSeconds = 3600.0;
  p.terminals.push_back(t);

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* a = find(plan, "t1");
  CHECK(a != nullptr);
  if (a != nullptr) CHECK(a->sectorId == "sec-1");
  CHECK(plan.handoffs.empty());
}

void testHandoffForLargeGain() {
  CASE("hysteresis: a clearly better sector does win");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  p.sectors.push_back(makeSector("sec-2", "site-1", 1.0));

  Terminal t = makeTerminal("t1", "sec-1", 50.0, 10.0, 100.0);
  Candidate better;
  better.sectorId = "sec-2";
  better.achievableDownMbps = 400.0;
  better.achievableUpMbps = 400.0;
  t.candidates.push_back(better);
  t.currentSectorId = "sec-1";
  t.dwellSeconds = 3600.0;
  p.terminals.push_back(t);

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* a = find(plan, "t1");
  CHECK(a != nullptr);
  if (a != nullptr) CHECK(a->sectorId == "sec-2");
  CHECK(plan.handoffs.size() == 1);
  if (plan.handoffs.size() == 1) {
    CHECK(plan.handoffs[0].reason == "better_rate");
    CHECK(plan.handoffs[0].fromSectorId == "sec-1");
  }
}

void testMinDwellBlocksHandoff() {
  CASE("a terminal that just moved is not moved again");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  p.sectors.push_back(makeSector("sec-2", "site-1", 1.0));

  Terminal t = makeTerminal("t1", "sec-1", 50.0, 10.0, 100.0);
  Candidate better;
  better.sectorId = "sec-2";
  better.achievableDownMbps = 400.0;
  better.achievableUpMbps = 400.0;
  t.candidates.push_back(better);
  t.currentSectorId = "sec-1";
  t.dwellSeconds = 5.0;  // below the 60s default
  p.terminals.push_back(t);

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* a = find(plan, "t1");
  CHECK(a != nullptr);
  if (a != nullptr) CHECK(a->sectorId == "sec-1");
  CHECK(plan.handoffs.empty());
}

void testBackhaulCaps() {
  CASE("backhaul capacity caps a sector that has plenty of airtime");
  Problem p;
  p.sites.push_back(makeSite("site-1", 20.0));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  p.terminals.push_back(makeTerminal("t1", "sec-1", 200.0, 200.0, 200.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* a = find(plan, "t1");
  CHECK(a != nullptr);
  if (a != nullptr) {
    CHECK_NEAR(a->grantDownMbps, 20.0, 1e-6);
    CHECK(a->limitedBy == LimitedBy::Backhaul);
  }
  CHECK(plan.backhaul.size() == 1);
  if (!plan.backhaul.empty()) CHECK(plan.backhaul[0].congested);
}

void testUnservedTerminalIsCounted() {
  CASE("a terminal with no viable candidate is reported, not dropped silently");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  // Zero achievable rate: the link does not close.
  p.terminals.push_back(makeTerminal("t1", "sec-1", 0.0, 10.0, 100.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  CHECK(plan.objective.unservedTerminals == 1);
  CHECK(plan.assignments.empty());
}

// A relay cannot deliver more than its trunk, however much airtime the far
// sector has. This is the constraint relay networks are built on and the one
// that is easiest to model wrongly.
void testRelayTrunkCapsSubtree() {
  CASE("a relay subtree is capped by the trunk's capacity");
  Problem p;
  p.sites.push_back(makeSite("hub", 10000.0));

  Site leaf = makeSite("leaf", 10000.0);
  leaf.parentSiteId = "hub";
  leaf.backhaulSectorId = "sec-hub";
  leaf.relayAchievableMbps = 50.0;
  p.sites.push_back(leaf);

  p.sectors.push_back(makeSector("sec-hub", "hub", 1.0));
  p.sectors.push_back(makeSector("sec-leaf", "leaf", 1.0));

  // The leaf sector could serve 200 Mbps; the trunk is 50.
  p.terminals.push_back(makeTerminal("behind-relay", "sec-leaf", 200.0, 200.0, 200.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* a = find(plan, "behind-relay");
  CHECK(a != nullptr);
  if (a != nullptr) {
    CHECK(a->grantDownMbps <= 50.0 + 1e-6);
    CHECK(a->limitedBy == LimitedBy::Backhaul);
  }

  const Assignment* relay = find(plan, "relay:leaf");
  CHECK(relay != nullptr);
  if (relay != nullptr) {
    CHECK(relay->isRelay);
    CHECK(relay->sectorId == "sec-hub");
  }
}

// The relay flag has to survive serialisation. It did not once, and every
// consumer downstream then counted the network's own plumbing as a customer.
void testRelayFlagIsSerialised() {
  CASE("is_relay survives the JSON round trip");
  Problem p;
  p.sites.push_back(makeSite("hub", 10000.0));

  Site leaf = makeSite("leaf", 10000.0);
  leaf.parentSiteId = "hub";
  leaf.backhaulSectorId = "sec-hub";
  leaf.relayAchievableMbps = 100.0;
  p.sites.push_back(leaf);

  p.sectors.push_back(makeSector("sec-hub", "hub", 1.0));
  p.sectors.push_back(makeSector("sec-leaf", "leaf", 1.0));
  p.terminals.push_back(makeTerminal("real-customer", "sec-leaf", 100.0, 50.0, 100.0));

  const json::Value doc = json::parse(json::dump(AirtimeScheduler().Solve(p).toJson()));

  int relays = 0;
  int customers = 0;
  for (const json::Value& a : doc["assignments"].items()) {
    CHECK(a.has("is_relay"));
    if (a["is_relay"].asBool()) {
      ++relays;
    } else {
      ++customers;
    }
  }
  CHECK(relays == 1);
  CHECK(customers == 1);
}

void testRelayCompetesForParentAirtime() {
  CASE("a relay competes for the parent sector's airtime");
  Problem p;
  p.sites.push_back(makeSite("hub", 10000.0));

  Site leaf = makeSite("leaf", 10000.0);
  leaf.parentSiteId = "hub";
  leaf.backhaulSectorId = "sec-hub";
  leaf.relayAchievableMbps = 100.0;
  p.sites.push_back(leaf);

  p.sectors.push_back(makeSector("sec-hub", "hub", 1.0));
  p.sectors.push_back(makeSector("sec-leaf", "leaf", 1.0));

  // A hungry subscriber directly on the hub, plus a relay carrying another.
  p.terminals.push_back(makeTerminal("on-hub", "sec-hub", 100.0, 100.0, 100.0));
  p.terminals.push_back(makeTerminal("behind-relay", "sec-leaf", 100.0, 100.0, 100.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  const SectorReport* hub = findSector(plan, "sec-hub");
  CHECK(hub != nullptr);
  if (hub != nullptr) {
    // Two claimants on one sector: neither can have all of it.
    CHECK(hub->airtimeUsedDown <= 1.0 + 1e-9);
    CHECK(hub->terminals == 2);
  }
  const Assignment* onHub = find(plan, "on-hub");
  CHECK(onHub != nullptr);
  if (onHub != nullptr) CHECK(onHub->grantDownMbps < 100.0);
}

void testPinIsHonoured() {
  CASE("an operator pin overrides the scheduler's preference");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 1.0));
  p.sectors.push_back(makeSector("sec-2", "site-1", 1.0));

  Terminal t = makeTerminal("t1", "sec-1", 50.0, 10.0, 100.0);
  Candidate better;
  better.sectorId = "sec-2";
  better.achievableDownMbps = 500.0;
  better.achievableUpMbps = 500.0;
  t.candidates.push_back(better);
  t.pinnedSectorId = "sec-1";
  p.terminals.push_back(t);

  const Plan plan = AirtimeScheduler().Solve(p);
  const Assignment* a = find(plan, "t1");
  CHECK(a != nullptr);
  if (a != nullptr) CHECK(a->sectorId == "sec-1");
}

void testBackhaulCycleIsReported() {
  CASE("a backhaul cycle is reported, not looped on");
  Problem p;
  Site a = makeSite("a");
  a.parentSiteId = "b";
  Site b = makeSite("b");
  b.parentSiteId = "a";
  p.sites.push_back(a);
  p.sites.push_back(b);
  p.sectors.push_back(makeSector("sec-a", "a", 1.0));

  const Plan plan = AirtimeScheduler().Solve(p);
  bool found = false;
  for (const std::string& w : plan.warnings) {
    if (w.find("cycle") != std::string::npos) found = true;
  }
  CHECK(found);
}

void testDeterminism() {
  CASE("identical problems produce byte-identical plans");
  Problem p;
  p.sites.push_back(makeSite("site-1"));
  p.sectors.push_back(makeSector("sec-1", "site-1", 0.85));
  p.sectors.push_back(makeSector("sec-2", "site-1", 0.85));
  for (int i = 0; i < 40; ++i) {
    Terminal t = makeTerminal("t" + std::to_string(i), "sec-1", 40.0 + i, 20.0, 100.0,
                              (i % 3 == 0) ? 5.0 : 0.0, (i % 5) + 1);
    Candidate alt;
    alt.sectorId = "sec-2";
    alt.achievableDownMbps = 100.0 - i;
    alt.achievableUpMbps = 50.0;
    t.candidates.push_back(alt);
    p.terminals.push_back(t);
  }

  Plan a = AirtimeScheduler().Solve(p);
  Plan b = AirtimeScheduler().Solve(p);
  // Wall-clock solve time is observational and varies by run; every decision in
  // the plan must not. Normalise the timing and compare everything else.
  a.solveMicros = 0;
  b.solveMicros = 0;
  const std::string first = json::dump(a.toJson());
  const std::string second = json::dump(b.toJson());
  CHECK(first == second);
  if (first != second) {
    std::cerr << "  first:  " << first.substr(0, 400) << "\n";
    std::cerr << "  second: " << second.substr(0, 400) << "\n";
  }
}

void testJsonRoundTrip() {
  CASE("JSON survives a parse and emit cycle");
  const std::string src =
      R"({"epoch":7,"tick_seconds":15,"policy":{"handoff_gain_threshold":1.3,)"
      R"("min_dwell_seconds":30,"stickiness":0.2},"sites":[{"id":"s","backhaul_mbps":1000}],)"
      R"("sectors":[{"id":"x","site_id":"s","airtime_budget":0.85,"dl_ul_split":0.75}],)"
      R"("terminals":[{"id":"t","priority":2,"demand_down_mbps":12.5,"demand_up_mbps":2,)"
      R"("ceiling_down_mbps":50,"ceiling_up_mbps":10,"candidates":[{"sector_id":"x",)"
      R"("achievable_down_mbps":80,"achievable_up_mbps":40,"sinr_db":22.5,"measured":true}]}]})";

  const json::Value doc = json::parse(src);
  CHECK(doc["epoch"].asInt() == 7);
  CHECK(doc["policy"]["handoff_gain_threshold"].asNumber() == 1.3);

  const Problem p = Problem::fromJson(doc);
  CHECK(p.epoch == 7);
  CHECK(p.sites.size() == 1);
  CHECK(p.sectors.size() == 1);
  CHECK(p.terminals.size() == 1);
  CHECK_NEAR(p.sectors[0].dlUlSplit, 0.75, 1e-12);
  CHECK(p.terminals[0].candidates.size() == 1);
  CHECK(p.terminals[0].candidates[0].measured);

  // Re-emitting and re-parsing must be stable.
  const std::string again = json::dump(doc);
  CHECK(json::dump(json::parse(again)) == again);
}

void testJsonRejectsGarbage() {
  CASE("malformed JSON is rejected with a position");
  bool threw = false;
  try {
    json::parse("{\"a\": }");
  } catch (const json::ParseError&) {
    threw = true;
  }
  CHECK(threw);

  threw = false;
  try {
    json::parse("[1,2,3]extra");
  } catch (const json::ParseError&) {
    threw = true;
  }
  CHECK(threw);
}

void testJsonUnicode() {
  CASE("unicode escapes and surrogate pairs decode to UTF-8");
  const json::Value v = json::parse(R"({"name":"café 🚀"})");
  const std::string s = v["name"].asString();
  CHECK(s == "caf\xc3\xa9 \xf0\x9f\x9a\x80");
}

}  // namespace

int main() {
  testFillEqualWeights();
  testFillScarce();
  testFillWeighted();
  testFillNothingAvailable();
  testAirtimeFairnessNotBandwidthFairness();
  testCommittedRatesReservedFirst();
  testIdleSubscriberIsNotABreach();
  testRealBreachIsCountedConsistently();
  testOversubscriptionIsReported();
  testNoFlapForMarginalGain();
  testHandoffForLargeGain();
  testMinDwellBlocksHandoff();
  testBackhaulCaps();
  testUnservedTerminalIsCounted();
  testRelayTrunkCapsSubtree();
  testRelayFlagIsSerialised();
  testRelayCompetesForParentAirtime();
  testPinIsHonoured();
  testBackhaulCycleIsReported();
  testDeterminism();
  testJsonRoundTrip();
  testJsonRejectsGarbage();
  testJsonUnicode();

  if (g_failures == 0) {
    std::cout << "all scheduler tests passed\n";
    return 0;
  }
  std::cerr << g_failures << " check(s) failed\n";
  return 1;
}
