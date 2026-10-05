package net.bswisp.oss.util;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Minimal JSON reader and writer.
 *
 * <p>Hand-written so the OSS builds with nothing but a JDK. A billing system is
 * exactly the kind of long-lived service where a dependency tree becomes a
 * liability: it still has to run in five years, and RFC 8259 will not have moved.
 *
 * <p>Objects decode to {@link LinkedHashMap} so key order survives a round trip,
 * which keeps generated documents diffable.
 */
public final class Json {

    private Json() {}

    /** Thrown when input is not valid JSON. Carries the offset. */
    public static final class ParseException extends RuntimeException {
        private static final long serialVersionUID = 1L;
        public final int offset;

        ParseException(String message, int offset) {
            super(message + " at offset " + offset);
            this.offset = offset;
        }
    }

    /** Parses a complete JSON document. */
    public static Object parse(String text) {
        Parser p = new Parser(text);
        p.skipWhitespace();
        Object value = p.parseValue(0);
        p.skipWhitespace();
        if (p.pos != text.length()) {
            throw new ParseException("trailing content after document", p.pos);
        }
        return value;
    }

    /** Parses a document expected to be an object. */
    @SuppressWarnings("unchecked")
    public static Map<String, Object> parseObject(String text) {
        Object v = parse(text);
        if (!(v instanceof Map)) {
            throw new ParseException("expected a JSON object at the top level", 0);
        }
        return (Map<String, Object>) v;
    }

    /** Serialises a value. Pass indent &gt; 0 to pretty-print. */
    public static String write(Object value, int indent) {
        StringBuilder sb = new StringBuilder(256);
        writeInto(sb, value, indent, 0);
        return sb.toString();
    }

    public static String write(Object value) {
        return write(value, 0);
    }

    // ---- typed accessors -------------------------------------------------
    //
    // These return defaults rather than throwing on a missing key. A field a
    // peer has not sent is the normal case under the additive-change rule in
    // schema/README.md, not an error.

    @SuppressWarnings("unchecked")
    public static Map<String, Object> obj(Object o, String key) {
        if (o instanceof Map<?, ?> m && m.get(key) instanceof Map) {
            return (Map<String, Object>) m.get(key);
        }
        return new LinkedHashMap<>();
    }

    @SuppressWarnings("unchecked")
    public static List<Object> arr(Object o, String key) {
        if (o instanceof Map<?, ?> m && m.get(key) instanceof List) {
            return (List<Object>) m.get(key);
        }
        return List.of();
    }

    public static String str(Object o, String key, String fallback) {
        if (o instanceof Map<?, ?> m && m.get(key) instanceof String s) {
            return s;
        }
        return fallback;
    }

    public static double num(Object o, String key, double fallback) {
        if (o instanceof Map<?, ?> m && m.get(key) instanceof Number n) {
            return n.doubleValue();
        }
        return fallback;
    }

    public static long integer(Object o, String key, long fallback) {
        if (o instanceof Map<?, ?> m && m.get(key) instanceof Number n) {
            return n.longValue();
        }
        return fallback;
    }

    public static boolean bool(Object o, String key, boolean fallback) {
        if (o instanceof Map<?, ?> m && m.get(key) instanceof Boolean b) {
            return b;
        }
        return fallback;
    }

    // ---- writer ----------------------------------------------------------

    private static void writeInto(StringBuilder sb, Object value, int indent, int depth) {
        if (value == null) {
            sb.append("null");
        } else if (value instanceof String s) {
            escapeInto(sb, s);
        } else if (value instanceof Boolean b) {
            sb.append(b ? "true" : "false");
        } else if (value instanceof Number n) {
            writeNumber(sb, n);
        } else if (value instanceof Map<?, ?> m) {
            writeObject(sb, m, indent, depth);
        } else if (value instanceof Iterable<?> it) {
            writeArray(sb, it, indent, depth);
        } else {
            // Anything else is a programming slip. Emitting it as a string keeps
            // the document valid and makes the slip visible in the output rather
            // than throwing from inside a response writer.
            escapeInto(sb, String.valueOf(value));
        }
    }

    private static void writeNumber(StringBuilder sb, Number n) {
        if (n instanceof Double || n instanceof Float) {
            double d = n.doubleValue();
            if (Double.isNaN(d) || Double.isInfinite(d)) {
                // JSON has no NaN, and a silent 0 would be read as a real
                // measurement.
                sb.append("null");
            } else if (d == Math.floor(d) && Math.abs(d) < 1e15) {
                sb.append((long) d);
            } else {
                sb.append(d);
            }
        } else {
            sb.append(n);
        }
    }

    private static void writeObject(StringBuilder sb, Map<?, ?> m, int indent, int depth) {
        if (m.isEmpty()) {
            sb.append("{}");
            return;
        }
        sb.append('{');
        boolean first = true;
        for (Map.Entry<?, ?> e : m.entrySet()) {
            if (!first) sb.append(',');
            first = false;
            newline(sb, indent, depth + 1);
            escapeInto(sb, String.valueOf(e.getKey()));
            sb.append(':');
            if (indent > 0) sb.append(' ');
            writeInto(sb, e.getValue(), indent, depth + 1);
        }
        newline(sb, indent, depth);
        sb.append('}');
    }

    private static void writeArray(StringBuilder sb, Iterable<?> it, int indent, int depth) {
        var iterator = it.iterator();
        if (!iterator.hasNext()) {
            sb.append("[]");
            return;
        }
        sb.append('[');
        boolean first = true;
        while (iterator.hasNext()) {
            if (!first) sb.append(',');
            first = false;
            newline(sb, indent, depth + 1);
            writeInto(sb, iterator.next(), indent, depth + 1);
        }
        newline(sb, indent, depth);
        sb.append(']');
    }

