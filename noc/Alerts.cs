namespace Bswisp.Noc;

/// <summary>How urgently an operator should look.</summary>
public enum Severity
{
    /// <summary>Worth knowing. Nothing is broken.</summary>
    Info = 0,

    /// <summary>Heading somewhere bad. Act this week.</summary>
    Warning = 1,

    /// <summary>Subscribers are affected now, or a contract is being breached.</summary>
    Critical = 2,
}

/// <summary>
/// One thing an operator should know about, and what to do about it.
/// </summary>
/// <remarks>
/// The <see cref="Action"/> field is the point of this whole class. A dashboard
/// that reports "sector ridge-n oversubscribed" tells an operator something is
/// wrong; one that adds "add a channel or move subscribers, this cannot be fixed
/// in software" tells them what to do at 2am. The second is a tool and the first
/// is a decoration.
/// </remarks>
public sealed record Alert(
    Severity Severity,
    string Subject,
    string Title,
    string Detail,
    string Action);

/// <summary>
/// Turns a scheduler plan into alerts.
/// </summary>
/// <remarks>
/// Thresholds live here as named constants rather than in a config file, because
/// each one encodes a judgement about the physics or the business, and a comment
/// explaining why 0.9 is the airtime threshold is worth more than the ability to
/// change it to 0.91 without a rebuild.
/// </remarks>
public static class AlertEvaluator
{
    /// <summary>
    /// Airtime load at which latency starts to suffer noticeably.
    /// </summary>
    /// <remarks>
    /// A sector at 90% of its airtime budget is not 10% away from trouble. Queueing
    /// delay rises sharply as utilisation approaches one, so by the time a sector
    /// reads 95% the subscribers on it have already noticed.
    /// </remarks>
    private const double AirtimeWarnThreshold = 0.90;

    /// <summary>Backhaul utilisation worth acting on. Lead times for transit are long.</summary>
    private const double BackhaulWarnThreshold = 0.85;

    /// <summary>Address pool utilisation worth acting on.</summary>
    private const double IpamWarnThreshold = 0.85;

    /// <summary>
    /// SINR below which a link is one weather event from failing.
    /// </summary>
    /// <remarks>
    /// The lowest modulation needs about 2 dB. Five dB of headroom above that is
    /// roughly one rain event, so a terminal sitting here is a truck roll waiting
    /// to be scheduled rather than an outage waiting to be reported.
    /// </remarks>
    private const double SinrWarnThresholdDb = 7.0;

    /// <summary>
    /// Sector changes in one session before a terminal counts as flapping.
    /// </summary>
    /// <remarks>
    /// Hysteresis should hold a terminal still. A high count means two sectors
    /// score nearly equally for it, which is a tuning problem, not a radio fault.
    /// </remarks>
    private const int FlapThreshold = 8;

    /// <summary>
    /// How many tick intervals may pass before the control plane counts as stalled.
    /// </summary>
    /// <remarks>
    /// Three, not one: a single late tick is a slow solve or a busy box, while
    /// three in a row means grants are frozen and the network is running on a
    /// stale allocation.
    /// </remarks>
    private const int StaleTickMultiplier = 3;

    public static List<Alert> Evaluate(
        ControlPlaneStatus? status,
        Plan? plan,
        IReadOnlyList<Session>? sessions,
        DateTimeOffset now)
    {
        var alerts = new List<Alert>();

        // Reachability first. Every other check is meaningless if the numbers are
        // not arriving, and a dashboard full of stale green is the worst outcome
        // of all.
        if (status is null)
        {
            alerts.Add(new Alert(
                Severity.Critical,
                "control-plane",
                "Control plane unreachable",
                "The NOC cannot read status from basestationd, so nothing below is current.",
                "Check that basestationd is running and that the admin token is correct. "
                + "Subscribers keep their last allocation while it is down, so service "
                + "continues but nothing adapts."));
            return alerts;
        }

        EvaluateScheduler(alerts, status, now);
        EvaluateIpam(alerts, status);

        if (plan is not null)
        {
            EvaluateContracts(alerts, plan);
            EvaluateSectors(alerts, plan);
            EvaluateBackhaul(alerts, plan);
            EvaluateTerminals(alerts, plan);
            EvaluatePlanWarnings(alerts, plan);
        }

        if (sessions is not null)
        {
            EvaluateFlapping(alerts, sessions);
        }

        // Most urgent first, then alphabetically so the order is stable between
        // refreshes and the page does not reshuffle under the operator's cursor.
        return alerts
            .OrderByDescending(a => a.Severity)
            .ThenBy(a => a.Subject, StringComparer.Ordinal)
            .ThenBy(a => a.Title, StringComparer.Ordinal)
            .ToList();
    }

