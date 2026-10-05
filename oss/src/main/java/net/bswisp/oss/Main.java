package net.bswisp.oss;

import java.nio.file.Path;
import java.time.LocalDate;
import java.util.Optional;

import net.bswisp.oss.model.Model.Account;
import net.bswisp.oss.model.Model.Invoice;
import net.bswisp.oss.model.Model.ServicePlan;
import net.bswisp.oss.model.Model.Status;

/**
 * The OSS/BSS entry point: subscriber lifecycle, billing, and provisioning the
 * control plane.
 */
public final class Main {

    private static final String USAGE = """
            bswisp-oss -- operations and billing for a fixed wireless network

            usage:
              bswisp-oss serve      [options]   run the HTTP service
              bswisp-oss seed       [options]   write starter plans and accounts
              bswisp-oss bill       [options]   run billing for a period and print invoices
              bswisp-oss provision  [options]   push the book of business to the control plane

            options:
              --store FILE          data file (default: var/oss.json)
              --listen HOST:PORT    serve address (default: 127.0.0.1:8090)
              --control-plane URL   basestationd base URL (default: http://127.0.0.1:8080)
              --period YYYY-MM      billing period for `bill` (default: last month)

            environment:
              OSS_TOKEN             bearer token for this service's own API
              CONTROL_PLANE_TOKEN   admin token for the control plane
            """;

    public static void main(String[] args) {
        try {
            System.exit(run(args));
        } catch (Exception e) {
            System.err.println("bswisp-oss: " + e.getMessage());
            System.exit(1);
        }
    }

    private static int run(String[] args) throws Exception {
        if (args.length == 0 || args[0].equals("-h") || args[0].equals("--help")) {
            System.out.print(USAGE);
            return args.length == 0 ? 1 : 0;
        }

        String command = args[0];
        String store = "var/oss.json";
        String listen = "127.0.0.1:8090";
        String controlPlane = "http://127.0.0.1:8080";
        String period = null;

        for (int i = 1; i < args.length; i++) {
            switch (args[i]) {
                case "--store" -> store = require(args, ++i, "--store");
                case "--listen" -> listen = require(args, ++i, "--listen");
                case "--control-plane" -> controlPlane = require(args, ++i, "--control-plane");
                case "--period" -> period = require(args, ++i, "--period");
                default -> {
                    System.err.println("unrecognised option: " + args[i]);
                    return 1;
                }
            }
        }

        Repository repo = new Repository(Path.of(store));
        repo.load();

        String ossToken = System.getenv("OSS_TOKEN");
        String cpToken = Optional.ofNullable(System.getenv("CONTROL_PLANE_TOKEN")).orElse("");
        Provisioning provisioning = cpToken.isBlank()
                ? null
                : new Provisioning(controlPlane, cpToken);

        return switch (command) {
            case "serve" -> serve(repo, provisioning, ossToken, listen);
            case "seed" -> seed(repo, store);
            case "bill" -> bill(repo, period);
            case "provision" -> provision(repo, provisioning, controlPlane);
            default -> {
                System.err.println("unknown command: " + command);
                System.out.print(USAGE);
                yield 1;
            }
        };
    }

    private static int serve(Repository repo, Provisioning provisioning,
            String token, String listen) throws Exception {
        String[] parts = listen.split(":");
        if (parts.length != 2) {
            System.err.println("--listen must be HOST:PORT");
            return 1;
        }
        Server server = new Server(repo, provisioning, token, parts[0],
                Integer.parseInt(parts[1]));
        server.start();

        System.out.printf("bswisp-oss listening on %s (%d accounts, %d plans)%n",
                listen, repo.accounts().size(), repo.plans().size());
        if (token == null || token.isBlank()) {
            System.out.println("  OSS_TOKEN is unset, so the API routes are disabled.");
        }
        if (provisioning == null) {
            System.out.println("  CONTROL_PLANE_TOKEN is unset, so provisioning is disabled.");
        }

        Runtime.getRuntime().addShutdownHook(new Thread(server::stop));
        Thread.currentThread().join();
        return 0;
    }

