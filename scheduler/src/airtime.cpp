#include "bswisp/airtime.hpp"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <limits>
#include <numeric>
#include <sstream>
#include <unordered_map>

namespace bswisp {

const char* const kSolverVersion = "bswisp-airtime/1.0 (C++17)";

namespace {

constexpr double kEpsilon = 1e-9;
constexpr double kRateEpsilon = 1e-6;  // Mbps below which a difference is noise

// kMaxRelayDepth bounds the relay chain. Every hop adds latency and another
// shared trunk; past a handful of hops the network is a liability rather than
// an asset, so refusing to model it deeply is a feature.
constexpr int kMaxRelayDepth = 8;

double weightOf(const Terminal& t) {
  if (t.weightOverride > 0) return t.weightOverride;
  double w = static_cast<double>(t.priority);
  return w > 0 ? w : 1.0;
}

// effectiveWant is what a terminal can actually use: its offered load, capped by
// what it pays for. Allocating to the ceiling instead would reserve capacity for
// subscribers who are asleep.
double effectiveWant(double demand, double ceiling) {
  return ceiling > 0 ? std::min(demand, ceiling) : demand;
}

struct SectorState {
  const Sector* sector = nullptr;
  int siteDepth = 0;
  std::size_t siteIdx = 0;

  // reservedDown counts committed airtime claimed during assignment.
  // pressureDown counts demand-driven load and steers balancing only. They are
  // separate so a best-effort network, which has no commitments at all, still
  // balances across sectors.
  double reservedDown = 0.0;
  double pressureDown = 0.0;
  int count = 0;

  std::vector<std::size_t> terms;

  double usedDown = 0.0;
  double usedUp = 0.0;
  double offeredDown = 0.0;
  double grantedDown = 0.0;
  bool oversubscribed = false;

  double budgetDown() const { return sector->airtimeBudget * sector->dlUlSplit; }

  double budgetUp() const {
    // Time-division gear splits one airtime pool between directions.
    // Frequency-division gear has an independent uplink; dlUlSplit >= 1.0 is
    // how the inventory signals that.
    if (sector->dlUlSplit >= 1.0) return sector->airtimeBudget;
    return sector->airtimeBudget * (1.0 - sector->dlUlSplit);
  }
};

struct SiteState {
  const Site* site = nullptr;
  int depth = 0;
  std::vector<std::size_t> sectors;
  std::vector<std::size_t> wiredChildren;
  std::size_t relayTermIdx = kNoTerm;
  double ownGranted = 0.0;
  double subtreeGranted = 0.0;
  double subtreeCommitted = 0.0;
  // effectiveCapacity is the ceiling actually enforced on this site, which for a
  // relay site is its trunk radio rather than any wired figure in inventory.
  // Reported as-is so a dashboard never shows a congested site sitting at 9%.
  double effectiveCapacity = 0.0;
  bool congested = false;

  static constexpr std::size_t kNoTerm = static_cast<std::size_t>(-1);
};

struct TermState {
  const Candidate* cand = nullptr;
  std::string sectorId;
  std::size_t sectorIdx = 0;
  bool assigned = false;

