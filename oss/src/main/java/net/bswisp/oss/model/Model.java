package net.bswisp.oss.model;

import java.time.LocalDate;
import java.util.List;
import java.util.Map;

import net.bswisp.oss.util.Json;
import net.bswisp.oss.util.Money;

/** Domain types for the operations and billing system. */
public final class Model {

    private Model() {}

    /**
     * Where an account is in its life.
     *
     * <p>PENDING matters: an account is sold, surveyed and provisioned before it
     * passes traffic, and it must not be billed during that window. Collapsing
     * PENDING into ACTIVE is how customers get charged for service that was not
     * installed yet.
     */
    public enum Status {
        PENDING, ACTIVE, SUSPENDED, CANCELLED;

        public static Status of(String s) {
            try {
                return valueOf(s.toUpperCase());
            } catch (IllegalArgumentException | NullPointerException e) {
                return PENDING;
            }
        }

        /** Whether the control plane should let this account pass traffic. */
        public boolean carriesTraffic() {
            return this == ACTIVE;
        }

        /** Whether this account is billable for a period it overlaps. */
        public boolean billable() {
            // A suspended account still occupies a sector's inventory and keeps
            // its address, so it is still billable unless the operator credits
            // it. Cancelled and pending are not.
            return this == ACTIVE || this == SUSPENDED;
        }
    }

    /** A service plan, as sold. */
    public record ServicePlan(
            String name,
            Money monthlyPrice,
            double downMbps,
            double upMbps,
            double committedDownMbps,
            double committedUpMbps,
            int priority,
            double burstMbps,
            double burstSeconds,
            /** Monthly allowance in gigabytes. Zero means unmetered. */
            double dataCapGb,
            /** Charged per gigabyte beyond the cap. */
            Money overagePerGb) {

        public Map<String, Object> toJson() {
            return Map.of(
                    "name", name,
                    "monthly_price", monthlyPrice.toString(),
                    "down_mbps", downMbps,
                    "up_mbps", upMbps,
                    "committed_down_mbps", committedDownMbps,
                    "committed_up_mbps", committedUpMbps,
                    "priority", priority,
                    "data_cap_gb", dataCapGb,
                    "overage_per_gb", overagePerGb.toString());
        }

        public static ServicePlan fromJson(Map<String, Object> o) {
            return new ServicePlan(
                    Json.str(o, "name", ""),
                    Money.parse(Json.str(o, "monthly_price", "0.00")),
                    Json.num(o, "down_mbps", 0),
                    Json.num(o, "up_mbps", 0),
                    Json.num(o, "committed_down_mbps", 0),
                    Json.num(o, "committed_up_mbps", 0),
                    (int) Json.integer(o, "priority", 1),
                    Json.num(o, "burst_mbps", 0),
                    Json.num(o, "burst_seconds", 0),
                    Json.num(o, "data_cap_gb", 0),
                    Money.parse(Json.str(o, "overage_per_gb", "0.00")));
        }
    }

    /** A customer account. */
    public record Account(
            String id,
            String name,
            String email,
            String planName,
            Status status,
            String serviceAddress,
            double lat,
            double lon,
            double antennaHeightM,
            /** Null until the install is signed off. */
            LocalDate activatedOn,
            LocalDate cancelledOn,
            /** The CPE serial, which is the control plane's terminal id. */
            String terminalId,
            String notes) {

        public Account withStatus(Status next, LocalDate on) {
            LocalDate activated = activatedOn;
            LocalDate cancelled = cancelledOn;
            if (next == Status.ACTIVE && activated == null) activated = on;
            if (next == Status.CANCELLED) cancelled = on;
            return new Account(id, name, email, planName, next, serviceAddress,
                    lat, lon, antennaHeightM, activated, cancelled, terminalId, notes);
        }

        public Map<String, Object> toJson() {
            var m = new java.util.LinkedHashMap<String, Object>();
            m.put("id", id);
            m.put("name", name);
            m.put("email", email);
            m.put("plan", planName);
            m.put("status", status.name());
            m.put("service_address", serviceAddress);
            m.put("lat", lat);
            m.put("lon", lon);
            m.put("antenna_height_m", antennaHeightM);
            m.put("activated_on", activatedOn == null ? null : activatedOn.toString());
            m.put("cancelled_on", cancelledOn == null ? null : cancelledOn.toString());
            m.put("terminal_id", terminalId);
            m.put("notes", notes);
            return m;
        }

        public static Account fromJson(Map<String, Object> o) {
            return new Account(
                    Json.str(o, "id", ""),
                    Json.str(o, "name", ""),
                    Json.str(o, "email", ""),
                    Json.str(o, "plan", ""),
                    Status.of(Json.str(o, "status", "PENDING")),
                    Json.str(o, "service_address", ""),
                    Json.num(o, "lat", 0),
                    Json.num(o, "lon", 0),
                    Json.num(o, "antenna_height_m", 6),
                    parseDate(Json.str(o, "activated_on", null)),
                    parseDate(Json.str(o, "cancelled_on", null)),
                    Json.str(o, "terminal_id", ""),
                    Json.str(o, "notes", ""));
        }

        private static LocalDate parseDate(String s) {
            return (s == null || s.isBlank()) ? null : LocalDate.parse(s);
        }
    }

