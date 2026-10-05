package net.bswisp.oss;

import java.nio.file.Files;
import java.nio.file.Path;
import java.time.LocalDate;
import java.util.List;
import java.util.Map;
import java.util.Optional;

import net.bswisp.oss.model.Model.Account;
import net.bswisp.oss.model.Model.Invoice;
import net.bswisp.oss.model.Model.ServicePlan;
import net.bswisp.oss.model.Model.Status;
import net.bswisp.oss.model.Model.UsageRecord;
import net.bswisp.oss.util.Json;
import net.bswisp.oss.util.Money;

/**
 * Tests, run as a plain main so the build needs no test framework on the
 * classpath.
 *
 * <p>Weighted towards billing, because billing errors are the ones a customer
 * notices and can dispute.
 */
public final class Tests {

    private static int failures = 0;
    private static String current = "";

    public static void main(String[] args) {
        moneyIsExact();
        moneyProrationRounds();
        moneyFormats();

        jsonRoundTrips();
        jsonKeepsIntegersIntegral();
        jsonRejectsGarbage();

        fullMonthIsFullPrice();
        midMonthActivationIsProrated();
        midMonthCancellationIsProrated();
        pendingAccountIsNotBilled();
        cancelledBeforePeriodIsNotBilled();
        suspendedAccountIsStillBilled();
        overageIsChargedOnlyAboveTheCap();
        unmeteredPlanNeverChargesOverage();
        activatedAfterPeriodExplainsItself();
        invoiceIdsAreDeterministic();
        repositoryRefusesDuplicateInvoices();

        repositoryRoundTrips();

        if (failures == 0) {
            System.out.println("all OSS tests passed");
            return;
        }
        System.err.printf("%d check(s) failed%n", failures);
        System.exit(1);
    }

    // ---- money -----------------------------------------------------------

    static void moneyIsExact() {
        test("money addition is exact where doubles are not");
        // Ten lots of 0.10 is exactly 1.00. The same sum in double arithmetic
        // lands on 0.9999999999999999, and a billing total that is a cent out is
        // a dispute.
        Money sum = Money.ZERO;
        for (int i = 0; i < 10; i++) {
            sum = sum.plus(Money.parse("0.10"));
        }
        check(sum.equals(Money.parse("1.00")), "0.10 x 10 = " + sum);
        check(sum.cents() == 100, "cents = " + sum.cents());

        double naive = 0.0;
        for (int i = 0; i < 10; i++) {
            naive += 0.10;
        }
        check(naive != 1.0, "double arithmetic really is inexact here, so the test means something");
    }

    static void moneyProrationRounds() {
        test("proration rounds half up and states its denominator");
        Money monthly = Money.parse("49.99");
        // 11 days of a 30 day month: 4999 * 11 / 30 = 1832.96 -> 1833 cents.
        check(monthly.prorate(11, 30).equals(Money.ofCents(1833)),
                "11/30 of 49.99 = " + monthly.prorate(11, 30));
        check(monthly.prorate(30, 30).equals(monthly), "full period is full price");
        check(monthly.prorate(0, 30).isZero(), "no days is nothing");

        boolean threw = false;
        try {
            monthly.prorate(1, 0);
        } catch (IllegalArgumentException e) {
            threw = true;
        }
        check(threw, "a zero denominator is rejected rather than producing infinity");
    }

    static void moneyFormats() {
        test("money renders as a plain decimal");
        check(Money.parse("49.99").toString().equals("49.99"), Money.parse("49.99").toString());
        check(Money.parse("100").toString().equals("100.00"), Money.parse("100").toString());
        check(Money.ofCents(5).toString().equals("0.05"), Money.ofCents(5).toString());
        check(Money.ofCents(-250).toString().equals("-2.50"), Money.ofCents(-250).toString());
        check(Money.ofCents(-5).toString().equals("-0.05"), Money.ofCents(-5).toString());
    }

    // ---- json ------------------------------------------------------------

    static void jsonRoundTrips() {
        test("JSON survives a write and read cycle");
        String src = """
                {"a":1,"b":[1,2,3],"c":{"d":"text with \\"quotes\\" and \\n"},"e":true,"f":null}
                """;
        Object first = Json.parse(src);
        String written = Json.write(first);
        check(Json.write(Json.parse(written)).equals(written), "stable across a second cycle");
        check(Json.str(Json.obj(first, "c"), "d", "").contains("\"quotes\""), "escapes decode");
    }

    static void jsonKeepsIntegersIntegral() {
        test("integers stay integers rather than becoming doubles");
        Map<String, Object> o = Json.parseObject("{\"cents\":123456789012}");
        check(o.get("cents") instanceof Long, "parsed as " + o.get("cents").getClass().getSimpleName());
        check(Json.integer(o, "cents", 0) == 123456789012L, "value survives");
    }

