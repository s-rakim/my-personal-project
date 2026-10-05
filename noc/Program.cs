using Bswisp.Noc;

// Self-test mode runs the alert rules against synthetic plans and exits. Kept in
// the same assembly so the NOC needs no second project to be testable.
if (args.Length > 0 && args[0] == "--self-test")
{
    return SelfTest.Run();
}

var builder = WebApplication.CreateBuilder(args);

builder.Services.AddHttpClient<ControlPlaneClient>();
builder.Services.AddResponseCompression();

var app = builder.Build();

app.UseResponseCompression();
app.UseDefaultFiles();
app.UseStaticFiles();

// The single endpoint the dashboard polls. One call rather than three so the page
// never renders a half-consistent view assembled from different instants.
app.MapGet("/api/overview", async (ControlPlaneClient client, CancellationToken ct) =>
{
    var statusTask = client.GetStatusAsync(ct);
    var planTask = client.GetPlanAsync(ct);
    var sessionsTask = client.GetSessionsAsync(ct);
    await Task.WhenAll(statusTask, planTask, sessionsTask).ConfigureAwait(false);

    var status = statusTask.Result;
    var plan = planTask.Result;
    var sessions = sessionsTask.Result;

    var now = DateTimeOffset.UtcNow;
    var alerts = AlertEvaluator.Evaluate(status, plan, sessions, now);

    return Results.Ok(new
    {
        generatedAt = now,
        controlPlaneUrl = client.BaseUrl,
        reachable = status is not null,
        status,
        plan,
        sessions = sessions ?? new List<Session>(),
        alerts = alerts.Select(a => new
        {
            severity = a.Severity.ToString().ToLowerInvariant(),
            subject = a.Subject,
            title = a.Title,
            detail = a.Detail,
            action = a.Action,
        }),
        worstSeverity = alerts.Count > 0
            ? alerts.Max(a => a.Severity).ToString().ToLowerInvariant()
            : "good",
    });
});

app.MapGet("/healthz", () => Results.Ok(new { status = "ok" }));

var listen = app.Configuration["Noc:Listen"] ?? "http://127.0.0.1:8070";
app.Logger.LogInformation("bswisp-noc on {Listen}, watching {ControlPlane}",
    listen, app.Services.GetRequiredService<ControlPlaneClient>().BaseUrl);

app.Run(listen);
return 0;

/// <summary>
/// Exercises the alert rules against synthetic plans.
/// </summary>
/// <remarks>
/// Weighted towards the cases where an alert must or must not fire, because a
/// dashboard that stays calm during an outage is worse than no dashboard, and one
/// that cries wolf on a healthy network gets ignored and then is also worse than
/// no dashboard.
/// </remarks>
internal static class SelfTest
{
    private static int _failures;
    private static string _case = "";

    public static int Run()
    {
        UnreachableControlPlaneIsCritical();
        HealthyNetworkIsQuiet();
        CommittedShortfallIsCritical();
        OversubscribedSectorIsCritical();
        SaturatedRelayNamesTheTrunk();
        FullAirtimeWarns();
        FillingBackhaulWarns();
        UnservedTerminalIsCritical();
        StalledSchedulerIsCritical();
        FlappingTerminalWarns();
        MarginalLinkWarns();
        RelaysAreNotCountedAsSubscribers();
        AlertsAreOrderedBySeverity();
        EveryAlertCarriesAnAction();

        if (_failures == 0)
        {
            Console.WriteLine("all NOC tests passed");
            return 0;
        }
        Console.Error.WriteLine($"{_failures} check(s) failed");
        return 1;
    }

    private static readonly DateTimeOffset Now = DateTimeOffset.Parse("2026-10-05T12:00:00Z");

    private static ControlPlaneStatus HealthyStatus() => new()
    {
        NodeId = "pop-ridge",
        Scheduler = new SchedulerStatus
        {
            Epoch = 100,
            LastTickAt = Now.AddSeconds(-3),
            Healthy = true,
            TickSeconds = 15,
        },
        Ipam = new IpamStatus { V4Used = 5, V4Capacity = 4000, V6Used = 5, V6Capacity = 16000 },
        SessionsOnline = 5,
        SessionsTotal = 5,
    };

    private static Plan HealthyPlan() => new()
    {
        Epoch = 100,
        Solver = "bswisp-airtime/1.0 (C++17)",
        Objective = new Objective
        {
            OfferedDownMbps = 150, GrantedDownMbps = 150,
            CommittedShortfallMbps = 0, UnservedTerminals = 0,
        },
        Sectors = new List<SectorReport>
        {
            new()
            {
                SectorId = "ridge-n", AirtimeUsedDown = 0.30, AirtimeUsedUp = 0.10,
                AirtimeBudget = 0.85, Terminals = 3,
                OfferedDownMbps = 100, GrantedDownMbps = 100,
            },
        },
        Backhaul = new List<BackhaulReport>
        {
            new() { SiteId = "pop-ridge", OfferedMbps = 150, CapacityMbps = 1000,
                    Utilization = 0.15, Congested = false, RelayDepth = 0 },
        },
        Assignments = new List<Assignment>
        {
            new() { TerminalId = "cpe-0001", SectorId = "ridge-n", GrantDownMbps = 50,
                    SinrDb = 24, Mcs = "64QAM 5/6", CommittedMet = true, LimitedBy = "demand" },
        },
    };