    private static void EvaluateScheduler(List<Alert> alerts, ControlPlaneStatus status,
        DateTimeOffset now)
    {
        var scheduler = status.Scheduler;

        if (!string.IsNullOrWhiteSpace(scheduler.LastError))
        {
            alerts.Add(new Alert(
                Severity.Critical,
                "scheduler",
                "The last allocation tick failed",
                scheduler.LastError!,
                "Subscribers are running on the previous allocation, which is stale but "
                + "safe. Check the solver binary and the daemon log."));
        }

        if (scheduler.TickSeconds > 0 && scheduler.LastTickAt > DateTimeOffset.MinValue)
        {
            var age = now - scheduler.LastTickAt;
            var limit = TimeSpan.FromSeconds(scheduler.TickSeconds * StaleTickMultiplier);
            if (age > limit)
            {
                alerts.Add(new Alert(
                    Severity.Critical,
                    "scheduler",
                    "Allocation has stalled",
                    $"The last tick was {age.TotalSeconds:F0}s ago, over "
                    + $"{StaleTickMultiplier} intervals of {scheduler.TickSeconds:F0}s.",
                    "Grants are frozen. Subscribers keep working at their last rate, but "
                    + "nothing adapts to demand or to a failing link."));
            }
        }

        if (!scheduler.Healthy && string.IsNullOrWhiteSpace(scheduler.LastError))
        {
            alerts.Add(new Alert(
                Severity.Warning,
                "scheduler",
                "Scheduler has not completed a tick yet",
                "The control plane is up but has produced no allocation.",
                "Normal for the first few seconds after a restart. Investigate if it persists."));
        }

        foreach (var note in scheduler.Notes ?? new List<string>())
        {
            alerts.Add(new Alert(
                Severity.Warning,
                "scheduler",
                "A terminal was left out of the allocation",
                note,
                "A terminal in this state authenticates and then receives nothing, which "
                + "the subscriber experiences as a total outage."));
        }
    }

    private static void EvaluateIpam(List<Alert> alerts, ControlPlaneStatus status)
    {
        var used = status.Ipam.V4Utilisation;
        if (used >= IpamWarnThreshold)
        {
            alerts.Add(new Alert(
                used >= 0.95 ? Severity.Critical : Severity.Warning,
                "ipam",
                "CGNAT address pool is filling up",
                $"{status.Ipam.V4Used} of {status.Ipam.V4Capacity} addresses allocated "
                + $"({used:P0}).",
                "A new subscriber cannot be given an address once this is full, and "
                + "authentication starts failing. Widen the pool before that."));
        }
    }

    private static void EvaluateContracts(List<Alert> alerts, Plan plan)
    {
        // The single most important number on the dashboard. Anything above zero
        // means the network is failing a promise that was sold.
        if (plan.Objective.CommittedShortfallMbps > 0.01)
        {
            var affected = plan.Subscribers.Count(a => !a.CommittedMet);
            alerts.Add(new Alert(
                Severity.Critical,
                "contracts",
                "Committed rates are not being delivered",
                $"{plan.Objective.CommittedShortfallMbps:F1} Mbps of committed capacity "
                + $"short across {affected} subscriber(s) who are asking for it.",
                "This is a breach of what was sold, not a performance issue. Either add "
                + "capacity where the shortfall is, or stop selling committed rates there."));
        }

        if (plan.Objective.UnservedTerminals > 0)
        {
            alerts.Add(new Alert(
                Severity.Critical,
                "contracts",
                "Terminals could not be placed on any sector",
                $"{plan.Objective.UnservedTerminals} terminal(s) have no viable sector.",
                "These subscribers have no service at all. Check alignment, obstruction, "
                + "and whether their sector is enabled."));
        }
    }

    private static void EvaluateSectors(List<Alert> alerts, Plan plan)
    {
        foreach (var sector in plan.Sectors)
        {
            if (sector.Oversubscribed)
            {
                alerts.Add(new Alert(
                    Severity.Critical,
                    $"sector/{sector.SectorId}",
                    "Sector is oversubscribed on committed rates",
                    $"Committed rates alone exceed the airtime budget, with "
                    + $"{sector.Terminals} terminal(s) assigned.",
                    "No scheduler change fixes this. Add a channel, narrow the beam, "
                    + "split the sector, or move subscribers off it."));
                continue;
            }

            var load = sector.DownlinkUtilisation;
            if (load >= AirtimeWarnThreshold)
            {
                alerts.Add(new Alert(
                    Severity.Warning,
                    $"sector/{sector.SectorId}",
                    "Sector airtime is nearly exhausted",
                    $"{load:P0} of the downlink airtime budget is in use across "
                    + $"{sector.Terminals} terminal(s), granting "
                    + $"{sector.GrantedDownMbps:F0} of {sector.OfferedDownMbps:F0} Mbps offered.",
                    "Latency rises sharply near full airtime. Plan more capacity here "
                    + "before selling into this sector again."));
            }
        }
    }

