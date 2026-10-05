#include "bswisp/model.hpp"

#include <algorithm>

namespace bswisp {

const char* limitedByName(LimitedBy l) {
  switch (l) {
    case LimitedBy::Demand: return "demand";
    case LimitedBy::Ceiling: return "ceiling";
    case LimitedBy::Airtime: return "airtime";
    case LimitedBy::Backhaul: return "backhaul";
    case LimitedBy::None: break;
  }
  return "none";
}

Problem Problem::fromJson(const json::Value& v) {
  Problem p;
  p.epoch = v["epoch"].asInt(0);
  p.generatedAt = v["generated_at"].asString();
  p.tickSeconds = v["tick_seconds"].asNumber(15.0);

  const json::Value& pol = v["policy"];
  p.policy.handoffGainThreshold = pol["handoff_gain_threshold"].asNumber(1.25);
  p.policy.minDwellSeconds = pol["min_dwell_seconds"].asNumber(60.0);
  p.policy.stickiness = pol["stickiness"].asNumber(0.15);
  p.policy.reserveCommittedFirst = pol["reserve_committed_first"].asBool(true);

  for (const json::Value& s : v["sites"].items()) {
    Site site;
    site.id = s["id"].asString();
    site.backhaulMbps = s["backhaul_mbps"].asNumber(0.0);
    site.parentSiteId = s["parent_site_id"].asString();
    site.backhaulSectorId = s["backhaul_sector_id"].asString();
    site.relayAchievableMbps = s["relay_achievable_mbps"].asNumber(0.0);
    p.sites.push_back(std::move(site));
  }

  for (const json::Value& s : v["sectors"].items()) {
    Sector sec;
    sec.id = s["id"].asString();
    sec.siteId = s["site_id"].asString();
    sec.airtimeBudget = s["airtime_budget"].asNumber(0.85);
    sec.dlUlSplit = s["dl_ul_split"].asNumber(0.75);
    sec.maxTerminals = static_cast<int>(s["max_terminals"].asInt(0));
    p.sectors.push_back(std::move(sec));
  }

  for (const json::Value& t : v["terminals"].items()) {
    Terminal term;
    term.id = t["id"].asString();
    term.priority = static_cast<int>(t["priority"].asInt(0));
    term.demandDownMbps = t["demand_down_mbps"].asNumber(0.0);
    term.demandUpMbps = t["demand_up_mbps"].asNumber(0.0);
    term.ceilingDownMbps = t["ceiling_down_mbps"].asNumber(0.0);
    term.ceilingUpMbps = t["ceiling_up_mbps"].asNumber(0.0);
    term.committedDownMbps = t["committed_down_mbps"].asNumber(0.0);
    term.committedUpMbps = t["committed_up_mbps"].asNumber(0.0);
    term.currentSectorId = t["current_sector_id"].asString();
    term.dwellSeconds = t["dwell_seconds"].asNumber(0.0);
    term.pinnedSectorId = t["pinned_sector_id"].asString();

    for (const json::Value& c : t["candidates"].items()) {
      Candidate cand;
      cand.sectorId = c["sector_id"].asString();
      cand.achievableDownMbps = c["achievable_down_mbps"].asNumber(0.0);
      cand.achievableUpMbps = c["achievable_up_mbps"].asNumber(0.0);
      cand.sinrDb = c["sinr_db"].asNumber(0.0);
      cand.mcs = c["mcs"].asString();
      cand.measured = c["measured"].asBool(false);
      term.candidates.push_back(std::move(cand));
    }
    p.terminals.push_back(std::move(term));
  }

  return p;
}

json::Value Plan::toJson() const {
  json::Value out = json::Value::object();
  out.set("epoch", json::Value(epoch));
  out.set("solver", json::Value(solver));
  out.set("solve_micros", json::Value(solveMicros));

  json::Value obj = json::Value::object();
  obj.set("offered_down_mbps", json::Value(objective.offeredDownMbps));
  obj.set("granted_down_mbps", json::Value(objective.grantedDownMbps));
  obj.set("offered_up_mbps", json::Value(objective.offeredUpMbps));
  obj.set("granted_up_mbps", json::Value(objective.grantedUpMbps));
  obj.set("committed_shortfall_mbps", json::Value(objective.committedShortfallMbps));
  obj.set("unserved_terminals", json::Value(objective.unservedTerminals));
  out.set("objective", std::move(obj));

  json::Value as = json::Value::array();
  for (const Assignment& a : assignments) {
    json::Value v = json::Value::object();
    v.set("terminal_id", json::Value(a.terminalId));
    v.set("sector_id", json::Value(a.sectorId));
    v.set("airtime_down", json::Value(a.airtimeDown));
    v.set("airtime_up", json::Value(a.airtimeUp));
    v.set("grant_down_mbps", json::Value(a.grantDownMbps));
    v.set("grant_up_mbps", json::Value(a.grantUpMbps));
    v.set("sinr_db", json::Value(a.sinrDb));
    v.set("mcs", json::Value(a.mcs));
    v.set("committed_met", json::Value(a.committedMet));
    v.set("limited_by", json::Value(std::string(limitedByName(a.limitedBy))));
    as.push(std::move(v));
  }
  out.set("assignments", std::move(as));

  json::Value hs = json::Value::array();
  for (const Handoff& h : handoffs) {
    json::Value v = json::Value::object();
    v.set("terminal_id", json::Value(h.terminalId));
    v.set("from_sector_id", json::Value(h.fromSectorId));
    v.set("to_sector_id", json::Value(h.toSectorId));
    v.set("reason", json::Value(h.reason));
    v.set("gain", json::Value(h.gain));
    hs.push(std::move(v));
  }
  out.set("handoffs", std::move(hs));

  json::Value ss = json::Value::array();
  for (const SectorReport& s : sectors) {
    json::Value v = json::Value::object();
    v.set("sector_id", json::Value(s.sectorId));
    v.set("airtime_used_down", json::Value(s.airtimeUsedDown));
    v.set("airtime_used_up", json::Value(s.airtimeUsedUp));
    v.set("airtime_budget", json::Value(s.airtimeBudget));
    v.set("terminals", json::Value(s.terminals));
    v.set("offered_down_mbps", json::Value(s.offeredDownMbps));
    v.set("granted_down_mbps", json::Value(s.grantedDownMbps));
    v.set("oversubscribed", json::Value(s.oversubscribed));
    ss.push(std::move(v));
  }
  out.set("sectors", std::move(ss));

  json::Value bs = json::Value::array();
  for (const BackhaulReport& b : backhaul) {
    json::Value v = json::Value::object();
    v.set("site_id", json::Value(b.siteId));
    v.set("offered_mbps", json::Value(b.offeredMbps));
    v.set("capacity_mbps", json::Value(b.capacityMbps));
    v.set("utilization", json::Value(b.utilization));
    v.set("congested", json::Value(b.congested));
    v.set("relay_depth", json::Value(b.relayDepth));
    bs.push(std::move(v));
  }
  out.set("backhaul", std::move(bs));

  json::Value ws = json::Value::array();
  for (const std::string& w : warnings) ws.push(json::Value(w));
  out.set("warnings", std::move(ws));

  return out;
}

}  // namespace bswisp