    static void jsonRejectsGarbage() {
        test("malformed JSON is rejected");
        check(throwsParse("{\"a\": }"), "missing value");
        check(throwsParse("[1,2,3]trailing"), "trailing content");
        check(throwsParse("{\"unterminated\": \"oops"), "unterminated string");
    }

    static boolean throwsParse(String s) {
        try {
            Json.parse(s);
            return false;
        } catch (Json.ParseException e) {
            return true;
        }
    }

    // ---- billing ---------------------------------------------------------

    static ServicePlan plan() {
        return new ServicePlan("residential-50", Money.parse("49.99"),
                50, 10, 10, 2, 1, 75, 20, 0, Money.ZERO);
    }

    static ServicePlan cappedPlan() {
        return new ServicePlan("residential-150", Money.parse("79.99"),
                150, 25, 25, 5, 2, 200, 20, 1000, Money.parse("2.00"));
    }

    static Account account(Status status, LocalDate activated, LocalDate cancelled) {
        return new Account("sub-1", "Test", "t@example.invalid", "residential-50",
                status, "", 0, 0, 6, activated, cancelled, "cpe-1", "");
    }

    static final LocalDate START = LocalDate.of(2026, 6, 1);
    static final LocalDate END = LocalDate.of(2026, 6, 30);

    static void fullMonthIsFullPrice() {
        test("a full month in service is the full price");
        Invoice i = Billing.invoiceFor(
                account(Status.ACTIVE, LocalDate.of(2026, 1, 1), null),
                plan(), START, END, Optional.empty(), END).invoice().orElseThrow();
        check(i.total().equals(Money.parse("49.99")), "total = " + i.total());
        check(i.lines().size() == 1, "one line");
    }

    static void midMonthActivationIsProrated() {
        test("activation partway through the month is prorated");
        // Activated on the 20th of a 30 day month: the 20th to the 30th
        // inclusive is 11 days.
        Invoice i = Billing.invoiceFor(
                account(Status.ACTIVE, LocalDate.of(2026, 6, 20), null),
                plan(), START, END, Optional.empty(), END).invoice().orElseThrow();
        check(i.total().equals(Money.ofCents(1833)), "total = " + i.total());
        check(i.lines().get(0).description().contains("11 of 30 days"),
                i.lines().get(0).description());
    }

    static void midMonthCancellationIsProrated() {
        test("cancellation partway through the month is prorated");
        // In service the 1st to the 10th inclusive is 10 days: 4999*10/30 = 1666.3 -> 1666.
        Invoice i = Billing.invoiceFor(
                account(Status.ACTIVE, LocalDate.of(2026, 1, 1), LocalDate.of(2026, 6, 10)),
                plan(), START, END, Optional.empty(), END).invoice().orElseThrow();
        check(i.total().equals(Money.ofCents(1666)), "total = " + i.total());
    }

    static void pendingAccountIsNotBilled() {
        test("a sold but uninstalled account is not billed");
        var outcome = Billing.invoiceFor(account(Status.PENDING, null, null),
                plan(), START, END, Optional.empty(), END);
        check(!outcome.billed(), "PENDING produced an invoice");
        check(outcome.reason().contains("not yet installed"),
                "reason should explain why: " + outcome.reason());
    }

    static void cancelledBeforePeriodIsNotBilled() {
        test("an account cancelled before the period is not billed");
        var outcome = Billing.invoiceFor(
                account(Status.CANCELLED, LocalDate.of(2026, 1, 1), LocalDate.of(2026, 5, 15)),
                plan(), START, END, Optional.empty(), END);
        check(!outcome.billed(), "a cancelled account produced an invoice");
        check(outcome.reason().contains("cancelled"),
                "reason should explain why: " + outcome.reason());
    }

    static void suspendedAccountIsStillBilled() {
        test("a suspended account still holds its slot, so it is still billed");
        // Suspension is usually non-payment. Stopping the invoices at that point
        // means the arrears stop growing, which is not what suspension is for.
        check(Billing.invoiceFor(
                account(Status.SUSPENDED, LocalDate.of(2026, 1, 1), null),
                plan(), START, END, Optional.empty(), END).billed(),
                "SUSPENDED produced no invoice");
    }

