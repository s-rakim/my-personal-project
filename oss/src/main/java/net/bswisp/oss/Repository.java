package net.bswisp.oss;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.time.LocalDate;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;

import net.bswisp.oss.model.Model.Account;
import net.bswisp.oss.model.Model.Invoice;
import net.bswisp.oss.model.Model.ServicePlan;
import net.bswisp.oss.model.Model.UsageRecord;
import net.bswisp.oss.util.Json;

/**
 * Persistence, as one JSON document.
 *
 * <p>A file because a small operator's book of business is a few thousand records
 * that a human edits a few times a day, and a file can be read, diffed, committed
 * and restored without a DBA. Past that, or once more than one process writes,
 * this wants Postgres behind the same methods.
 *
 * <p>Writes go through a temporary file and a rename. A half-written customer
 * ledger is worse than a stale one.
 */
public final class Repository {

    private final Path path;

    private final Map<String, ServicePlan> plans = new LinkedHashMap<>();
    private final Map<String, Account> accounts = new LinkedHashMap<>();
    private final List<UsageRecord> usage = new ArrayList<>();
    private final Map<String, Invoice> invoices = new LinkedHashMap<>();

    public Repository(Path path) {
        this.path = path;
    }

    public synchronized void load() throws IOException {
        if (!Files.exists(path)) {
            return;
        }
        String text = Files.readString(path, StandardCharsets.UTF_8);
        if (text.isBlank()) {
            return;
        }
        Map<String, Object> doc = Json.parseObject(text);

        plans.clear();
        accounts.clear();
        usage.clear();
        invoices.clear();

        for (Object raw : Json.arr(doc, "plans")) {
            ServicePlan p = ServicePlan.fromJson(asMap(raw));
            plans.put(p.name(), p);
        }
        for (Object raw : Json.arr(doc, "accounts")) {
            Account a = Account.fromJson(asMap(raw));
            accounts.put(a.id(), a);
        }
        for (Object raw : Json.arr(doc, "usage")) {
            usage.add(UsageRecord.fromJson(asMap(raw)));
        }
        for (Object raw : Json.arr(doc, "invoices")) {
            Invoice i = Invoice.fromJson(asMap(raw));
            invoices.put(i.id(), i);
        }
    }

    public synchronized void save() throws IOException {
        var doc = new LinkedHashMap<String, Object>();
        doc.put("version", 1);
        doc.put("plans", plans.values().stream().map(ServicePlan::toJson).toList());
        doc.put("accounts", accounts.values().stream().map(Account::toJson).toList());
        doc.put("usage", usage.stream().map(UsageRecord::toJson).toList());
        doc.put("invoices", invoices.values().stream().map(Invoice::toJson).toList());

        Path parent = path.toAbsolutePath().getParent();
        if (parent != null) {
            Files.createDirectories(parent);
        }
        Path tmp = Files.createTempFile(parent, ".oss", ".tmp");
        try {
            Files.writeString(tmp, Json.write(doc, 2) + "\n", StandardCharsets.UTF_8);
            Files.move(tmp, path, StandardCopyOption.REPLACE_EXISTING,
                    StandardCopyOption.ATOMIC_MOVE);
        } finally {
            Files.deleteIfExists(tmp);
        }
    }

    public synchronized List<ServicePlan> plans() {
        return List.copyOf(plans.values());
    }

    public synchronized Optional<ServicePlan> plan(String name) {
        return Optional.ofNullable(plans.get(name));
    }

    public synchronized void putPlan(ServicePlan p) {
        plans.put(p.name(), p);
    }

    public synchronized List<Account> accounts() {
        return List.copyOf(accounts.values());
    }

    public synchronized Optional<Account> account(String id) {
        return Optional.ofNullable(accounts.get(id));
    }

    public synchronized void putAccount(Account a) {
        accounts.put(a.id(), a);
    }

    public synchronized List<UsageRecord> usage() {
        return List.copyOf(usage);
    }

    public synchronized void addUsage(UsageRecord r) {
        usage.add(r);
    }

    /** Usage for one account overlapping a period, summed. */
    public synchronized Optional<UsageRecord> usageFor(String accountId,
            LocalDate start, LocalDate end) {
        long down = 0;
        long up = 0;
        boolean found = false;
        for (UsageRecord r : usage) {
            if (!r.accountId().equals(accountId)) continue;
            if (r.periodEnd().isBefore(start) || r.periodStart().isAfter(end)) continue;
            down += r.bytesDown();
            up += r.bytesUp();
            found = true;
        }
        return found
                ? Optional.of(new UsageRecord(accountId, start, end, down, up))
                : Optional.empty();
    }

    public synchronized List<Invoice> invoices() {
        return List.copyOf(invoices.values());
    }

    /**
     * Stores an invoice unless one already exists for the same account and
     * period. Returns true when it was new.
     *
     * <p>This is where billing idempotency lives. Invoice ids are derived from the
     * account and period, so a retried billing run collides here instead of
     * charging twice.
     */
    public synchronized boolean addInvoiceIfAbsent(Invoice i) {
        if (invoices.containsKey(i.id())) {
            return false;
        }
        invoices.put(i.id(), i);
        return true;
    }

    @SuppressWarnings("unchecked")
    private static Map<String, Object> asMap(Object raw) {
        return raw instanceof Map ? (Map<String, Object>) raw : new LinkedHashMap<>();
    }

    /** A small starter book of business, for a first run. */
    public synchronized void seed() {
        putPlan(new ServicePlan("residential-50", net.bswisp.oss.util.Money.parse("49.99"),
                50, 10, 10, 2, 1, 75, 20, 0, net.bswisp.oss.util.Money.ZERO));
        putPlan(new ServicePlan("residential-150", net.bswisp.oss.util.Money.parse("79.99"),
                150, 25, 25, 5, 2, 200, 20, 1500,
                net.bswisp.oss.util.Money.parse("2.00")));
        putPlan(new ServicePlan("business-200", net.bswisp.oss.util.Money.parse("249.99"),
                200, 50, 50, 15, 5, 0, 0, 0, net.bswisp.oss.util.Money.ZERO));
    }
}