    /** Metered usage for one account over one period. */
    public record UsageRecord(
            String accountId,
            LocalDate periodStart,
            LocalDate periodEnd,
            long bytesDown,
            long bytesUp) {

        /**
         * Billable volume in gigabytes.
         *
         * <p>A gigabyte here is 10^9 bytes, not 2^30. That is the convention
         * every ISP bills on, and the 7% difference between the two is large
         * enough to be worth stating rather than leaving to whoever reads the
         * code next.
         *
         * <p>Only the downlink counts, which is also convention; charging for
         * upload surprises people.
         */
        public double billableGb() {
            return bytesDown / 1_000_000_000.0;
        }

        public Map<String, Object> toJson() {
            var m = new java.util.LinkedHashMap<String, Object>();
            m.put("account_id", accountId);
            m.put("period_start", periodStart.toString());
            m.put("period_end", periodEnd.toString());
            m.put("bytes_down", bytesDown);
            m.put("bytes_up", bytesUp);
            return m;
        }

        public static UsageRecord fromJson(Map<String, Object> o) {
            return new UsageRecord(
                    Json.str(o, "account_id", ""),
                    LocalDate.parse(Json.str(o, "period_start", "1970-01-01")),
                    LocalDate.parse(Json.str(o, "period_end", "1970-01-01")),
                    Json.integer(o, "bytes_down", 0),
                    Json.integer(o, "bytes_up", 0));
        }
    }

    /** One line on an invoice. */
    public record InvoiceLine(String description, Money amount) {
        public Map<String, Object> toJson() {
            var m = new java.util.LinkedHashMap<String, Object>();
            m.put("description", description);
            m.put("amount", amount.toString());
            m.put("amount_cents", amount.cents());
            return m;
        }
    }

    /** An invoice for one account and one period. */
    public record Invoice(
            String id,
            String accountId,
            LocalDate periodStart,
            LocalDate periodEnd,
            List<InvoiceLine> lines,
            Money total,
            LocalDate issuedOn) {

        public Map<String, Object> toJson() {
            var m = new java.util.LinkedHashMap<String, Object>();
            m.put("id", id);
            m.put("account_id", accountId);
            m.put("period_start", periodStart.toString());
            m.put("period_end", periodEnd.toString());
            m.put("lines", lines.stream().map(InvoiceLine::toJson).toList());
            m.put("total", total.toString());
            m.put("total_cents", total.cents());
            m.put("issued_on", issuedOn.toString());
            return m;
        }

        public static Invoice fromJson(Map<String, Object> o) {
            var lines = new java.util.ArrayList<InvoiceLine>();
            for (Object raw : Json.arr(o, "lines")) {
                if (raw instanceof Map<?, ?> lm) {
                    @SuppressWarnings("unchecked")
                    Map<String, Object> line = (Map<String, Object>) lm;
                    lines.add(new InvoiceLine(
                            Json.str(line, "description", ""),
                            Money.ofCents(Json.integer(line, "amount_cents", 0))));
                }
            }
            return new Invoice(
                    Json.str(o, "id", ""),
                    Json.str(o, "account_id", ""),
                    LocalDate.parse(Json.str(o, "period_start", "1970-01-01")),
                    LocalDate.parse(Json.str(o, "period_end", "1970-01-01")),
                    lines,
                    Money.ofCents(Json.integer(o, "total_cents", 0)),
                    LocalDate.parse(Json.str(o, "issued_on", "1970-01-01")));
        }
    }
}
