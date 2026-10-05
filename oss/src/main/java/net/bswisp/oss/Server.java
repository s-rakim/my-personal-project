package net.bswisp.oss;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.LocalDate;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.Executors;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import net.bswisp.oss.model.Model.Account;
import net.bswisp.oss.model.Model.Invoice;
import net.bswisp.oss.model.Model.ServicePlan;
import net.bswisp.oss.model.Model.Status;
import net.bswisp.oss.model.Model.UsageRecord;
import net.bswisp.oss.util.Json;

/**
 * The OSS HTTP interface.
 *
 * <p>Built on the JDK's own HTTP server so the service has no dependencies. A
 * billing system outlives its framework choices, and this one has to keep
 * starting in five years.
 */
public final class Server {

    private static final int MAX_BODY_BYTES = 1 << 20;

    private final Repository repo;
    private final Provisioning provisioning;
    private final String token;
    private final HttpServer http;

    public Server(Repository repo, Provisioning provisioning, String token,
            String host, int port) throws IOException {
        this.repo = repo;
        this.provisioning = provisioning;
        this.token = token;
        this.http = HttpServer.create(new InetSocketAddress(host, port), 0);

        http.createContext("/healthz", this::handleHealth);
        http.createContext("/oss/v1/plans", guard(this::handlePlans));
        http.createContext("/oss/v1/accounts", guard(this::handleAccounts));
        http.createContext("/oss/v1/usage", guard(this::handleUsage));
        http.createContext("/oss/v1/billing/run", guard(this::handleBillingRun));
        http.createContext("/oss/v1/invoices", guard(this::handleInvoices));
        http.createContext("/oss/v1/provision", guard(this::handleProvision));

        // A bounded pool, not a cached one: an unbounded pool turns a traffic
        // spike into thousands of threads all contending on the same file lock.
        http.setExecutor(Executors.newFixedThreadPool(8));
    }

    public void start() {
        http.start();
    }

    public void stop() {
        http.stop(2);
    }

    public int port() {
        return http.getAddress().getPort();
    }

    // ---- routing ---------------------------------------------------------

    private interface Handler {
        void handle(HttpExchange exchange) throws IOException;
    }

    /** Wraps a handler with bearer-token authentication and error handling. */
    private com.sun.net.httpserver.HttpHandler guard(Handler inner) {
        return exchange -> {
            try {
                if (token == null || token.isBlank()) {
                    // Fail closed. These routes create customers and issue
                    // invoices; serving them unauthenticated is never right.
                    send(exchange, 403, error("OSS_TOKEN is unset, so these routes are disabled"));
                    return;
                }
                String auth = exchange.getRequestHeaders().getFirst("Authorization");
                if (auth == null || !auth.startsWith("Bearer ")
                        || !constantTimeEquals(auth.substring(7).trim(), token)) {
                    exchange.getResponseHeaders().set("WWW-Authenticate", "Bearer realm=\"bswisp-oss\"");
                    send(exchange, 401, error("invalid or missing token"));
                    return;
                }
                inner.handle(exchange);
            } catch (Json.ParseException e) {
                send(exchange, 400, error("invalid JSON: " + e.getMessage()));
            } catch (IllegalArgumentException e) {
                send(exchange, 400, error(e.getMessage()));
            } catch (Exception e) {
                // The message goes to the client; the stack trace stays here.
                send(exchange, 500, error(e.getClass().getSimpleName() + ": " + e.getMessage()));
            } finally {
                exchange.close();
            }
        };
    }

    private void handleHealth(HttpExchange exchange) throws IOException {
        send(exchange, 200, Map.of("status", "ok", "accounts", repo.accounts().size()));
        exchange.close();
    }

    private void handlePlans(HttpExchange exchange) throws IOException {
        if (!"GET".equals(exchange.getRequestMethod())) {
            send(exchange, 405, error("use GET"));
            return;
        }
        send(exchange, 200, Map.of("plans",
                repo.plans().stream().map(ServicePlan::toJson).toList()));
    }

    private void handleAccounts(HttpExchange exchange) throws IOException {
        String path = exchange.getRequestURI().getPath();
        String method = exchange.getRequestMethod();

        // /oss/v1/accounts/{id}/status
        if (path.endsWith("/status") && "POST".equals(method)) {
            String id = extractId(path, "/oss/v1/accounts/", "/status");
            Account account = repo.account(id).orElseThrow(
                    () -> new IllegalArgumentException("no account " + id));

            Map<String, Object> body = Json.parseObject(readBody(exchange));
            Status next = Status.of(Json.str(body, "status", ""));
            LocalDate on = LocalDate.parse(Json.str(body, "on", LocalDate.now().toString()));

            Account updated = account.withStatus(next, on);
            repo.putAccount(updated);
            repo.save();
            send(exchange, 200, updated.toJson());
            return;
        }

        if ("GET".equals(method)) {
            send(exchange, 200, Map.of("accounts",
                    repo.accounts().stream().map(Account::toJson).toList()));
            return;
        }

        if ("POST".equals(method)) {
            Map<String, Object> body = Json.parseObject(readBody(exchange));
            String id = Json.str(body, "id", "");
            if (id.isBlank()) {
                throw new IllegalArgumentException("id is required");
            }
            String planName = Json.str(body, "plan", "");
            if (repo.plan(planName).isEmpty()) {
                throw new IllegalArgumentException("unknown plan '" + planName + "'");
            }

            Account account = Account.fromJson(body);
            repo.putAccount(account);
            repo.save();
            send(exchange, 201, account.toJson());
            return;
        }

        send(exchange, 405, error("use GET or POST"));
    }