    private static void UnreachableControlPlaneIsCritical()
    {
        Case("an unreachable control plane is critical and stops further checks");
        var alerts = AlertEvaluator.Evaluate(null, null, null, Now);
        Check(alerts.Count == 1, $"expected exactly one alert, got {alerts.Count}");
        Check(alerts[0].Severity == Severity.Critical, "should be critical");
        Check(alerts[0].Action.Contains("keep their last allocation"),
            "the action should say service continues");
    }

    private static void HealthyNetworkIsQuiet()
    {
        Case("a healthy network produces no alerts at all");
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), HealthyPlan(),
            new List<Session>(), Now);
        Check(alerts.Count == 0,
            "a quiet network must be quiet, or operators learn to ignore the page. Got: "
            + string.Join("; ", alerts.Select(a => a.Title)));
    }

    private static void CommittedShortfallIsCritical()
    {
        Case("a committed rate shortfall is critical");
        var plan = HealthyPlan() with
        {
            Objective = new Objective { CommittedShortfallMbps = 12.5 },
            Assignments = new List<Assignment>
            {
                new() { TerminalId = "cpe-0001", CommittedMet = false },
                new() { TerminalId = "cpe-0002", CommittedMet = true },
            },
        };
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, null, Now);
        var alert = alerts.FirstOrDefault(a => a.Subject == "contracts");
        Check(alert is not null, "no contracts alert");
        Check(alert?.Severity == Severity.Critical, "should be critical");
        Check(alert?.Detail.Contains("1 subscriber") == true,
            $"should count only the affected subscriber: {alert?.Detail}");
    }

    private static void OversubscribedSectorIsCritical()
    {
        Case("an oversubscribed sector is critical and says software cannot fix it");
        var plan = HealthyPlan();
        plan.Sectors[0] = plan.Sectors[0] with { Oversubscribed = true };
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, null, Now);
        var alert = alerts.FirstOrDefault(a => a.Subject == "sector/ridge-n");
        Check(alert?.Severity == Severity.Critical, "should be critical");
        Check(alert?.Action.Contains("No scheduler change") == true,
            $"the action must say this is a capacity problem: {alert?.Action}");
    }

    private static void SaturatedRelayNamesTheTrunk()
    {
        Case("a saturated relay blames the trunk, not the sectors behind it");
        var plan = HealthyPlan();
        plan.Backhaul.Add(new BackhaulReport
        {
            SiteId = "relay-mill", OfferedMbps = 43, CapacityMbps = 43,
            Utilization = 1.0, Congested = true, RelayDepth = 1,
        });
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, null, Now);
        var alert = alerts.FirstOrDefault(a => a.Subject == "site/relay-mill");
        Check(alert?.Severity == Severity.Critical, "should be critical");
        Check(alert?.Detail.Contains("relay hop") == true,
            $"should mention the relay depth: {alert?.Detail}");
        Check(alert?.Action.Contains("relay trunk is the limit") == true,
            $"should name the trunk as the limit: {alert?.Action}");
    }

    private static void FullAirtimeWarns()
    {
        Case("a sector near full airtime warns");
        var plan = HealthyPlan();
        // 0.62 of a 0.6375 downlink budget is about 97%.
        plan.Sectors[0] = plan.Sectors[0] with
        {
            AirtimeUsedDown = 0.62, AirtimeUsedUp = 0.2, AirtimeBudget = 0.85,
        };
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, null, Now);
        Check(alerts.Any(a => a.Subject == "sector/ridge-n" && a.Severity == Severity.Warning),
            "no airtime warning");
    }

    private static void FillingBackhaulWarns()
    {
        Case("a backhaul filling up warns before it saturates");
        var plan = HealthyPlan();
        plan.Backhaul[0] = plan.Backhaul[0] with { Utilization = 0.90 };
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, null, Now);
        var alert = alerts.FirstOrDefault(a => a.Subject == "site/pop-ridge");
        Check(alert?.Severity == Severity.Warning, "should warn, not yet critical");
        Check(alert?.Action.Contains("lead times") == true,
            "the action should mention lead times, which is why this warns early");
    }

    private static void UnservedTerminalIsCritical()
    {
        Case("an unplaceable terminal is critical");
        var plan = HealthyPlan() with
        {
            Objective = new Objective { UnservedTerminals = 2 },
        };
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, null, Now);
        Check(alerts.Any(a => a.Severity == Severity.Critical
                              && a.Title.Contains("could not be placed")),
            "no unserved-terminal alert");
    }

    private static void StalledSchedulerIsCritical()
    {
        Case("a stalled scheduler is critical after three intervals, not one");
        var oneLate = HealthyStatus();
        oneLate = oneLate with
        {
            Scheduler = oneLate.Scheduler with { LastTickAt = Now.AddSeconds(-20) },
        };
        Check(!AlertEvaluator.Evaluate(oneLate, HealthyPlan(), null, Now)
                .Any(a => a.Title.Contains("stalled")),
            "one late tick is a slow solve, not an outage");

        var stalled = HealthyStatus();
        stalled = stalled with
        {
            Scheduler = stalled.Scheduler with { LastTickAt = Now.AddSeconds(-120) },
        };
        var alerts = AlertEvaluator.Evaluate(stalled, HealthyPlan(), null, Now);
        var alert = alerts.FirstOrDefault(a => a.Title.Contains("stalled"));
        Check(alert?.Severity == Severity.Critical, "three intervals late should be critical");
        Check(alert?.Action.Contains("Grants are frozen") == true,
            "the action should say what a stall actually means");
    }

    private static void FlappingTerminalWarns()
    {
        Case("a flapping terminal warns and is framed as a tuning problem");
        var sessions = new List<Session>
        {
            new() { TerminalId = "cpe-0009", SectorId = "ridge-n", Handoffs = 25, Online = true },
            new() { TerminalId = "cpe-0010", SectorId = "ridge-n", Handoffs = 1, Online = true },
        };
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), HealthyPlan(), sessions, Now);
        Check(alerts.Any(a => a.Subject == "terminal/cpe-0009"), "no flap alert for the flapper");
        Check(!alerts.Any(a => a.Subject == "terminal/cpe-0010"), "quiet terminal must not alert");
        var alert = alerts.First(a => a.Subject == "terminal/cpe-0009");
        Check(alert.Action.Contains("dwell") || alert.Action.Contains("gain threshold"),
            $"the action should name the tuning knobs: {alert.Action}");
    }

    private static void MarginalLinkWarns()
    {
        Case("a link with no margin warns before it fails");
        var plan = HealthyPlan();
        plan.Assignments.Add(new Assignment
        {
            TerminalId = "cpe-0007", SectorId = "mill-e", SinrDb = 3.2,
            Mcs = "BPSK 1/2", GrantDownMbps = 4,
        });
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, null, Now);
        var alert = alerts.FirstOrDefault(a => a.Subject == "terminal/cpe-0007");
        Check(alert?.Severity == Severity.Warning, "should warn");
        Check(alert?.Action.Contains("foliage") == true,
            "the action should suggest the usual cause of a link that degraded after install");
    }

    private static void RelaysAreNotCountedAsSubscribers()
    {
        Case("relay plumbing is never counted as a subscriber");
        var plan = HealthyPlan();
        plan.Assignments.Add(new Assignment
        {
            TerminalId = "relay:relay-mill", SectorId = "ridge-n", IsRelay = true,
            SinrDb = 0, CommittedMet = false, GrantDownMbps = 43,
        });
        Check(plan.Subscribers.Count() == 1,
            $"a relay leaked into the subscriber list: {plan.Subscribers.Count()}");

        // A relay reports SINR 0 and CommittedMet false as a matter of course, so
        // including it would produce two phantom alerts on every healthy network.
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, new List<Session>(), Now);
        Check(!alerts.Any(a => a.Subject.Contains("relay:")),
            "relay produced a subscriber alert: "
            + string.Join("; ", alerts.Select(a => a.Subject)));
    }

    private static void AlertsAreOrderedBySeverity()
    {
        Case("alerts come back most urgent first");
        var plan = HealthyPlan() with
        {
            Objective = new Objective { CommittedShortfallMbps = 5 },
        };
        plan.Backhaul[0] = plan.Backhaul[0] with { Utilization = 0.9 };
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, null, Now);
        Check(alerts.Count >= 2, "expected both a critical and a warning");
        for (var i = 1; i < alerts.Count; i++)
        {
            Check(alerts[i - 1].Severity >= alerts[i].Severity,
                $"out of order at index {i}");
        }
    }

    private static void EveryAlertCarriesAnAction()
    {
        Case("every alert says what to do about it");
        var plan = HealthyPlan() with
        {
            Objective = new Objective { CommittedShortfallMbps = 5, UnservedTerminals = 1 },
            Warnings = new List<string> { "sector ridge-n is oversubscribed: ..." },
        };
        plan.Sectors[0] = plan.Sectors[0] with { Oversubscribed = true };
        plan.Backhaul[0] = plan.Backhaul[0] with { Congested = true };

        var sessions = new List<Session>
        {
            new() { TerminalId = "cpe-0009", Handoffs = 20, Online = true },
        };
        var alerts = AlertEvaluator.Evaluate(HealthyStatus(), plan, sessions, Now);
        Check(alerts.Count > 0, "expected alerts");
        foreach (var alert in alerts)
        {
            Check(!string.IsNullOrWhiteSpace(alert.Action),
                $"alert '{alert.Title}' has no action, which makes it a decoration");
            Check(!string.IsNullOrWhiteSpace(alert.Detail),
                $"alert '{alert.Title}' has no detail");
        }
    }

    private static void Case(string name) => _case = name;

    private static void Check(bool condition, string detail)
    {
        if (!condition)
        {
            Console.Error.WriteLine($"FAIL [{_case}]: {detail}");
            _failures++;
        }
    }
}
