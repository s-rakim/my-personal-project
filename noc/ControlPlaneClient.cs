using System.Net.Http.Headers;
using System.Text.Json;

namespace Bswisp.Noc;

/// <summary>
/// Reads the control plane's operator API.
/// </summary>
/// <remarks>
/// Every call is allowed to fail and returns null rather than throwing. A NOC
/// console whose own page breaks when the thing it monitors goes down is useless
/// precisely when it is needed, so a failed fetch becomes a Critical alert
/// instead of a stack trace.
/// </remarks>
public sealed class ControlPlaneClient
{
    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        PropertyNameCaseInsensitive = true,
        // Tolerate fields this build has never heard of, per the additive-change
        // rule in schema/README.md.
        ReadCommentHandling = JsonCommentHandling.Skip,
        AllowTrailingCommas = true,
    };

    private readonly HttpClient _http;
    private readonly ILogger<ControlPlaneClient> _log;

    public ControlPlaneClient(HttpClient http, IConfiguration config,
        ILogger<ControlPlaneClient> log)
    {
        _http = http;
        _log = log;

        var baseUrl = config["ControlPlane:Url"] ?? "http://127.0.0.1:8080";
        _http.BaseAddress = new Uri(baseUrl.TrimEnd('/') + "/");
        _http.Timeout = TimeSpan.FromSeconds(5);

        var token = config["ControlPlane:Token"]
                    ?? Environment.GetEnvironmentVariable("CONTROL_PLANE_TOKEN");
        if (!string.IsNullOrWhiteSpace(token))
        {
            _http.DefaultRequestHeaders.Authorization =
                new AuthenticationHeaderValue("Bearer", token);
        }
        else
        {
            _log.LogWarning(
                "CONTROL_PLANE_TOKEN is unset; the control plane will refuse every "
                + "operator route and the dashboard will show it as unreachable");
        }
    }

    public string BaseUrl => _http.BaseAddress?.ToString() ?? "(unset)";

    public Task<ControlPlaneStatus?> GetStatusAsync(CancellationToken ct) =>
        GetAsync<ControlPlaneStatus>("v1/status", ct);

    public Task<Plan?> GetPlanAsync(CancellationToken ct) =>
        GetAsync<Plan>("v1/plan", ct);

    public Task<List<Session>?> GetSessionsAsync(CancellationToken ct) =>
        GetAsync<List<Session>>("v1/sessions", ct);

    private async Task<T?> GetAsync<T>(string path, CancellationToken ct) where T : class
    {
        try
        {
            using var response = await _http.GetAsync(path, ct).ConfigureAwait(false);
            if (!response.IsSuccessStatusCode)
            {
                _log.LogWarning("control plane returned {Status} for {Path}",
                    (int)response.StatusCode, path);
                return null;
            }
            var body = await response.Content.ReadAsStringAsync(ct).ConfigureAwait(false);
            return JsonSerializer.Deserialize<T>(body, JsonOptions);
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            _log.LogWarning("control plane timed out on {Path}", path);
            return null;
        }
        catch (Exception e) when (e is HttpRequestException or JsonException)
        {
            _log.LogWarning(e, "could not read {Path} from the control plane", path);
            return null;
        }
    }
}