    private static void EvaluateBackhaul(List<Alert> alerts, Plan plan)
    {
        foreach (var site in plan.Backhaul)
        {
            if (site.Congested)
            {
                var relayNote = site.RelayDepth > 0
                    ? $" It is {site.RelayDepth} relay hop(s) from the PoP, so every "
                      + "subscriber behind it is affected."
                    : string.Empty;

                alerts.Add(new Alert(
                    Severity.Critical,
                    $"site/{site.SiteId}",
                    "Site trunk is saturated",
                    $"{site.OfferedMbps:F0} Mbps offered into {site.CapacityMbps:F0} Mbps "
                    + $"of capacity." + relayNote,
                    site.RelayDepth > 0
                        ? "The relay trunk is the limit, not the sectors behind it. Give "
                          + "this site its own circuit, or a faster relay radio."
                        : "Buy more transit for this site, or move subscribers to another."));
                continue;
            }

            if (site.CapacityMbps > 0 && site.Utilization >= BackhaulWarnThreshold)
            {
                alerts.Add(new Alert(
                    Severity.Warning,
                    $"site/{site.SiteId}",
                    "Site trunk is filling up",
                    $"{site.Utilization:P0} of {site.CapacityMbps:F0} Mbps in use.",
                    "Transit and microwave links have long lead times. Order capacity now "
                    + "rather than when it saturates."));
            }
        }
    }

    private static void EvaluateTerminals(List<Alert> alerts, Plan plan)
    {
        var marginal = plan.Subscribers
            .Where(a => a.SinrDb > 0 && a.SinrDb < SinrWarnThresholdDb)
            .OrderBy(a => a.SinrDb)
            .ToList();

        foreach (var terminal in marginal.Take(10))
        {
            alerts.Add(new Alert(
                Severity.Warning,
                $"terminal/{terminal.TerminalId}",
                "Link has almost no margin left",
                $"{terminal.SinrDb:F1} dB on {terminal.SectorId} at {terminal.Mcs}, "
                + $"granted {terminal.GrantDownMbps:F1} Mbps.",
                "About one rain event from dropping out. Check alignment and look for "
                + "foliage that has grown into the path since install."));
        }

        if (marginal.Count > 10)
        {
            alerts.Add(new Alert(
                Severity.Warning,
                "terminals",
                "Many links have no margin",
                $"{marginal.Count} terminals are below {SinrWarnThresholdDb:F0} dB; "
                + "the ten worst are listed individually.",
                "A cluster like this usually means a new interferer rather than many "
                + "simultaneous install faults. Re-scan the band."));
        }
    }

    private static void EvaluateFlapping(List<Alert> alerts, IReadOnlyList<Session> sessions)
    {
        foreach (var session in sessions.Where(s => s.Handoffs >= FlapThreshold)
                                        .OrderByDescending(s => s.Handoffs)
                                        .Take(10))
        {
            alerts.Add(new Alert(
                Severity.Warning,
                $"terminal/{session.TerminalId}",
                "Terminal is flapping between sectors",
                $"{session.Handoffs} sector changes this session, currently on "
                + $"{session.SectorId}.",
                "Two sectors score nearly equally for this terminal. Raise the handoff "
                + "gain threshold or the minimum dwell, or pin it to one sector."));
        }
    }

    private static void EvaluatePlanWarnings(List<Alert> alerts, Plan plan)
    {
        foreach (var warning in plan.Warnings)
        {
            // The solver's own warnings are already written for an operator, so
            // they are passed through rather than reworded. Severity is inferred
            // from the subject: an oversubscribed sector is a capacity failure,
            // while a trimmed relay subtree is the system working as designed.
            var critical = warning.Contains("oversubscribed", StringComparison.OrdinalIgnoreCase)
                           || warning.Contains("cycle", StringComparison.OrdinalIgnoreCase)
                           || warning.Contains("unknown", StringComparison.OrdinalIgnoreCase);

            alerts.Add(new Alert(
                critical ? Severity.Critical : Severity.Info,
                "solver",
                critical ? "Solver reported a capacity or configuration fault" : "Solver note",
                warning,
                critical
                    ? "Reported by the allocator itself, so it reflects the live network."
                    : "Informational. The allocator handled it."));
        }
    }
}