    static void overageIsChargedOnlyAboveTheCap() {
        test("overage is charged on the excess only, in whole gigabytes");
        Account a = new Account("sub-2", "Heavy", "h@example.invalid", "residential-150",
                Status.ACTIVE, "", 0, 0, 6, LocalDate.of(2026, 1, 1), null, "cpe-2", "");

        // 1250.5 GB against a 1000 GB cap: 250 whole GB over at 2.00 = 500.00.
        UsageRecord heavy = new UsageRecord("sub-2", START, END, 1_250_500_000_000L, 0);
        Invoice i = Billing.invoiceFor(a, cappedPlan(), START, END, Optional.of(heavy), END)
                .invoice().orElseThrow();
        check(i.lines().size() == 2, "expected a service line and an overage line, got "
                + i.lines().size());
        check(i.total().equals(Money.parse("79.99").plus(Money.parse("500.00"))),
                "total = " + i.total());

        // Exactly at the cap is not over it.
        UsageRecord atCap = new UsageRecord("sub-2", START, END, 1_000_000_000_000L, 0);
        Invoice j = Billing.invoiceFor(a, cappedPlan(), START, END, Optional.of(atCap), END)
                .invoice().orElseThrow();
        check(j.lines().size() == 1, "at the cap should not generate an overage line");
    }

    static void unmeteredPlanNeverChargesOverage() {
        test("an unmetered plan never charges overage however much is used");
        UsageRecord enormous = new UsageRecord("sub-1", START, END, 9_000_000_000_000L, 0);
        Invoice i = Billing.invoiceFor(
                account(Status.ACTIVE, LocalDate.of(2026, 1, 1), null),
                plan(), START, END, Optional.of(enormous), END).invoice().orElseThrow();
        check(i.lines().size() == 1, "unmetered plan produced " + i.lines().size() + " lines");
        check(i.total().equals(Money.parse("49.99")), "total = " + i.total());
    }

    static void activatedAfterPeriodExplainsItself() {
        test("an account installed after the period says so, rather than just failing");
        var outcome = Billing.invoiceFor(
                account(Status.ACTIVE, LocalDate.of(2026, 10, 1), null),
                plan(), START, END, Optional.empty(), END);
        check(!outcome.billed(), "should not bill for a period before install");
        check(outcome.reason().contains("after this period"),
                "reason should name the cause: " + outcome.reason());
    }

    static void invoiceIdsAreDeterministic() {
        test("invoice ids are derived, so a retried run collides instead of double charging");
        String a = Billing.invoiceId("sub-1", START);
        String b = Billing.invoiceId("sub-1", START);
        check(a.equals(b), a + " != " + b);
        check(!a.equals(Billing.invoiceId("sub-1", START.plusMonths(1))),
                "different periods must differ");
        check(!a.equals(Billing.invoiceId("sub-2", START)),
                "different accounts must differ");
    }

    static void repositoryRefusesDuplicateInvoices() {
        test("the repository refuses a second invoice for the same period");
        Repository repo = new Repository(Path.of("/tmp/bswisp-oss-test-ignored.json"));
        Invoice i = new Invoice(Billing.invoiceId("sub-1", START), "sub-1", START, END,
                List.of(), Money.parse("49.99"), END);
        check(repo.addInvoiceIfAbsent(i), "first insert should succeed");
        check(!repo.addInvoiceIfAbsent(i), "second insert should be refused");
        check(repo.invoices().size() == 1, "only one invoice stored");
    }

    static void repositoryRoundTrips() {
        test("the repository survives a save and load");
        try {
            Path tmp = Files.createTempFile("bswisp-oss-test", ".json");
            Files.deleteIfExists(tmp);

            Repository write = new Repository(tmp);
            write.seed();
            write.putAccount(account(Status.ACTIVE, LocalDate.of(2026, 3, 4), null));
            write.addUsage(new UsageRecord("sub-1", START, END, 123_456_789L, 42L));
            write.addInvoiceIfAbsent(new Invoice("inv-x", "sub-1", START, END,
                    List.of(new net.bswisp.oss.model.Model.InvoiceLine("Service",
                            Money.parse("49.99"))),
                    Money.parse("49.99"), END));
            write.save();

            Repository read = new Repository(tmp);
            read.load();

            check(read.plans().size() == write.plans().size(), "plans survived");
            check(read.accounts().size() == 1, "accounts survived");
            check(read.account("sub-1").orElseThrow().activatedOn()
                    .equals(LocalDate.of(2026, 3, 4)), "dates survived");
            check(read.usage().size() == 1, "usage survived");
            check(read.usage().get(0).bytesDown() == 123_456_789L,
                    "byte counters survived exactly");
            check(read.invoices().size() == 1, "invoices survived");
            check(read.invoices().get(0).total().equals(Money.parse("49.99")),
                    "money survived exactly: " + read.invoices().get(0).total());

            Files.deleteIfExists(tmp);
        } catch (Exception e) {
            check(false, "threw " + e);
        }
    }

    // ---- harness ---------------------------------------------------------

    static void test(String name) {
        current = name;
    }

    static void check(boolean condition, String detail) {
        if (!condition) {
            System.err.printf("FAIL [%s]: %s%n", current, detail);
            failures++;
        }
    }
}
