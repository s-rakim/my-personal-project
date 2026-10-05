package net.bswisp.oss;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.LinkedHashMap;
import java.util.Map;

import net.bswisp.oss.model.Model.Account;
import net.bswisp.oss.model.Model.ServicePlan;
import net.bswisp.oss.util.Json;

/**
 * Pushes the book of business into the control plane.
 *
 * <p>The division of ownership is the important part. The OSS owns customers:
 * who they are, what they bought, whether they have paid. The network engineer
 * owns towers: sites, sectors, azimuths, channels. Provisioning merges the two by
 * reading the current inventory, replacing only the customer-shaped parts, and
 * writing it back.
 *
 * <p>Replacing the whole document instead would delete every site and sector the
 * moment the OSS was asked to provision a customer, which is a memorable way to
 * take a network down.
 */
public final class Provisioning {

    private final String controlPlaneUrl;
    private final String adminToken;
    private final HttpClient http;

    public Provisioning(String controlPlaneUrl, String adminToken) {
        this.controlPlaneUrl = controlPlaneUrl.replaceAll("/+$", "");
        this.adminToken = adminToken;
        this.http = HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(5))
                .build();
    }

    /** What a provisioning run did. */
    public record Result(int subscribers, int terminals, int skipped, String message) {
        public Map<String, Object> toJson() {
            var m = new LinkedHashMap<String, Object>();
            m.put("subscribers", subscribers);
            m.put("terminals", terminals);
            m.put("skipped", skipped);
            m.put("message", message);
            return m;
        }
    }

    /** Reads inventory, merges in the OSS view of customers, writes it back. */
    public Result push(Repository repo) throws IOException, InterruptedException {
        Map<String, Object> inventory = fetchInventory();

        var subscribers = new LinkedHashMap<String, Object>();
        var terminals = new LinkedHashMap<String, Object>();
        int skipped = 0;

        for (Account account : repo.accounts()) {
            if (account.status() == net.bswisp.oss.model.Model.Status.CANCELLED) {
                // Cancelled accounts are withdrawn from the network but kept in
                // the OSS: their invoices and usage history still matter.
                skipped++;
                continue;
            }
            ServicePlan plan = repo.plan(account.planName()).orElse(null);
            if (plan == null) {
                skipped++;
                continue;
            }

            var sub = new LinkedHashMap<String, Object>();
            sub.put("id", account.id());
            sub.put("name", account.name());
            sub.put("plan", account.planName());
            sub.put("username", account.id() + "@bswisp.net");
            // Credentials are deliberately absent. The control plane carries its
            // existing hash forward for an account it already knows, so the OSS
            // never holds or transmits a subscriber password.
            sub.put("password_hash", "");
            sub.put("suspended", !account.status().carriesTraffic());
            sub.put("created_at", java.time.Instant.now().toString());
            subscribers.put(account.id(), sub);

            if (account.terminalId() != null && !account.terminalId().isBlank()) {
                var term = new LinkedHashMap<String, Object>();
                term.put("id", account.terminalId());
                term.put("subscriber_id", account.id());
                var position = new LinkedHashMap<String, Object>();
                position.put("lat", account.lat());
                position.put("lon", account.lon());
                position.put("height_m", account.antennaHeightM());
                term.put("position", position);
                term.put("antenna_gain_dbi", 19.0);
                term.put("noise_figure_db", 6.0);
                term.put("feeder_loss_db", 0.5);
                term.put("tx_power_dbm", 23.0);
                term.put("token_hash", "");
                term.put("enabled", account.status().carriesTraffic());
                term.put("install_at", java.time.Instant.now().toString());
                terminals.put(account.terminalId(), term);
            }
        }

        // Plans are pushed too, since they define the rate limits the scheduler
        // enforces. Prices stay here; the control plane has no business knowing
        // what anything costs.
        var planJson = new LinkedHashMap<String, Object>();
        for (ServicePlan p : repo.plans()) {
            var pj = new LinkedHashMap<String, Object>();
            pj.put("name", p.name());
            pj.put("down_mbps", p.downMbps());
            pj.put("up_mbps", p.upMbps());
            pj.put("committed_down_mbps", p.committedDownMbps());
            pj.put("committed_up_mbps", p.committedUpMbps());
            pj.put("priority", p.priority());
            pj.put("burst_mbps", p.burstMbps());
            pj.put("burst_seconds", p.burstSeconds());
            planJson.put(p.name(), pj);
        }

        inventory.put("plans", planJson);
        inventory.put("subscribers", subscribers);
        inventory.put("terminals", terminals);

        HttpRequest put = HttpRequest.newBuilder(URI.create(controlPlaneUrl + "/v1/inventory"))
                .header("Content-Type", "application/json")
                .header("Authorization", "Bearer " + adminToken)
                .timeout(Duration.ofSeconds(15))
                .PUT(HttpRequest.BodyPublishers.ofString(Json.write(inventory, 2)))
                .build();

        HttpResponse<String> response = http.send(put, HttpResponse.BodyHandlers.ofString());
        if (response.statusCode() / 100 != 2) {
            throw new IOException("control plane rejected the inventory: HTTP "
                    + response.statusCode() + " " + response.body());
        }

        return new Result(subscribers.size(), terminals.size(), skipped,
                "inventory accepted by " + controlPlaneUrl);
    }

    private Map<String, Object> fetchInventory() throws IOException, InterruptedException {
        HttpRequest get = HttpRequest.newBuilder(URI.create(controlPlaneUrl + "/v1/inventory"))
                .header("Authorization", "Bearer " + adminToken)
                .timeout(Duration.ofSeconds(15))
                .GET()
                .build();

        HttpResponse<String> response = http.send(get, HttpResponse.BodyHandlers.ofString());
        if (response.statusCode() / 100 != 2) {
            throw new IOException("cannot read inventory from the control plane: HTTP "
                    + response.statusCode() + " " + response.body());
        }
        return Json.parseObject(response.body());
    }
}