    private void handleUsage(HttpExchange exchange) throws IOException {
        if ("GET".equals(exchange.getRequestMethod())) {
            send(exchange, 200, Map.of("usage",
                    repo.usage().stream().map(UsageRecord::toJson).toList()));
            return;
        }
        if (!"POST".equals(exchange.getRequestMethod())) {
            send(exchange, 405, error("use GET or POST"));
            return;
        }
        Map<String, Object> body = Json.parseObject(readBody(exchange));
        UsageRecord record = UsageRecord.fromJson(body);
        if (repo.account(record.accountId()).isEmpty()) {
            throw new IllegalArgumentException("no account " + record.accountId());
        }
        repo.addUsage(record);
        repo.save();
        send(exchange, 201, record.toJson());
    }

    private void handleBillingRun(HttpExchange exchange) throws IOException {
        if (!"POST".equals(exchange.getRequestMethod())) {
            send(exchange, 405, error("use POST"));
            return;
        }
        Map<String, Object> body = Json.parseObject(readBody(exchange));
        LocalDate start = LocalDate.parse(Json.str(body, "period_start",
                LocalDate.now().withDayOfMonth(1).toString()));
        LocalDate end = LocalDate.parse(Json.str(body, "period_end",
                start.withDayOfMonth(start.lengthOfMonth()).toString()));
        LocalDate issued = LocalDate.parse(Json.str(body, "issued_on", LocalDate.now().toString()));

        var issuedList = new java.util.ArrayList<Map<String, Object>>();
        var notBillableReasons = new java.util.ArrayList<Map<String, Object>>();
        int duplicates = 0;
        int notBillable = 0;

        for (Account account : repo.accounts()) {
            Optional<ServicePlan> plan = repo.plan(account.planName());
            if (plan.isEmpty()) {
                notBillableReasons.add(Map.of("account_id", account.id(),
                        "reason", "unknown plan '" + account.planName() + "'"));
                notBillable++;
                continue;
            }
            Billing.Outcome outcome = Billing.invoiceFor(
                    account, plan.get(), start, end,
                    repo.usageFor(account.id(), start, end), issued);
            if (!outcome.billed()) {
                notBillableReasons.add(Map.of(
                        "account_id", account.id(), "reason", outcome.reason()));
                notBillable++;
                continue;
            }
            Invoice invoice = outcome.invoice().orElseThrow();
            if (repo.addInvoiceIfAbsent(invoice)) {
                issuedList.add(invoice.toJson());
            } else {
                duplicates++;
            }
        }
        repo.save();

        var out = new LinkedHashMap<String, Object>();
        out.put("period_start", start.toString());
        out.put("period_end", end.toString());
        out.put("issued", issuedList);
        out.put("issued_count", issuedList.size());
        // Reported rather than hidden: a retried run showing a pile of duplicates
        // is the idempotency guard doing its job, and an operator should see it.
        out.put("already_invoiced", duplicates);
        out.put("not_billable", notBillable);
        out.put("not_billable_reasons", notBillableReasons);
        send(exchange, 200, out);
    }

    private void handleInvoices(HttpExchange exchange) throws IOException {
        if (!"GET".equals(exchange.getRequestMethod())) {
            send(exchange, 405, error("use GET"));
            return;
        }
        List<Invoice> all = repo.invoices();
        long totalCents = all.stream().mapToLong(i -> i.total().cents()).sum();
        var out = new LinkedHashMap<String, Object>();
        out.put("invoices", all.stream().map(Invoice::toJson).toList());
        out.put("count", all.size());
        out.put("total", net.bswisp.oss.util.Money.ofCents(totalCents).toString());
        send(exchange, 200, out);
    }

    private void handleProvision(HttpExchange exchange) throws IOException {
        if (!"POST".equals(exchange.getRequestMethod())) {
            send(exchange, 405, error("use POST"));
            return;
        }
        if (provisioning == null) {
            send(exchange, 503, error("no control plane configured; set --control-plane"));
            return;
        }
        try {
            send(exchange, 200, provisioning.push(repo).toJson());
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            send(exchange, 503, error("provisioning interrupted"));
        }
    }

    // ---- helpers ---------------------------------------------------------

    private static String extractId(String path, String prefix, String suffix) {
        if (!path.startsWith(prefix) || !path.endsWith(suffix)) {
            throw new IllegalArgumentException("unrecognised path " + path);
        }
        String id = path.substring(prefix.length(), path.length() - suffix.length());
        if (id.isBlank() || id.contains("/")) {
            throw new IllegalArgumentException("unrecognised path " + path);
        }
        return id;
    }

    private static String readBody(HttpExchange exchange) throws IOException {
        byte[] raw = exchange.getRequestBody().readNBytes(MAX_BODY_BYTES);
        if (raw.length == 0) {
            return "{}";
        }
        return new String(raw, StandardCharsets.UTF_8);
    }

    private static Map<String, Object> error(String message) {
        return Map.of("error", message == null ? "unknown error" : message);
    }

    private static void send(HttpExchange exchange, int status, Object body) throws IOException {
        byte[] raw = (Json.write(body, 2) + "\n").getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(status, raw.length);
        try (OutputStream out = exchange.getResponseBody()) {
            out.write(raw);
        }
    }

    private static boolean constantTimeEquals(String a, String b) {
        byte[] x = a.getBytes(StandardCharsets.UTF_8);
        byte[] y = b.getBytes(StandardCharsets.UTF_8);
        int diff = x.length ^ y.length;
        for (int i = 0; i < x.length && i < y.length; i++) {
            diff |= x[i] ^ y[i];
        }
        return diff == 0;
    }
}