  double cirGrantDown = 0.0;  // the part of grantDown that is a commitment
  double airtimeDown = 0.0;
  double airtimeUp = 0.0;
  double grantDown = 0.0;
  double grantUp = 0.0;
  bool committedMet = true;
  LimitedBy limitedBy = LimitedBy::None;
};

const Candidate* findCandidate(const Terminal& t, const std::string& sectorId) {
  for (const Candidate& c : t.candidates) {
    if (c.sectorId == sectorId) return &c;
  }
  return nullptr;
}

// scoreCandidate ranks a sector for a terminal. Raw achievable rate dominates,
// discounted by how loaded the sector already is and by whether it can still
// honour this terminal's commitment, with a bonus for staying put.
double scoreCandidate(const Terminal& t, const Candidate& c, const SectorState& st,
                      const Policy& policy) {
  if (c.achievableDownMbps <= 0) return 0.0;

  const double budget = st.budgetDown();
  const double cirAirtime = t.committedDownMbps > 0 ? t.committedDownMbps / c.achievableDownMbps : 0.0;
  const double remaining = budget - st.reservedDown;

  // A sector that cannot honour the commitment is heavily penalised but not
  // excluded: a degraded link beats no link, and the plan reports the breach.
  const double feasibility = (cirAirtime <= remaining + kEpsilon) ? 1.0 : 0.25;

  const double load = budget > 0 ? st.pressureDown / budget : 1.0;
  const double balance = 1.0 / (1.0 + std::max(0.0, load));
  const double stick = (t.currentSectorId == c.sectorId) ? (1.0 + policy.stickiness) : 1.0;

  return c.achievableDownMbps * feasibility * balance * stick;
}

}  // namespace

double WeightedMaxMinFill(std::vector<double>& alloc, const std::vector<double>& want,
                          const std::vector<double>& weight, double available) {
  alloc.assign(want.size(), 0.0);
  if (want.empty() || available <= 0) return std::max(0.0, available);

  std::vector<std::size_t> order(want.size());
  std::iota(order.begin(), order.end(), 0);

  // Cheapest-to-satisfy first, measured per unit of weight. Ties break on index
  // so the result is reproducible from a captured problem.
  std::stable_sort(order.begin(), order.end(), [&](std::size_t a, std::size_t b) {
    const double ra = weight[a] > 0 ? want[a] / weight[a] : std::numeric_limits<double>::infinity();
    const double rb = weight[b] > 0 ? want[b] / weight[b] : std::numeric_limits<double>::infinity();
    if (ra != rb) return ra < rb;
    return a < b;
  });

  double activeWeight = 0.0;
  for (std::size_t i = 0; i < want.size(); ++i) {
    if (weight[i] > 0 && want[i] > 0) activeWeight += weight[i];
  }

  double left = available;
  for (std::size_t pos = 0; pos < order.size(); ++pos) {
    const std::size_t i = order[pos];
    if (weight[i] <= 0 || want[i] <= 0) continue;
    if (activeWeight <= 0) break;

    const double lambda = want[i] / weight[i];
    if (lambda * activeWeight <= left + kEpsilon) {
      // Everyone still unsaturated could be raised to this level, so terminal i
      // gets everything it asked for and drops out.
      alloc[i] = want[i];
      left -= want[i];
      activeWeight -= weight[i];
    } else {
      // Budget runs out below this level: share what is left in proportion to
      // weight and stop.
      const double lam = left / activeWeight;
      for (std::size_t q = pos; q < order.size(); ++q) {
        const std::size_t j = order[q];
        if (weight[j] <= 0 || want[j] <= 0) continue;
        alloc[j] = std::min(want[j], lam * weight[j]);
      }
      left = 0.0;
      break;
    }
  }
  return std::max(0.0, left);
}

namespace {

// allocateDirection runs the two-phase allocation for one direction of one
// sector, writing grants and airtime back into TermState.
void allocateDirection(SectorState& st, const std::vector<Terminal>& terms,
                       std::vector<TermState>& ts, const Policy& policy, bool down,
                       std::vector<std::string>& warnings) {
  const std::size_t n = st.terms.size();
  if (n == 0) return;

  auto achievable = [&](std::size_t i) {
    return down ? ts[i].cand->achievableDownMbps : ts[i].cand->achievableUpMbps;
  };
  auto demandOf = [&](std::size_t i) {
    return down ? terms[i].demandDownMbps : terms[i].demandUpMbps;
  };
  auto ceilingOf = [&](std::size_t i) {
    return down ? terms[i].ceilingDownMbps : terms[i].ceilingUpMbps;
  };
  auto committedOf = [&](std::size_t i) {
    return down ? terms[i].committedDownMbps : terms[i].committedUpMbps;
  };

  const double budget = down ? st.budgetDown() : st.budgetUp();

  // Phase 1: reserve committed rates.
  std::vector<double> cirAir(n, 0.0);
  double totalCir = 0.0;
  for (std::size_t k = 0; k < n; ++k) {
    const std::size_t i = st.terms[k];
    const double a = achievable(i);
    double cir = committedOf(i);
    if (ceilingOf(i) > 0) cir = std::min(cir, ceilingOf(i));
    const double air = (a > 0 && cir > 0) ? cir / a : 0.0;
    cirAir[k] = air;
    totalCir += air;
  }

  std::vector<double> baseAir(n, 0.0);
  const bool over = policy.reserveCommittedFirst && totalCir > budget + kEpsilon;
  if (over) {
    st.oversubscribed = true;
    const double scale = totalCir > 0 ? budget / totalCir : 0.0;
    for (std::size_t k = 0; k < n; ++k) baseAir[k] = cirAir[k] * scale;
    if (down) {
      std::ostringstream msg;
      msg << "sector " << st.sector->id << " is oversubscribed: committed rates need "
          << totalCir << " airtime but only " << budget
          << " is available. This needs another channel or fewer subscribers, "
             "not a scheduler change.";
      warnings.push_back(msg.str());
    }
  } else if (policy.reserveCommittedFirst) {
    baseAir = cirAir;
  }
  // When reserveCommittedFirst is off, baseAir stays zero and commitments take
  // their chances in the fair fill alongside everything else.

  double usedBase = 0.0;
  for (double a : baseAir) usedBase += a;
  const double remaining = std::max(0.0, budget - usedBase);

  // Phase 2: weighted max-min fair fill of whatever demand the reservation did
  // not already cover.
  std::vector<double> want(n, 0.0), weight(n, 0.0), extra;
  for (std::size_t k = 0; k < n; ++k) {
    const std::size_t i = st.terms[k];
    const double a = achievable(i);
    const double target = effectiveWant(demandOf(i), ceilingOf(i));
    const double have = baseAir[k] * a;
    const double residual = std::max(0.0, target - have);
    want[k] = a > 0 ? residual / a : 0.0;
    weight[k] = weightOf(terms[i]);
  }
  WeightedMaxMinFill(extra, want, weight, remaining);

  // Convert airtime back into rate, clamp to what the terminal can use, then
  // recompute airtime from the clamped rate so the plan never claims to have
  // spent airtime it did not.
  for (std::size_t k = 0; k < n; ++k) {
    const std::size_t i = st.terms[k];
    const double a = achievable(i);
    const double target = effectiveWant(demandOf(i), ceilingOf(i));

    double air = baseAir[k] + extra[k];
    double grant = a > 0 ? air * a : 0.0;
    if (grant > target) {
      grant = target;
      air = a > 0 ? grant / a : 0.0;
    }

    LimitedBy limited;
    if (grant >= target - kRateEpsilon) {
      const double ceil = ceilingOf(i);
      limited = (ceil > 0 && ceil < demandOf(i) - kRateEpsilon) ? LimitedBy::Ceiling
                                                                : LimitedBy::Demand;
    } else {
      limited = LimitedBy::Airtime;
    }

    // A commitment is only breached when the subscriber wanted it and did not
    // get it. Someone idling at 2 Mbps on a 50 Mbps committed rate is not being
    // short-changed, and counting them as a breach would leave the shortfall
    // metric permanently non-zero -- which makes the one number that should mean
    // "we are failing a contract" mean nothing at all.
    const double committed = committedOf(i);
    const bool met = committed <= 0 || grant >= std::min(committed, target) - kRateEpsilon;

    if (down) {
      ts[i].airtimeDown = air;
      ts[i].grantDown = grant;
      ts[i].cirGrantDown = std::min(grant, committed);
      ts[i].limitedBy = limited;
      ts[i].committedMet = met;
      st.usedDown += air;
      st.offeredDown += target;
      st.grantedDown += grant;
    } else {
      ts[i].airtimeUp = air;
      ts[i].grantUp = grant;
      // A downlink breach is the one that gets reported; uplink shortfall only
      // downgrades the flag, never upgrades it.
      ts[i].committedMet = ts[i].committedMet && met;
      st.usedUp += air;
    }
  }
}

}  // namespace

Plan AirtimeScheduler::Solve(const Problem& problem) const {
  const auto started = std::chrono::steady_clock::now();

  Plan plan;
  plan.epoch = problem.epoch;
  plan.solver = kSolverVersion;

  // ---- index sites and sectors ----
  std::unordered_map<std::string, std::size_t> siteIdx, sectorIdx;
  std::vector<SiteState> sites;
  std::vector<SectorState> sectors;

  sites.reserve(problem.sites.size());
  for (const Site& s : problem.sites) {
    if (s.id.empty() || siteIdx.count(s.id)) {
      plan.warnings.push_back("ignoring site with empty or duplicate id: '" + s.id + "'");
      continue;
    }
    siteIdx[s.id] = sites.size();
    SiteState st;
    st.site = &s;
    sites.push_back(st);
  }

  sectors.reserve(problem.sectors.size());
  for (const Sector& sec : problem.sectors) {
    if (sec.id.empty() || sectorIdx.count(sec.id)) {
      plan.warnings.push_back("ignoring sector with empty or duplicate id: '" + sec.id + "'");
      continue;
    }
    auto it = siteIdx.find(sec.siteId);
    if (it == siteIdx.end()) {
      plan.warnings.push_back("sector " + sec.id + " references unknown site " + sec.siteId +
                              "; sector dropped");
      continue;
    }
    sectorIdx[sec.id] = sectors.size();
    SectorState st;
    st.sector = &sec;
    st.siteIdx = it->second;
    sectors.push_back(st);
    sites[it->second].sectors.push_back(sectors.size() - 1);
  }

  // ---- backhaul depth, with cycle detection ----
  for (std::size_t i = 0; i < sites.size(); ++i) {
    int depth = 0;
    std::vector<std::string> seen{sites[i].site->id};
    std::string cur = sites[i].site->parentSiteId;
    while (!cur.empty()) {
      if (std::find(seen.begin(), seen.end(), cur) != seen.end()) {
        plan.warnings.push_back("backhaul cycle through site " + cur +
                                "; treating " + sites[i].site->id + " as a root");
        depth = 0;
        break;
      }
      auto it = siteIdx.find(cur);
      if (it == siteIdx.end()) {
        plan.warnings.push_back("site " + sites[i].site->id + " has unknown parent " + cur);
        break;
      }
      seen.push_back(cur);
      ++depth;
      if (depth > kMaxRelayDepth) {
        plan.warnings.push_back("relay chain from site " + sites[i].site->id +
                                " exceeds " + std::to_string(kMaxRelayDepth) + " hops");
        break;
      }
      cur = sites[it->second].site->parentSiteId;
    }
    sites[i].depth = depth;
  }
  for (SectorState& sec : sectors) sec.siteDepth = sites[sec.siteIdx].depth;

  // Children reaching their parent over a wire consume the parent's backhaul but
  // not its airtime. Children relaying over the air do both, and are handled by
  // injection below.
  for (std::size_t i = 0; i < sites.size(); ++i) {
    const Site* s = sites[i].site;
    if (s->parentSiteId.empty() || !s->backhaulSectorId.empty()) continue;
    auto it = siteIdx.find(s->parentSiteId);
    if (it != siteIdx.end()) sites[it->second].wiredChildren.push_back(i);
  }

  // ---- assignment ----
  std::vector<Terminal> terms;
  // Reserve up front and never exceed it. TermState::cand points into
  // terms[i].candidates, and while vector growth would move the Terminal
  // objects rather than their candidate buffers, depending on that is too subtle
  // to leave to chance. At most one relay terminal is appended per site, so this
  // bound is exact.
  terms.reserve(problem.terminals.size() + problem.sites.size() + 1);
  terms.assign(problem.terminals.begin(), problem.terminals.end());
  std::vector<TermState> ts(terms.size());

  std::vector<std::size_t> order(terms.size());
  std::iota(order.begin(), order.end(), 0);
  // Hardest to place first: high priority, then few options, then large
  // commitments. Fully deterministic, so a captured problem always replays.
  std::stable_sort(order.begin(), order.end(), [&](std::size_t a, std::size_t b) {
    if (terms[a].priority != terms[b].priority) return terms[a].priority > terms[b].priority;
    if (terms[a].candidates.size() != terms[b].candidates.size()) {
      return terms[a].candidates.size() < terms[b].candidates.size();
    }
    if (terms[a].committedDownMbps != terms[b].committedDownMbps) {
      return terms[a].committedDownMbps > terms[b].committedDownMbps;
    }
    return terms[a].id < terms[b].id;
  });

  for (std::size_t i : order) {
    const Terminal& t = terms[i];

    // An operator pin wins outright. If it costs the subscriber their committed
    // rate the plan says so rather than quietly overriding the pin.
    if (!t.pinnedSectorId.empty()) {
      auto si = sectorIdx.find(t.pinnedSectorId);
      const Candidate* c = findCandidate(t, t.pinnedSectorId);
      if (si != sectorIdx.end() && c != nullptr && c->achievableDownMbps > 0) {
        ts[i].cand = c;
        ts[i].sectorId = c->sectorId;
        ts[i].sectorIdx = si->second;
        ts[i].assigned = true;
      } else {
        plan.warnings.push_back("terminal " + t.id + " is pinned to " + t.pinnedSectorId +
                                " but that sector is not a viable candidate; "
                                "falling back to automatic selection");
      }
    }

    if (!ts[i].assigned) {
      double bestScore = 0.0;
      const Candidate* best = nullptr;
      std::size_t bestIdx = 0;

      for (const Candidate& c : t.candidates) {
        auto si = sectorIdx.find(c.sectorId);
        if (si == sectorIdx.end() || c.achievableDownMbps <= 0) continue;
        SectorState& st = sectors[si->second];

        // Respect the sector's terminal limit, but never evict an incumbent.
        const bool incumbent = (t.currentSectorId == c.sectorId);
        if (st.sector->maxTerminals > 0 && st.count >= st.sector->maxTerminals && !incumbent) {
          continue;
        }
        const double score = scoreCandidate(t, c, st, problem.policy);
        if (score > bestScore) {
          bestScore = score;
          best = &c;
          bestIdx = si->second;
        }
      }

      if (best == nullptr) {
        plan.objective.unservedTerminals++;
        continue;
      }

      // Hysteresis: a working link is only given up for a clearly better one,
      // and never before it has been held for the minimum dwell.
      std::string reason = "initial";
      double gain = 0.0;
      const Candidate* currentCand = t.currentSectorId.empty()
                                         ? nullptr
                                         : findCandidate(t, t.currentSectorId);
      auto curIt = t.currentSectorId.empty() ? sectorIdx.end()
                                             : sectorIdx.find(t.currentSectorId);

      if (currentCand != nullptr && curIt != sectorIdx.end() &&
          currentCand->achievableDownMbps > 0) {
        const double currentScore =
            scoreCandidate(t, *currentCand, sectors[curIt->second], problem.policy);
        gain = currentScore > 0 ? bestScore / currentScore : 0.0;

        const bool dwellHeld = t.dwellSeconds < problem.policy.minDwellSeconds;
        const bool worthMoving = currentScore <= 0 ||
                                 bestScore > currentScore * problem.policy.handoffGainThreshold;

        if (dwellHeld || !worthMoving) {
          best = currentCand;
          bestIdx = curIt->second;
          reason.clear();
        } else {
          // Distinguish "somewhere better appeared" from "here got crowded",
          // because they call for different operator responses.
          const SectorState& cur = sectors[curIt->second];
          const double load = cur.budgetDown() > 0 ? cur.pressureDown / cur.budgetDown() : 0.0;
          reason = load > 0.9 ? "congestion" : "better_rate";
        }
      } else if (!t.currentSectorId.empty()) {
        reason = "link_lost";
      }

      ts[i].cand = best;
      ts[i].sectorId = best->sectorId;
      ts[i].sectorIdx = bestIdx;
      ts[i].assigned = true;

      if (!reason.empty() && best->sectorId != t.currentSectorId) {
        Handoff h;
        h.terminalId = t.id;
        h.fromSectorId = t.currentSectorId;
        h.toSectorId = best->sectorId;
        h.reason = reason;
        h.gain = gain;
        plan.handoffs.push_back(h);
      }
    }

    SectorState& st = sectors[ts[i].sectorIdx];
    st.terms.push_back(i);
    st.count++;
    if (ts[i].cand->achievableDownMbps > 0) {
      if (t.committedDownMbps > 0) {
        st.reservedDown += t.committedDownMbps / ts[i].cand->achievableDownMbps;
      }
      st.pressureDown +=
          effectiveWant(t.demandDownMbps, t.ceilingDownMbps) / ts[i].cand->achievableDownMbps;
    }
  }

  // ---- allocation, deepest sites first ----
  //
  // Solving the far end of a relay chain first means a relay's real load on its
  // parent is known before the parent's airtime is divided up. The relay then
  // competes for that airtime as a synthetic terminal, which is exactly what
  // the radios do.
  int maxDepth = 0;
  for (const SiteState& s : sites) maxDepth = std::max(maxDepth, s.depth);

  auto collectSubtree = [&](std::size_t siteI) {
    std::vector<std::size_t> out;
    std::vector<std::size_t> stack{siteI};
    while (!stack.empty()) {
      const std::size_t cur = stack.back();
      stack.pop_back();
      for (std::size_t sIdx : sites[cur].sectors) {
        // Relay terminals are included deliberately. A relay stands in for
        // everything behind it, carrying that subtree's summed weight, so it
        // takes a proportional share of any cut here and the reconciliation
        // pass below pushes the reduction down to the real subscribers.
        for (std::size_t tIdx : sectors[sIdx].terms) out.push_back(tIdx);
      }
      for (std::size_t child : sites[cur].wiredChildren) stack.push_back(child);
    }
    std::sort(out.begin(), out.end());
    return out;
  };

  // capSubtree reduces a group of terminals' downlink grants to fit a capacity,
  // cutting surplus above committed rates first and only breaking commitments
  // when the capacity cannot cover them. Airtime is recomputed from the new
  // grant so the plan stays self-consistent.
  auto capSubtree = [&](const std::vector<std::size_t>& group, double capacity,
                        LimitedBy reason) {
    double totalCir = 0.0, totalSurplus = 0.0;
    for (std::size_t i : group) {
      totalCir += ts[i].cirGrantDown;
      totalSurplus += std::max(0.0, ts[i].grantDown - ts[i].cirGrantDown);
    }
    const double total = totalCir + totalSurplus;
    if (total <= capacity + kRateEpsilon) return;

    const bool breakingCommitments = totalCir > capacity;
    const double surplusFactor =
        totalSurplus > 0 ? std::max(0.0, (capacity - totalCir) / totalSurplus) : 0.0;
    const double cirFactor = breakingCommitments && totalCir > 0 ? capacity / totalCir : 1.0;

    for (std::size_t i : group) {
      const double cir = ts[i].cirGrantDown * cirFactor;
      const double surplus =
          breakingCommitments ? 0.0 : std::max(0.0, ts[i].grantDown - ts[i].cirGrantDown) * surplusFactor;
      const double grant = cir + surplus;

      if (grant < ts[i].grantDown - kRateEpsilon) {
        ts[i].grantDown = grant;
        ts[i].limitedBy = reason;
        const double a = ts[i].cand ? ts[i].cand->achievableDownMbps : 0.0;
        ts[i].airtimeDown = a > 0 ? grant / a : 0.0;
        const double committed = terms[i].committedDownMbps;
        if (committed > 0 && grant < committed - kRateEpsilon) ts[i].committedMet = false;
      }
    }
  };

  for (int depth = maxDepth; depth >= 0; --depth) {
    for (std::size_t si = 0; si < sites.size(); ++si) {
      SiteState& site = sites[si];
      if (site.depth != depth) continue;

      for (std::size_t secI : site.sectors) {
        SectorState& st = sectors[secI];
        allocateDirection(st, terms, ts, problem.policy, /*down=*/true, plan.warnings);
        allocateDirection(st, terms, ts, problem.policy, /*down=*/false, plan.warnings);
      }

      // Own traffic, then anything arriving over a wire from below.
      site.ownGranted = 0.0;
      site.subtreeCommitted = 0.0;
      for (std::size_t secI : site.sectors) {
        site.ownGranted += sectors[secI].grantedDown;
      }
      site.subtreeGranted = site.ownGranted;
      for (std::size_t child : site.wiredChildren) {
        site.subtreeGranted += sites[child].subtreeGranted;
        site.subtreeCommitted += sites[child].subtreeCommitted;
      }
      for (std::size_t secI : site.sectors) {
        for (std::size_t tIdx : sectors[secI].terms) {
          if (!terms[tIdx].isRelay) site.subtreeCommitted += terms[tIdx].committedDownMbps;
        }
      }

      // This site's own circuit to its parent or to the PoP. A relay site's real
      // ceiling is its trunk radio; any wired backhaul_mbps on such a site is
      // advisory, so take whichever binds. The airtime-limited trunk value is
      // not known until the parent sector is solved, so pre-filter at the
      // radio's maximum here and let the reconciliation pass tighten it.
      double capacity = site.site->backhaulMbps;
      if (!site.site->backhaulSectorId.empty() && site.site->relayAchievableMbps > 0) {
        capacity = capacity > 0 ? std::min(capacity, site.site->relayAchievableMbps)
                                : site.site->relayAchievableMbps;
      }
      site.effectiveCapacity = capacity;
      if (capacity > 0 && site.subtreeGranted > capacity + kRateEpsilon) {
        site.congested = true;
        std::ostringstream msg;
        msg << "site " << site.site->id << " backhaul is congested: " << site.subtreeGranted
            << " Mbps offered into " << capacity << " Mbps of capacity";
        plan.warnings.push_back(msg.str());
        capSubtree(collectSubtree(si), capacity, LimitedBy::Backhaul);

        site.ownGranted = 0.0;
        for (std::size_t secI : site.sectors) {
          sectors[secI].grantedDown = 0.0;
          sectors[secI].usedDown = 0.0;
          for (std::size_t tIdx : sectors[secI].terms) {
            sectors[secI].grantedDown += ts[tIdx].grantDown;
            sectors[secI].usedDown += ts[tIdx].airtimeDown;
          }
          site.ownGranted += sectors[secI].grantedDown;
        }
        site.subtreeGranted = std::min(site.subtreeGranted, capacity);
      }

      // Inject this site's trunk into the parent's sector as a synthetic
      // terminal, so it competes for the parent's airtime like anything else.
      if (!site.site->backhaulSectorId.empty()) {
        auto pi = sectorIdx.find(site.site->backhaulSectorId);
        if (pi == sectorIdx.end()) {
          plan.warnings.push_back("site " + site.site->id + " relays over unknown sector " +
                                  site.site->backhaulSectorId + "; its traffic is unaccounted");
          continue;
        }
        if (site.site->relayAchievableMbps <= 0) {
          plan.warnings.push_back("site " + site.site->id +
                                  " relays over sector " + site.site->backhaulSectorId +
                                  " but relay_achievable_mbps is unset; assuming the relay is "
                                  "not a constraint, which is optimistic");
          continue;
        }

        Terminal relay;
        relay.id = "relay:" + site.site->id;
        relay.isRelay = true;
        relay.relaySiteId = site.site->id;
        relay.demandDownMbps = site.subtreeGranted;
        relay.ceilingDownMbps = site.subtreeGranted;
        relay.committedDownMbps = site.subtreeCommitted;
        relay.demandUpMbps = site.subtreeGranted * 0.15;  // typical asymmetry
        relay.ceilingUpMbps = site.subtreeGranted * 0.15;
        relay.pinnedSectorId = site.site->backhaulSectorId;
        relay.currentSectorId = site.site->backhaulSectorId;
        relay.dwellSeconds = std::numeric_limits<double>::max();

        // Weight is the sum of everything behind the relay, so a trunk carrying
        // forty subscribers is not outvoted by one subscriber on the parent.
        double w = 0.0;
        for (std::size_t tIdx : collectSubtree(si)) w += weightOf(terms[tIdx]);
        relay.weightOverride = std::max(1.0, w);

        Candidate c;
        c.sectorId = site.site->backhaulSectorId;
        c.achievableDownMbps = site.site->relayAchievableMbps;
        c.achievableUpMbps = site.site->relayAchievableMbps;
        c.mcs = "relay-trunk";
        relay.candidates.push_back(c);

        terms.push_back(relay);
        ts.emplace_back();
        const std::size_t ri = terms.size() - 1;
        ts[ri].cand = &terms[ri].candidates.front();
        ts[ri].sectorId = c.sectorId;
        ts[ri].sectorIdx = pi->second;
        ts[ri].assigned = true;

        SectorState& parent = sectors[pi->second];
        parent.terms.push_back(ri);
        parent.count++;
        parent.reservedDown += relay.committedDownMbps / c.achievableDownMbps;
        parent.pressureDown += relay.demandDownMbps / c.achievableDownMbps;
        site.relayTermIdx = ri;
      }
    }
  }

  // ---- reconcile relay grants with the subtrees behind them ----
  //
  // A relay may have received less airtime on its parent than the traffic its
  // subtree was granted. Walk shallow sites first and cut each subtree to what
  // its trunk actually got. This is a single corrective pass, not a fixed
  // point: a long relay chain under heavy contention can still need another
  // tick to settle, which is why the residual is reported rather than hidden.
  for (int depth = 0; depth <= maxDepth; ++depth) {
    for (std::size_t si = 0; si < sites.size(); ++si) {
      SiteState& site = sites[si];
      if (site.depth != depth) continue;
      if (site.relayTermIdx == SiteState::kNoTerm) continue;

      const double trunk = ts[site.relayTermIdx].grantDown;
      if (trunk >= site.subtreeGranted - kRateEpsilon) continue;

      std::ostringstream msg;
      msg << "relay trunk for site " << site.site->id << " carries " << trunk
          << " Mbps but its subtree was granted " << site.subtreeGranted
          << " Mbps; subtree reduced to match";
      plan.warnings.push_back(msg.str());

      capSubtree(collectSubtree(si), trunk, LimitedBy::Backhaul);
      site.subtreeGranted = trunk;
      site.effectiveCapacity = trunk;
      site.congested = true;

      for (std::size_t secI : site.sectors) {
        sectors[secI].grantedDown = 0.0;
        sectors[secI].usedDown = 0.0;
        for (std::size_t tIdx : sectors[secI].terms) {
          sectors[secI].grantedDown += ts[tIdx].grantDown;
          sectors[secI].usedDown += ts[tIdx].airtimeDown;
        }
      }
    }
  }

  // ---- build the plan ----
  for (std::size_t i = 0; i < terms.size(); ++i) {
    if (!ts[i].assigned) continue;
    Assignment a;
    a.terminalId = terms[i].id;
    a.sectorId = ts[i].sectorId;
    a.airtimeDown = ts[i].airtimeDown;
    a.airtimeUp = ts[i].airtimeUp;
    a.grantDownMbps = ts[i].grantDown;
    a.grantUpMbps = ts[i].grantUp;
    a.sinrDb = ts[i].cand ? ts[i].cand->sinrDb : 0.0;
    a.mcs = ts[i].cand ? ts[i].cand->mcs : std::string();
    a.committedMet = ts[i].committedMet;
    a.limitedBy = ts[i].limitedBy;
    a.isRelay = terms[i].isRelay;
    plan.assignments.push_back(std::move(a));

    if (terms[i].isRelay) continue;  // relays are plumbing, not customers
    plan.objective.offeredDownMbps += effectiveWant(terms[i].demandDownMbps, terms[i].ceilingDownMbps);
    plan.objective.grantedDownMbps += ts[i].grantDown;
    plan.objective.offeredUpMbps += effectiveWant(terms[i].demandUpMbps, terms[i].ceilingUpMbps);
    plan.objective.grantedUpMbps += ts[i].grantUp;
    // Measured the same way as committedMet above, so the per-terminal flag and
    // the network-wide figure can never disagree.
    const double owed = std::min(terms[i].committedDownMbps,
                                 effectiveWant(terms[i].demandDownMbps, terms[i].ceilingDownMbps));
    if (owed > ts[i].grantDown + kRateEpsilon) {
      plan.objective.committedShortfallMbps += owed - ts[i].grantDown;
    }
  }
  std::sort(plan.assignments.begin(), plan.assignments.end(),
            [](const Assignment& a, const Assignment& b) { return a.terminalId < b.terminalId; });
  std::sort(plan.handoffs.begin(), plan.handoffs.end(),
            [](const Handoff& a, const Handoff& b) { return a.terminalId < b.terminalId; });

  for (const SectorState& st : sectors) {
    SectorReport r;
    r.sectorId = st.sector->id;
    r.airtimeUsedDown = st.usedDown;
    r.airtimeUsedUp = st.usedUp;
    r.airtimeBudget = st.sector->airtimeBudget;
    r.terminals = st.count;
    r.offeredDownMbps = st.offeredDown;
    r.grantedDownMbps = st.grantedDown;
    r.oversubscribed = st.oversubscribed;
    plan.sectors.push_back(std::move(r));
  }
  std::sort(plan.sectors.begin(), plan.sectors.end(),
            [](const SectorReport& a, const SectorReport& b) { return a.sectorId < b.sectorId; });

  for (const SiteState& s : sites) {
    BackhaulReport r;
    r.siteId = s.site->id;
    r.offeredMbps = s.subtreeGranted;
    r.capacityMbps = s.effectiveCapacity > 0 ? s.effectiveCapacity : s.site->backhaulMbps;
    r.utilization = r.capacityMbps > 0 ? s.subtreeGranted / r.capacityMbps : 0.0;
    r.congested = s.congested;
    r.relayDepth = s.depth;
    plan.backhaul.push_back(std::move(r));
  }
  std::sort(plan.backhaul.begin(), plan.backhaul.end(),
            [](const BackhaulReport& a, const BackhaulReport& b) { return a.siteId < b.siteId; });

  plan.solveMicros = std::chrono::duration_cast<std::chrono::microseconds>(
                         std::chrono::steady_clock::now() - started)
                         .count();
  return plan;
}

}  // namespace bswisp