    private static void newline(StringBuilder sb, int indent, int depth) {
        if (indent <= 0) return;
        sb.append('\n');
        sb.append(" ".repeat(indent * depth));
    }

    private static void escapeInto(StringBuilder sb, String s) {
        sb.append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"' -> sb.append("\\\"");
                case '\\' -> sb.append("\\\\");
                case '\b' -> sb.append("\\b");
                case '\f' -> sb.append("\\f");
                case '\n' -> sb.append("\\n");
                case '\r' -> sb.append("\\r");
                case '\t' -> sb.append("\\t");
                default -> {
                    if (c < 0x20) {
                        sb.append(String.format("\\u%04x", (int) c));
                    } else {
                        sb.append(c);
                    }
                }
            }
        }
        sb.append('"');
    }

    // ---- parser ----------------------------------------------------------

    private static final class Parser {
        /** Bounds nesting so a corrupt payload cannot exhaust the stack. */
        private static final int MAX_DEPTH = 200;

        private final String s;
        private int pos;

        Parser(String s) {
            this.s = s;
        }

        Object parseValue(int depth) {
            if (depth > MAX_DEPTH) throw new ParseException("nesting too deep", pos);
            if (pos >= s.length()) throw new ParseException("unexpected end of input", pos);

            return switch (s.charAt(pos)) {
                case '{' -> parseObjectValue(depth);
                case '[' -> parseArrayValue(depth);
                case '"' -> parseString();
                case 't' -> { expect("true"); yield Boolean.TRUE; }
                case 'f' -> { expect("false"); yield Boolean.FALSE; }
                case 'n' -> { expect("null"); yield null; }
                default -> parseNumber();
            };
        }

        Map<String, Object> parseObjectValue(int depth) {
            pos++;
            Map<String, Object> out = new LinkedHashMap<>();
            skipWhitespace();
            if (peek() == '}') { pos++; return out; }

            while (true) {
                skipWhitespace();
                if (peek() != '"') throw new ParseException("expected an object key", pos);
                String key = parseString();
                skipWhitespace();
                if (peek() != ':') throw new ParseException("expected ':' after key", pos);
                pos++;
                skipWhitespace();
                out.put(key, parseValue(depth + 1));
                skipWhitespace();
                char c = peek();
                if (c == ',') { pos++; continue; }
                if (c == '}') { pos++; return out; }
                throw new ParseException("expected ',' or '}'", pos);
            }
        }

        List<Object> parseArrayValue(int depth) {
            pos++;
            List<Object> out = new ArrayList<>();
            skipWhitespace();
            if (peek() == ']') { pos++; return out; }

            while (true) {
                skipWhitespace();
                out.add(parseValue(depth + 1));
                skipWhitespace();
                char c = peek();
                if (c == ',') { pos++; continue; }
                if (c == ']') { pos++; return out; }
                throw new ParseException("expected ',' or ']'", pos);
            }
        }

        String parseString() {
            pos++;
            StringBuilder sb = new StringBuilder();
            while (true) {
                if (pos >= s.length()) throw new ParseException("unterminated string", pos);
                char c = s.charAt(pos);
                if (c == '"') { pos++; return sb.toString(); }
                if (c != '\\') {
                    if (c < 0x20) {
                        throw new ParseException("unescaped control character", pos);
                    }
                    sb.append(c);
                    pos++;
                    continue;
                }
                pos++;
                if (pos >= s.length()) throw new ParseException("unterminated escape", pos);
                char esc = s.charAt(pos++);
                switch (esc) {
                    case '"' -> sb.append('"');
                    case '\\' -> sb.append('\\');
                    case '/' -> sb.append('/');
                    case 'b' -> sb.append('\b');
                    case 'f' -> sb.append('\f');
                    case 'n' -> sb.append('\n');
                    case 'r' -> sb.append('\r');
                    case 't' -> sb.append('\t');
                    case 'u' -> {
                        if (pos + 4 > s.length()) {
                            throw new ParseException("truncated \\u escape", pos);
                        }
                        // Java strings are UTF-16, so a surrogate pair in the
                        // input is already in the right encoding once appended.
                        sb.append((char) Integer.parseInt(s.substring(pos, pos + 4), 16));
                        pos += 4;
                    }
                    default -> throw new ParseException("unknown escape sequence", pos - 1);
                }
            }
        }

        Number parseNumber() {
            int start = pos;
            if (peek() == '-') pos++;
            while (pos < s.length() && isNumberChar(s.charAt(pos))) pos++;
            if (pos == start) throw new ParseException("expected a value", pos);

            String text = s.substring(start, pos);
            try {
                // Keep integers as longs. Money is counted in whole cents, and
                // routing those through a double is how rounding errors enter a
                // billing system.
                if (text.indexOf('.') < 0 && text.indexOf('e') < 0 && text.indexOf('E') < 0) {
                    return Long.parseLong(text);
                }
                return Double.parseDouble(text);
            } catch (NumberFormatException e) {
                throw new ParseException("invalid number '" + text + "'", start);
            }
        }

        private static boolean isNumberChar(char c) {
            return (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E'
                    || c == '+' || c == '-';
        }

        void expect(String literal) {
            if (!s.startsWith(literal, pos)) throw new ParseException("invalid literal", pos);
            pos += literal.length();
        }

        void skipWhitespace() {
            while (pos < s.length()) {
                char c = s.charAt(pos);
                if (c == ' ' || c == '\t' || c == '\n' || c == '\r') pos++;
                else break;
            }
        }

        char peek() {
            return pos < s.length() ? s.charAt(pos) : '\0';
        }
    }
}
