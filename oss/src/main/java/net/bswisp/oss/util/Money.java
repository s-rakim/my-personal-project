package net.bswisp.oss.util;

import java.math.BigDecimal;
import java.math.RoundingMode;
import java.util.Objects;

/**
 * An amount of money, counted in whole cents.
 *
 * <p>Never a {@code double}. 0.1 has no exact binary representation, so summing a
 * few hundred invoice lines in floating point produces a total that is off by a
 * cent, and a billing system that is off by a cent is one a customer can
 * legitimately dispute. Integer cents make addition exact and force every
 * division to state its rounding, which is where the real decisions are.
 *
 * <p>Immutable, and comparable so invoice lines sort predictably.
 */
public final class Money implements Comparable<Money> {

    public static final Money ZERO = new Money(0);

    private final long cents;

    private Money(long cents) {
        this.cents = cents;
    }

    public static Money ofCents(long cents) {
        return new Money(cents);
    }

    /**
     * Builds an amount from a decimal string such as {@code "49.99"}.
     *
     * <p>Takes a string rather than a double so a price from a config file or a
     * form never passes through binary floating point on its way in.
     */
    public static Money parse(String amount) {
        Objects.requireNonNull(amount, "amount");
        BigDecimal d = new BigDecimal(amount.trim());
        return new Money(d.movePointRight(2).setScale(0, RoundingMode.HALF_UP).longValueExact());
    }

    public long cents() {
        return cents;
    }

    public Money plus(Money other) {
        return new Money(Math.addExact(cents, other.cents));
    }

    public Money minus(Money other) {
        return new Money(Math.subtractExact(cents, other.cents));
    }

    public Money times(long factor) {
        return new Money(Math.multiplyExact(cents, factor));
    }

    /**
     * Scales by a fraction, rounding half up.
     *
     * <p>Used for proration. The rounding is explicit because somebody has to
     * absorb the half cent, and leaving that to a floating point cast means
     * nobody chose.
     */
    public Money prorate(long numerator, long denominator) {
        if (denominator == 0) {
            throw new IllegalArgumentException("cannot prorate by a zero denominator");
        }
        BigDecimal scaled = BigDecimal.valueOf(cents)
                .multiply(BigDecimal.valueOf(numerator))
                .divide(BigDecimal.valueOf(denominator), 0, RoundingMode.HALF_UP);
        return new Money(scaled.longValueExact());
    }

    public boolean isZero() {
        return cents == 0;
    }

    public boolean isNegative() {
        return cents < 0;
    }

    @Override
    public int compareTo(Money other) {
        return Long.compare(cents, other.cents);
    }

    @Override
    public boolean equals(Object o) {
        return o instanceof Money m && m.cents == cents;
    }

    @Override
    public int hashCode() {
        return Long.hashCode(cents);
    }

    /** Renders as a plain decimal, e.g. {@code 49.99} or {@code -5.00}. */
    @Override
    public String toString() {
        long whole = cents / 100;
        long part = Math.abs(cents % 100);
        String sign = (cents < 0 && whole == 0) ? "-" : "";
        return String.format("%s%d.%02d", sign, whole, part);
    }
}