    private static int seed(Repository repo, String store) throws Exception {
        if (!repo.accounts().isEmpty()) {
            System.err.printf("%s already holds %d accounts; refusing to overwrite%n",
                    store, repo.accounts().size());
            return 1;
        }
        repo.seed();

        LocalDate installed = LocalDate.now().withDayOfMonth(1);
        record Seed(String id, String name, String plan, double lat, double lon, String cpe) {}
        var seeds = new Seed[] {
            new Seed("sub-0001", "A. Okonkwo", "residential-150", 44.95183, -93.09715, "cpe-0001"),
            new Seed("sub-0002", "B. Lindqvist", "residential-50", 44.96233, -93.11067, "cpe-0002"),
            new Seed("sub-0003", "C. Herrera", "business-200", 44.92507, -93.10179, "cpe-0003"),
            new Seed("sub-0004", "D. Nakamura", "residential-150", 44.98267, -93.06539, "cpe-0004"),
            new Seed("sub-0005", "E. Balogun", "residential-50", 44.97446, -93.05604, "cpe-0005"),
        };
        for (Seed s : seeds) {
            repo.putAccount(new Account(s.id(), s.name(),
                    s.id() + "@example.invalid", s.plan(), Status.ACTIVE,
                    "", s.lat(), s.lon(), 6.0, installed, null, s.cpe(), ""));
        }
        repo.save();

        System.out.printf("Seeded %s with %d plans and %d accounts%n",
                store, repo.plans().size(), repo.accounts().size());
        return 0;
    }

    private static int bill(Repository repo, String period) throws Exception {
        LocalDate start;
        if (period == null) {
            start = LocalDate.now().withDayOfMonth(1).minusMonths(1);
        } else {
            String[] parts = period.split("-");
            if (parts.length != 2) {
                System.err.println("--period must be YYYY-MM");
                return 1;
            }
            start = LocalDate.of(Integer.parseInt(parts[0]), Integer.parseInt(parts[1]), 1);
        }
        LocalDate end = start.withDayOfMonth(start.lengthOfMonth());

        System.out.printf("Billing %s to %s%n%n", start, end);
        long totalCents = 0;
        int issued = 0;
        int duplicate = 0;
        int skipped = 0;

        for (Account account : repo.accounts()) {
            Optional<ServicePlan> plan = repo.plan(account.planName());
            if (plan.isEmpty()) {
                skipped++;
                continue;
            }
            Billing.Outcome outcome = Billing.invoiceFor(account, plan.get(), start, end,
                    repo.usageFor(account.id(), start, end), LocalDate.now());
            if (!outcome.billed()) {
                System.out.printf("  %-10s %-16s not billed: %s%n",
                        account.id(), account.planName(), outcome.reason());
                skipped++;
                continue;
            }
            Invoice invoice = outcome.invoice().orElseThrow();
            if (!repo.addInvoiceIfAbsent(invoice)) {
                System.out.printf("  %-10s already invoiced for this period%n", account.id());
                duplicate++;
                continue;
            }
            issued++;
            totalCents += invoice.total().cents();
            System.out.printf("  %-10s %-16s %8s%n",
                    account.id(), account.planName(), invoice.total());
            for (var line : invoice.lines()) {
                System.out.printf("             %-50s %8s%n",
                        truncate(line.description(), 50), line.amount());
            }
        }
        repo.save();

        System.out.printf("%n  %d issued, %d already invoiced, %d not billed%n",
                issued, duplicate, skipped);
        System.out.printf("  total %s%n", net.bswisp.oss.util.Money.ofCents(totalCents));
        return 0;
    }

    private static int provision(Repository repo, Provisioning provisioning,
            String controlPlane) throws Exception {
        if (provisioning == null) {
            System.err.println("CONTROL_PLANE_TOKEN is unset; cannot provision");
            return 1;
        }
        var result = provisioning.push(repo);
        System.out.printf("Pushed %d subscribers and %d terminals to %s (%d skipped)%n",
                result.subscribers(), result.terminals(), controlPlane, result.skipped());
        return 0;
    }

    private static String require(String[] args, int index, String flag) {
        if (index >= args.length) {
            throw new IllegalArgumentException(flag + " needs a value");
        }
        return args[index];
    }

    private static String truncate(String s, int max) {
        return s.length() <= max ? s : s.substring(0, max - 1) + "…";
    }
}
