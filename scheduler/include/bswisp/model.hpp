// Problem and plan types for the airtime solver, mirroring
// schema/scheduler-problem.schema.json and schema/scheduler-plan.schema.json.
//
// The solver is deliberately pure: it is handed achievable rates and demand,
// and it returns an allocation. It does no RF prediction, reads no inventory and
// talks to no network. That keeps it exhaustively testable from fixtures, which
// matters because this is the component that decides what every subscriber gets.
#ifndef BSWISP_MODEL_HPP
#define BSWISP_MODEL_HPP

#include <string>
#include <vector>

#include "bswisp/json.hpp"

namespace bswisp {

// Policy governs how aggressively terminals are moved between sectors.
struct Policy {
  // handoffGainThreshold is the score ratio a rival sector must beat before a
  // working terminal is moved. At 1.0 a terminal chases every marginal
  // improvement and spends its life re-associating instead of passing traffic.
  double handoffGainThreshold = 1.25;

  // minDwellSeconds pins a freshly moved terminal in place. The other half of
  // flap protection, and the one that saves you when two sectors have nearly
  // equal scores.
  double minDwellSeconds = 60.0;

  // stickiness is the bonus a terminal's current sector receives, expressing
  // that an established link is worth more than its bitrate alone.
  double stickiness = 0.15;

  // reserveCommittedFirst reserves every committed rate before any surplus is
  // shared. Disabling it raises average throughput and breaks the only
  // guarantee the business actually sold.
  bool reserveCommittedFirst = true;
};

// Site is a tower. Sites form a tree rooted at the PoP.
struct Site {
  std::string id;
  double backhaulMbps = 0.0;
  std::string parentSiteId;

  // backhaulSectorId names the parent sector carrying this site's relay. When
  // set, the relay is injected into that sector as a synthetic terminal, so it
  // competes for airtime with the subscribers on it, which is what physically
  // happens and what naive relay models miss.
  std::string backhaulSectorId;

  // relayAchievableMbps is the capacity of the relay radio link itself.
  double relayAchievableMbps = 0.0;
};

// Sector is one antenna's coverage and the airtime it has to give.
struct Sector {
  std::string id;
  std::string siteId;

  // airtimeBudget is the schedulable fraction of a second.
  double airtimeBudget = 0.85;

  // dlUlSplit is the share of airtime given to the downlink. Time-division gear
  // shares one airtime pool between both directions, so a sector cannot be
  // busy downstream and idle upstream; frequency-division gear has independent
  // pools and should set this to 1.0 with uplink budgeted separately.
  double dlUlSplit = 0.75;

  int maxTerminals = 0;  // 0 means unlimited
};

// Candidate is one sector a terminal could associate with, and what it would
// get there.
struct Candidate {
  std::string sectorId;
  double achievableDownMbps = 0.0;
  double achievableUpMbps = 0.0;
  double sinrDb = 0.0;
  std::string mcs;
  bool measured = false;
};

// Terminal is one subscriber's CPE for the purposes of this tick.
struct Terminal {
  std::string id;
  int priority = 0;

  double demandDownMbps = 0.0;
  double demandUpMbps = 0.0;
  double ceilingDownMbps = 0.0;
  double ceilingUpMbps = 0.0;
  double committedDownMbps = 0.0;
  double committedUpMbps = 0.0;

  std::string currentSectorId;
  double dwellSeconds = 0.0;
  std::string pinnedSectorId;

  std::vector<Candidate> candidates;

  // isRelay marks a synthetic terminal standing in for a child site's trunk.
  // Relays outrank subscribers for airtime because starving a relay degrades
  // every subscriber behind it.
  bool isRelay = false;
  std::string relaySiteId;

  // weightOverride replaces priority as the fairness weight when non-zero. A
  // relay carries the summed weight of everything behind it, so a trunk serving
  // forty subscribers is not outvoted by one subscriber on the parent tower.
  double weightOverride = 0.0;
};

// Problem is one tick of work.
struct Problem {
  long long epoch = 0;
  std::string generatedAt;
  double tickSeconds = 15.0;
  Policy policy;
  std::vector<Site> sites;
  std::vector<Sector> sectors;
  std::vector<Terminal> terminals;

  // fromJson tolerates unknown fields, per the additive-change rule in
  // schema/README.md.
  static Problem fromJson(const json::Value& v);
};

// LimitedBy records why a grant is not larger. It is the first thing to look at
// when a subscriber complains, and it costs nothing to carry.
enum class LimitedBy { None, Demand, Ceiling, Airtime, Backhaul };

const char* limitedByName(LimitedBy l);

struct Assignment {
  std::string terminalId;
  std::string sectorId;
  double airtimeDown = 0.0;
  double airtimeUp = 0.0;
  double grantDownMbps = 0.0;
  double grantUpMbps = 0.0;
  double sinrDb = 0.0;
  std::string mcs;
  bool committedMet = true;
  LimitedBy limitedBy = LimitedBy::None;
  bool isRelay = false;
};

struct Handoff {
  std::string terminalId;
  std::string fromSectorId;
  std::string toSectorId;
  std::string reason;  // initial | better_rate | congestion | pinned | link_lost
  double gain = 0.0;
};

struct SectorReport {
  std::string sectorId;
  double airtimeUsedDown = 0.0;
  double airtimeUsedUp = 0.0;
  double airtimeBudget = 0.0;
  int terminals = 0;
  double offeredDownMbps = 0.0;
  double grantedDownMbps = 0.0;
  bool oversubscribed = false;
};

struct BackhaulReport {
  std::string siteId;
  double offeredMbps = 0.0;
  double capacityMbps = 0.0;
  double utilization = 0.0;
  bool congested = false;
  int relayDepth = 0;
};

struct Objective {
  double offeredDownMbps = 0.0;
  double grantedDownMbps = 0.0;
  double offeredUpMbps = 0.0;
  double grantedUpMbps = 0.0;
  double committedShortfallMbps = 0.0;
  int unservedTerminals = 0;
};

struct Plan {
  long long epoch = 0;
  std::string solver;
  long long solveMicros = 0;
  Objective objective;
  std::vector<Assignment> assignments;
  std::vector<Handoff> handoffs;
  std::vector<SectorReport> sectors;
  std::vector<BackhaulReport> backhaul;
  std::vector<std::string> warnings;

  json::Value toJson() const;
};

}  // namespace bswisp

#endif  // BSWISP_MODEL_HPP
