package net.bswisp.oss;

import java.time.LocalDate;
import java.time.temporal.ChronoUnit;
import java.util.ArrayList;
import java.util.List;
import java.util.Optional;

import net.bswisp.oss.model.Model.Account;
import net.bswisp.oss.model.Model.Invoice;
import net.bswisp.oss.model.Model.InvoiceLine;
import net.bswisp.oss.model.Model.ServicePlan;
import net.bswisp.oss.model.Model.Status;
import net.bswisp.oss.model.Model.UsageRecord;
import net.bswisp.oss.util.Money;

/**
 * Turns accounts and usage into invoices.
 *
 * <p>Three rules do most of the work, and each exists because getting it wrong
 * produces a bill a customer can dispute:
 *
 * <ol>
 *   <li>Charge only for days the account was actually in service. An account
 *       activated on the 20th owes eleven thirtieths of a month, not a month.
 *   <li>Never bill a PENDING account. It has been sold but not installed.
 *   <li>Invoice generation is idempotent per account and period. A billing run
 *       that is retried after a crash must not charge twice, and billing runs do
 *       get retried.
 * </ol>
 */
public final class Billing {

    private Billing() {}

    /**
     * The result of trying to bill one account for one period.
     *
     * <p>Carries a reason when there is no invoice. "Not billable" with no
     * explanation is indistinguishable from a bug when an operator reads it in a
     * billing run, and the reasons are genuinely different: an account installed
     * after the period, one cancelled before it, and one still awaiting install
     * all produce no invoice for entirely different causes.
     */
    public record Outcome(Optional<Invoice> invoice, String reason) {
        static Outcome billed(Invoice invoice) {
            return new Outcome(Optional.of(invoice), "");
        }

        static Outcome notBillable(String reason) {
            return new Outcome(Optional.empty(), reason);
        }

        public boolean billed() {
            return invoice.isPresent();
        }
    }

    /**
     * Builds an invoice, or explains why there is none.
     *
     * <p>No invoice means none on purpose: issuing a zero invoice to a customer
     * who was not in service generates a support call for no revenue.
     */
    public static Outcome invoiceFor(
            Account account,
            ServicePlan plan,
            LocalDate periodStart,
            LocalDate periodEnd,
            Optional<UsageRecord> usage,
            LocalDate issuedOn) {

        if (periodEnd.isBefore(periodStart)) {
            throw new IllegalArgumentException(
                    "period end " + periodEnd + " precedes start " + periodStart);
        }
        if (!account.status().billable()) {
            return Outcome.notBillable(switch (account.status()) {
                case PENDING -> "sold but not yet installed";
                case CANCELLED -> "cancelled on " + account.cancelledOn();
                default -> "status " + account.status() + " is not billable";
            });
        }

        // The window the account was genuinely in service, clipped to the period.
        LocalDate serviceFrom = account.activatedOn() == null
                ? null
                : max(account.activatedOn(), periodStart);
        if (serviceFrom == null) {
            return Outcome.notBillable("never activated");
        }
        if (account.activatedOn().isAfter(periodEnd)) {
            return Outcome.notBillable(
                    "activated " + account.activatedOn() + ", after this period ended");
        }
        LocalDate serviceTo = account.cancelledOn() == null
                ? periodEnd
                : min(account.cancelledOn(), periodEnd);
        if (serviceTo.isBefore(serviceFrom)) {
            return Outcome.notBillable(
                    "cancelled " + account.cancelledOn() + ", before this period began");
        }

        // Both endpoints inclusive: a customer in service on the 1st and the 30th
        // had thirty days of service, not twenty-nine.
        long periodDays = ChronoUnit.DAYS.between(periodStart, periodEnd) + 1;
        long serviceDays = ChronoUnit.DAYS.between(serviceFrom, serviceTo) + 1;

        List<InvoiceLine> lines = new ArrayList<>();

        if (serviceDays >= periodDays) {
            lines.add(new InvoiceLine(
                    "%s service, %s to %s".formatted(plan.name(), periodStart, periodEnd),
                    plan.monthlyPrice()));
        } else {
            lines.add(new InvoiceLine(
                    "%s service, %s to %s (%d of %d days)".formatted(
                            plan.name(), serviceFrom, serviceTo, serviceDays, periodDays),
                    plan.monthlyPrice().prorate(serviceDays, periodDays)));
        }

        // Overage, only against a cap, and only on the part above it.
        if (plan.dataCapGb() > 0 && usage.isPresent()) {
            double used = usage.get().billableGb();
            double over = used - plan.dataCapGb();
            if (over > 0 && !plan.overagePerGb().isZero()) {
                long wholeGbOver = (long) Math.floor(over);
                if (wholeGbOver > 0) {
                    lines.add(new InvoiceLine(
                            "Data overage, %.1f GB used against a %.0f GB allowance (%d GB billed)"
                                    .formatted(used, plan.dataCapGb(), wholeGbOver),
                            plan.overagePerGb().times(wholeGbOver)));
                }
            }
        }

        Money total = lines.stream().map(InvoiceLine::amount).reduce(Money.ZERO, Money::plus);
        if (total.isZero() && lines.size() <= 1) {
            return Outcome.notBillable("nothing to charge for this period");
        }

        return Outcome.billed(new Invoice(
                invoiceId(account.id(), periodStart),
                account.id(),
                periodStart,
                periodEnd,
                lines,
                total,
                issuedOn));
    }

    /**
     * A deterministic invoice id.
     *
     * <p>Derived from the account and period rather than from a counter or a
     * random value, so re-running a billing run produces the same id and the
     * duplicate is detectable instead of becoming a second charge.
     */
    public static String invoiceId(String accountId, LocalDate periodStart) {
        return "inv-%s-%s".formatted(periodStart.toString().replace("-", ""), accountId);
    }

    /** Suggests the status change a cap breach calls for, if any. */
    public static Optional<Status> capAction(ServicePlan plan, UsageRecord usage) {
        if (plan.dataCapGb() <= 0) {
            return Optional.empty();
        }
        // Only suggests; it never applies. Cutting a customer off is an operator
        // decision, and a billing library that can suspend accounts by itself is
        // one bad config away from suspending all of them.
        if (usage.billableGb() > plan.dataCapGb() && plan.overagePerGb().isZero()) {
            return Optional.of(Status.SUSPENDED);
        }
        return Optional.empty();
    }

    private static LocalDate max(LocalDate a, LocalDate b) {
        return a.isAfter(b) ? a : b;
    }

    private static LocalDate min(LocalDate a, LocalDate b) {
        return a.isBefore(b) ? a : b;
    }
}
