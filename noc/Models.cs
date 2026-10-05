using System.Text.Json.Serialization;

namespace Bswisp.Noc;

// Mirrors schema/scheduler-plan.schema.json. That file is authoritative; if
// these records disagree with it, these are wrong.
//
// Every field is nullable or defaulted on purpose. The dashboard must keep
// rendering when a newer control plane sends a field this build has never heard
// of, or omits one it used to send, per the additive-change rule in
// schema/README.md.

public sealed record Assignment
{
    [JsonPropertyName("terminal_id")] public string TerminalId { get; init; } = "";
    [JsonPropertyName("sector_id")] public string SectorId { get; init; } = "";
    [JsonPropertyName("airtime_down")] public double AirtimeDown { get; init; }
    [JsonPropertyName("airtime_up")] public double AirtimeUp { get; init; }
    [JsonPropertyName("grant_down_mbps")] public double GrantDownMbps { get; init; }
    [JsonPropertyName("grant_up_mbps")] public double GrantUpMbps { get; init; }
    [JsonPropertyName("sinr_db")] public double SinrDb { get; init; }
    [JsonPropertyName("mcs")] public string Mcs { get; init; } = "";
    [JsonPropertyName("committed_met")] public bool CommittedMet { get; init; } = true;
    [JsonPropertyName("limited_by")] public string LimitedBy { get; init; } = "none";

    /// <summary>
    /// Synthetic terminals standing in for a relay trunk. Infrastructure, not a
    /// customer, and must never appear in a subscriber count.
    /// </summary>
    [JsonPropertyName("is_relay")] public bool IsRelay { get; init; }
}

public sealed record Handoff
{
    [JsonPropertyName("terminal_id")] public string TerminalId { get; init; } = "";
    [JsonPropertyName("from_sector_id")] public string FromSectorId { get; init; } = "";
    [JsonPropertyName("to_sector_id")] public string ToSectorId { get; init; } = "";
    [JsonPropertyName("reason")] public string Reason { get; init; } = "";
    [JsonPropertyName("gain")] public double Gain { get; init; }
}

public sealed record SectorReport
{
    [JsonPropertyName("sector_id")] public string SectorId { get; init; } = "";
    [JsonPropertyName("airtime_used_down")] public double AirtimeUsedDown { get; init; }
    [JsonPropertyName("airtime_used_up")] public double AirtimeUsedUp { get; init; }
    [JsonPropertyName("airtime_budget")] public double AirtimeBudget { get; init; }
    [JsonPropertyName("terminals")] public int Terminals { get; init; }
    [JsonPropertyName("offered_down_mbps")] public double OfferedDownMbps { get; init; }
    [JsonPropertyName("granted_down_mbps")] public double GrantedDownMbps { get; init; }
    [JsonPropertyName("oversubscribed")] public bool Oversubscribed { get; init; }

    /// <summary>
    /// Airtime used as a fraction of the downlink budget. The budget already
    /// excludes the uplink share, so comparing against the raw budget would
    /// understate load by about a quarter.
    /// </summary>
    public double DownlinkUtilisation
    {
        get
        {
            // The report carries the whole-sector budget; the downlink gets the
            // DL/UL split of it. Derive the split from what was actually used
            // rather than assuming 0.75, so a frequency-division sector reads
            // correctly too.
            var downBudget = AirtimeUsedDown + AirtimeUsedUp > 0
                ? AirtimeBudget * (AirtimeUsedDown / Math.Max(1e-9, AirtimeUsedDown + AirtimeUsedUp))
                : AirtimeBudget;
            return downBudget > 0 ? AirtimeUsedDown / downBudget : 0;
        }
    }
}

public sealed record BackhaulReport
{
    [JsonPropertyName("site_id")] public string SiteId { get; init; } = "";
    [JsonPropertyName("offered_mbps")] public double OfferedMbps { get; init; }
    [JsonPropertyName("capacity_mbps")] public double CapacityMbps { get; init; }
    [JsonPropertyName("utilization")] public double Utilization { get; init; }
    [JsonPropertyName("congested")] public bool Congested { get; init; }
    [JsonPropertyName("relay_depth")] public int RelayDepth { get; init; }
}

public sealed record Objective
{
    [JsonPropertyName("offered_down_mbps")] public double OfferedDownMbps { get; init; }
    [JsonPropertyName("granted_down_mbps")] public double GrantedDownMbps { get; init; }
    [JsonPropertyName("offered_up_mbps")] public double OfferedUpMbps { get; init; }
    [JsonPropertyName("granted_up_mbps")] public double GrantedUpMbps { get; init; }
    [JsonPropertyName("committed_shortfall_mbps")] public double CommittedShortfallMbps { get; init; }
    [JsonPropertyName("unserved_terminals")] public int UnservedTerminals { get; init; }
}

public sealed record Plan
{
    [JsonPropertyName("epoch")] public long Epoch { get; init; }
    [JsonPropertyName("solver")] public string Solver { get; init; } = "";
    [JsonPropertyName("solve_micros")] public long SolveMicros { get; init; }
    [JsonPropertyName("objective")] public Objective Objective { get; init; } = new();
    [JsonPropertyName("assignments")] public List<Assignment> Assignments { get; init; } = new();
    [JsonPropertyName("handoffs")] public List<Handoff> Handoffs { get; init; } = new();
    [JsonPropertyName("sectors")] public List<SectorReport> Sectors { get; init; } = new();
    [JsonPropertyName("backhaul")] public List<BackhaulReport> Backhaul { get; init; } = new();
    [JsonPropertyName("warnings")] public List<string> Warnings { get; init; } = new();

    /// <summary>Subscriber assignments only, with relay plumbing excluded.</summary>
    public IEnumerable<Assignment> Subscribers => Assignments.Where(a => !a.IsRelay);
}

public sealed record SchedulerStatus
{
    [JsonPropertyName("epoch")] public long Epoch { get; init; }
    [JsonPropertyName("last_tick_at")] public DateTimeOffset LastTickAt { get; init; }
    [JsonPropertyName("last_error")] public string? LastError { get; init; }
    [JsonPropertyName("notes")] public List<string>? Notes { get; init; }
    [JsonPropertyName("healthy")] public bool Healthy { get; init; }
    [JsonPropertyName("tick_seconds")] public double TickSeconds { get; init; }
}

public sealed record IpamStatus
{
    [JsonPropertyName("v4_used")] public long V4Used { get; init; }
    [JsonPropertyName("v4_capacity")] public long V4Capacity { get; init; }
    [JsonPropertyName("v6_used")] public long V6Used { get; init; }
    [JsonPropertyName("v6_capacity")] public long V6Capacity { get; init; }

    public double V4Utilisation => V4Capacity > 0 ? (double)V4Used / V4Capacity : 0;
}

public sealed record ControlPlaneStatus
{
    [JsonPropertyName("node_id")] public string NodeId { get; init; } = "";
    [JsonPropertyName("scheduler")] public SchedulerStatus Scheduler { get; init; } = new();
    [JsonPropertyName("ipam")] public IpamStatus Ipam { get; init; } = new();
    [JsonPropertyName("sessions_online")] public int SessionsOnline { get; init; }
    [JsonPropertyName("sessions_total")] public int SessionsTotal { get; init; }
}

public sealed record Session
{
    [JsonPropertyName("terminal_id")] public string TerminalId { get; init; } = "";
    [JsonPropertyName("subscriber_id")] public string SubscriberId { get; init; } = "";
    [JsonPropertyName("sector_id")] public string SectorId { get; init; } = "";
    [JsonPropertyName("online")] public bool Online { get; init; }
    [JsonPropertyName("grant_down_mbps")] public double GrantDownMbps { get; init; }
    [JsonPropertyName("demand_down_mbps")] public double DemandDownMbps { get; init; }
    [JsonPropertyName("limited_by")] public string LimitedBy { get; init; } = "";
    [JsonPropertyName("committed_met")] public bool CommittedMet { get; init; } = true;
    [JsonPropertyName("handoffs")] public int Handoffs { get; init; }
    [JsonPropertyName("last_seen")] public DateTimeOffset LastSeen { get; init; }
}
